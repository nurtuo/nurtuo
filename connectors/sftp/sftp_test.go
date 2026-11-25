// SPDX-License-Identifier: Apache-2.0

package sftp_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/connectors/sftp"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/structpb"
)

const testPassword = "s3cret-pw"

// startServer runs an in-process SSH server with an in-memory SFTP subsystem and returns its
// address and host key fingerprint.
func startServer(t *testing.T) (addr, fingerprint string) {
	addr, fingerprint, _ = startServerH(t, nil)
	return addr, fingerprint
}

// startServerH is startServer with an optional wrapper around the SFTP handlers (fault injection)
// and returns the handlers.
func startServerH(t *testing.T, wrap func(pkgsftp.Handlers) pkgsftp.Handlers) (addr, fingerprint string, handlers pkgsftp.Handlers) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "alice" && string(pw) == testPassword {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	handlers = pkgsftp.InMemHandler() // shared by all connections: state persists across Execute calls
	if wrap != nil {
		handlers = wrap(handlers)
	}
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go serveConn(nc, cfg, handlers)
		}
	}()
	return l.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey()), handlers
}

func serveConn(nc net.Conn, cfg *ssh.ServerConfig, h pkgsftp.Handlers) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range creqs {
				ok := r.Type == "subsystem" && len(r.Payload) >= 8 && string(r.Payload[4:]) == "sftp"
				_ = r.Reply(ok, nil)
				if ok {
					go func() {
						srv := pkgsftp.NewRequestServer(ch, h)
						_ = srv.Serve()
						_ = ch.Close()
					}()
				}
			}
		}()
	}
}

func settings(t *testing.T, addr, fp, pw string) *connectorv1.ConnectionConfig {
	host, port, _ := net.SplitHostPort(addr)
	var p float64
	for _, c := range port {
		p = p*10 + float64(c-'0')
	}
	s, err := structpb.NewStruct(map[string]any{
		"host": host, "port": p, "user": "alice", "password": pw,
		"hostKeyPolicy": "fingerprint", "hostKeyFingerprint": fp,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &connectorv1.ConnectionConfig{Settings: s}
}

// discardLarge makes the in-memory SFTP server drop everything written to files whose name
// contains "large.bin" (and accept their rename), so that the contract suite's large-payload
// subtest measures the connector's heap and not the fixture's storage, which lives in the same
// process.
func discardLarge(h pkgsftp.Handlers) pkgsftp.Handlers {
	large := func(p string) bool { return strings.Contains(path.Base(p), "large.bin") }
	return pkgsftp.Handlers{
		FileGet:  h.FileGet,
		FileList: h.FileList,
		FilePut: putFunc(func(r *pkgsftp.Request) (io.WriterAt, error) {
			if large(r.Filepath) {
				return discardAt{}, nil
			}
			return h.FilePut.Filewrite(r)
		}),
		FileCmd: cmdFunc(func(r *pkgsftp.Request) error {
			if r.Method == "Rename" && large(r.Target) {
				return nil
			}
			return h.FileCmd.Filecmd(r)
		}),
	}
}

type putFunc func(*pkgsftp.Request) (io.WriterAt, error)

func (f putFunc) Filewrite(r *pkgsftp.Request) (io.WriterAt, error) { return f(r) }

type cmdFunc func(*pkgsftp.Request) error

func (f cmdFunc) Filecmd(r *pkgsftp.Request) error { return f(r) }

type discardAt struct{}

func (discardAt) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

func TestContract(t *testing.T) {
	addr, fp, _ := startServerH(t, discardLarge)
	conn := settings(t, addr, fp, testPassword)
	op := func(o connectorv1.OperationType, custom string) *connectorv1.ExecuteStart {
		return &connectorv1.ExecuteStart{OperationType: o, CustomType: custom}
	}
	ok := connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
	appErr := connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
	contracttest.Run(t, contracttest.Options{
		New:        func(testing.TB) connector.Connector { return sftp.New() },
		Connection: conn,
		Secrets:    []string{testPassword},
		// The upload is streamed to the remote file chunk by chunk; the suite checks that the heap
		// stays bounded while a payload larger than the 4 MiB gRPC message size goes through.
		LargePayload: &contracttest.LargePayloadCase{
			Start:      op(connectorv1.OperationType_OPERATION_TYPE_CREATE, ""),
			Properties: map[string]string{"path": "/in/large.bin"},
		},
		Execute: []contracttest.ExecuteCase{
			{
				Name:  "put",
				Start: op(connectorv1.OperationType_OPERATION_TYPE_CREATE, ""),
				Documents: []contracttest.Document{
					{ID: "a", Properties: map[string]string{"path": "/in/a.txt"}, Data: []byte("hello")},
					{ID: "big", Properties: map[string]string{"path": "/in/big.bin"}, Data: bytes.Repeat([]byte("x"), 1<<20+17)},
					{ID: "nopath", Data: []byte("x")},
				},
				Want: map[string]connectorv1.DocumentStatus{"a": ok, "big": ok, "nopath": appErr},
			},
			{
				Name:      "get",
				Start:     op(connectorv1.OperationType_OPERATION_TYPE_GET, ""),
				Documents: []contracttest.Document{{ID: "g1", Properties: map[string]string{"path": "/in/a.txt"}}, {ID: "g2", Properties: map[string]string{"path": "/missing"}}},
				Want:      map[string]connectorv1.DocumentStatus{"g1": ok, "g2": appErr},
			},
			{
				Name:      "list",
				Start:     op(connectorv1.OperationType_OPERATION_TYPE_QUERY, ""),
				Documents: []contracttest.Document{{ID: "l1", Properties: map[string]string{"path": "/in"}}},
				Want:      map[string]connectorv1.DocumentStatus{"l1": ok},
			},
			{
				Name:      "rename",
				Start:     op(connectorv1.OperationType_OPERATION_TYPE_EXECUTE, "RENAME"),
				Documents: []contracttest.Document{{ID: "r1", Properties: map[string]string{"path": "/in/a.txt", "newPath": "/in/b.txt"}}},
				Want:      map[string]connectorv1.DocumentStatus{"r1": ok},
			},
			{
				Name:  "delete",
				Start: op(connectorv1.OperationType_OPERATION_TYPE_DELETE, ""),
				Documents: []contracttest.Document{
					{ID: "d1", Properties: map[string]string{"path": "/in/b.txt"}},
					{ID: "d2", Properties: map[string]string{"path": "/in/b.txt"}},
				},
				Want: map[string]connectorv1.DocumentStatus{"d1": ok, "d2": appErr},
			},
		},
	})
}

// run executes one batch directly (in-process stream) and returns results and outputs.
func TestHostKeyAndTest(t *testing.T) {
	addr, fp := startServer(t)
	c := sftp.New()
	do := func(conn *connectorv1.ConnectionConfig) *connectorv1.TestResponse {
		r, err := c.Test(context.Background(), &connectorv1.TestRequest{Connection: conn})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := do(settings(t, addr, fp, testPassword)); !r.Ok {
		t.Fatalf("expected ok, got %q", r.Diagnostic)
	}
	r := do(settings(t, addr, fp, "wrong-password-xyz"))
	if r.Ok || strings.Contains(r.Diagnostic, "wrong-password-xyz") {
		t.Fatalf("bad password: ok=%v diag=%q", r.Ok, r.Diagnostic)
	}
	if r := do(settings(t, addr, "SHA256:bogus", testPassword)); r.Ok {
		t.Fatal("host key mismatch must fail")
	}
}

func TestRoundTripContent(t *testing.T) {
	addr, fp := startServer(t)
	conn := settings(t, addr, fp, testPassword)
	payload := make([]byte, 3<<20+5)
	_, _ = rand.Read(payload)

	put := run(t, conn, &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE}, "p", map[string]string{"path": "/f.bin"}, payload)
	if put.res.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS || put.res.Tracked["size"] != "3145733" {
		t.Fatalf("put: %v", put.res)
	}
	get := run(t, conn, &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_GET}, "g", map[string]string{"path": "/f.bin"}, nil)
	if !bytes.Equal(get.out, payload) || get.chunks < 2 {
		t.Fatalf("get mismatch: len=%d chunks=%d", len(get.out), get.chunks)
	}
	ls := run(t, conn, &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_QUERY}, "l", map[string]string{"path": "/"}, nil)
	var entries []map[string]any
	if err := json.Unmarshal(ls.out, &entries); err != nil || len(entries) != 1 || entries[0]["name"] != "f.bin" {
		t.Fatalf("list: %v %s", err, ls.out)
	}
	// path traversal is confined below the root
	esc := run(t, conn, &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_GET}, "e", map[string]string{"path": "../../etc/passwd"}, nil)
	if esc.res.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR {
		t.Fatalf("escape: %v", esc.res)
	}
}

type outcome struct {
	res    *connectorv1.DocumentResult
	out    []byte
	chunks int
}

// run drives Execute through the real gRPC server for a single document.
func run(t *testing.T, conn *connectorv1.ConnectionConfig, st *connectorv1.ExecuteStart, id string, props map[string]string, data []byte) outcome {
	t.Helper()
	cl := dialConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := cl.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.Connection = conn
	_ = s.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Start{Start: st}})
	const size = 256 * 1024
	for off := 0; ; off += size {
		end := min(off+size, len(data))
		ch := &connectorv1.DocumentChunk{DocumentId: id, Data: data[off:end], Last: end == len(data)}
		if off == 0 {
			ch.Properties = props
		}
		_ = s.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: ch}})
		if ch.Last {
			break
		}
	}
	_ = s.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}})
	_ = s.CloseSend()
	var o outcome
	for {
		r, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if x := r.GetResult(); x != nil {
			o.res = x
		}
		if x := r.GetOutput(); x != nil {
			o.out = append(o.out, x.Data...)
			o.chunks++
		}
	}
	if o.res == nil {
		t.Fatal("no result")
	}
	return o
}
