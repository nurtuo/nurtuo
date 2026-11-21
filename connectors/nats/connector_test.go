// SPDX-License-Identifier: Apache-2.0

package nats_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	natsconn "github.com/nurtuo/nurtuo/connectors/nats"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

const token = "s3cr3t-token"

func startServer(t testing.TB, withToken bool) *server.Server {
	t.Helper()
	opts := &server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true}
	if withToken {
		opts.Authorization = token
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server not ready")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	return s
}

func conn(t testing.TB, settings map[string]any) *connectorv1.ConnectionConfig {
	t.Helper()
	st, err := structpb.NewStruct(settings)
	if err != nil {
		t.Fatal(err)
	}
	return &connectorv1.ConnectionConfig{Settings: st}
}

func opts(t testing.TB, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	s := startServer(t, true)
	nc, err := nats.Connect(s.ClientURL(), nats.Token(token))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	// Responder for request/reply.
	sub, err := nc.Subscribe("svc.echo", func(m *nats.Msg) { _ = m.Respond(append([]byte("echo:"), m.Data...)) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	js, _ := nc.JetStream()
	if _, err := js.AddStream(&nats.StreamConfig{Name: "ORDERS", Subjects: []string{"orders.>"}}); err != nil {
		t.Fatal(err)
	}
	// Flush so the responder is registered before requests.
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	connection := conn(t, map[string]any{"url": s.ClientURL(), "token": token})
	subj := func(v string) map[string]string { return map[string]string{natsconn.PropSubject: v} }
	ok, appErr, fail := connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
		connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE
	_ = fail

	contracttest.Run(t, contracttest.Options{
		New:        func(testing.TB) connector.Connector { return natsconn.New() },
		Connection: connection,
		Secrets:    []string{token},
		Execute: []contracttest.ExecuteCase{
			{
				Name:  "publish",
				Start: &connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE},
				Documents: []contracttest.Document{
					{ID: "a", Properties: subj("events.a"), Data: []byte("hello")},
					{ID: "nosubject", Data: []byte("x")},
				},
				Want: map[string]connectorv1.DocumentStatus{"a": ok, "nosubject": appErr},
			},
			{
				Name: "request-reply",
				Start: &connectorv1.ExecuteStart{
					OperationType: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: natsconn.CustomRequest,
					Options: opts(t, map[string]any{"timeout_ms": 1000}),
				},
				Documents: []contracttest.Document{
					{ID: "r1", Properties: subj("svc.echo"), Data: []byte("ping")},
					{ID: "r2", Properties: subj("svc.nobody"), Data: []byte("ping")},
				},
				Want: map[string]connectorv1.DocumentStatus{"r1": ok, "r2": appErr},
			},
			{
				Name: "jetstream-publish",
				Start: &connectorv1.ExecuteStart{
					OperationType: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: natsconn.CustomJetStreamPub,
					Options: opts(t, map[string]any{"timeout_ms": 1000}),
				},
				Documents: []contracttest.Document{
					{ID: "j1", Properties: subj("orders.new"), Data: []byte("o1")},
					{ID: "j2", Properties: subj("nostream.x"), Data: []byte("o2")},
				},
				Want: map[string]connectorv1.DocumentStatus{"j1": ok, "j2": appErr},
			},
		},
	})
}

func TestTestConnection(t *testing.T) {
	s := startServer(t, true)
	c := natsconn.New()
	cases := []struct {
		name     string
		settings map[string]any
		wantOK   bool
	}{
		{"good token", map[string]any{"url": s.ClientURL(), "token": token}, true},
		{"bad token", map[string]any{"url": s.ClientURL(), "token": "wrong-token-value"}, false},
		{"no auth", map[string]any{"url": s.ClientURL()}, false},
		{"missing url", map[string]any{"token": "x"}, false},
		{"conflicting auth", map[string]any{"url": s.ClientURL(), "token": "tok-zzz-1", "username": "u", "password": "p"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := c.Test(context.Background(), &connectorv1.TestRequest{Connection: conn(t, tc.settings)})
			if err != nil {
				t.Fatal(err)
			}
			if r.Ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (%s)", r.Ok, tc.wantOK, r.Diagnostic)
			}
			if tok, _ := tc.settings["token"].(string); tok != "" && contains(r.Diagnostic, tok) {
				t.Fatalf("diagnostic leaks token: %s", r.Diagnostic)
			}
			if tc.wantOK && !contains(r.Diagnostic, "JetStream available") {
				t.Fatalf("expected JetStream in diagnostic: %s", r.Diagnostic)
			}
		})
	}
}

func contains(s, sub string) bool { return sub != "" && strings.Contains(s, sub) }

// TestPublishDelivers checks the message actually arrives, with headers, and that request/reply
// returns the reply as an output document.
func TestPublishAndReplyOutput(t *testing.T) {
	s := startServer(t, false)
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	got := make(chan *nats.Msg, 1)
	if _, err := nc.ChanSubscribe("events.>", got); err != nil {
		t.Fatal(err)
	}
	if _, err := nc.Subscribe("svc.echo", func(m *nats.Msg) { _ = m.Respond(append([]byte("echo:"), m.Data...)) }); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()

	run := func(start *connectorv1.ExecuteStart, d contracttest.Document) (*connectorv1.DocumentResult, []byte) {
		start.Connection = conn(t, map[string]any{"url": s.ClientURL()})
		cl := &memStream{ctx: context.Background(), in: []*connectorv1.ExecuteRequest{
			{Msg: &connectorv1.ExecuteRequest_Start{Start: start}},
			{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: &connectorv1.DocumentChunk{DocumentId: d.ID, Properties: d.Properties, Data: d.Data, Last: true}}},
			{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}},
		}}
		if err := natsconn.New().Execute(cl); err != nil {
			t.Fatal(err)
		}
		var res *connectorv1.DocumentResult
		var out []byte
		for _, r := range cl.out {
			if x := r.GetResult(); x != nil {
				res = x
			}
			if o := r.GetOutput(); o != nil {
				out = append(out, o.Data...)
			}
		}
		return res, out
	}

	res, _ := run(&connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_CREATE},
		contracttest.Document{ID: "d", Data: []byte("body"), Properties: map[string]string{natsconn.PropSubject: "events.x", natsconn.PropHeaderPrefix + "Trace": "t1"}})
	if res.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
		t.Fatalf("publish: %v %s", res.Status, res.Message)
	}
	select {
	case m := <-got:
		if string(m.Data) != "body" || m.Header.Get("Trace") != "t1" {
			t.Fatalf("unexpected message %q %v", m.Data, m.Header)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message not delivered")
	}

	res, out := run(&connectorv1.ExecuteStart{OperationType: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: natsconn.CustomRequest, Options: opts(t, map[string]any{"subject": "svc.echo"})},
		contracttest.Document{ID: "q", Data: []byte("hi")})
	if res.Status != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS || string(out) != "echo:hi" || len(res.OutputDocumentIds) != 1 {
		t.Fatalf("request: %v %s out=%q", res.Status, res.Message, out)
	}
}

// memStream is a minimal in-memory ConnectorService_ExecuteServer.
type memStream struct {
	connectorv1.ConnectorService_ExecuteServer
	ctx context.Context
	in  []*connectorv1.ExecuteRequest
	out []*connectorv1.ExecuteResponse
}

func (m *memStream) Context() context.Context { return m.ctx }
func (m *memStream) Recv() (*connectorv1.ExecuteRequest, error) {
	if len(m.in) == 0 {
		return nil, io.EOF
	}
	r := m.in[0]
	m.in = m.in[1:]
	return r, nil
}
func (m *memStream) Send(r *connectorv1.ExecuteResponse) error { m.out = append(m.out, r); return nil }
