// SPDX-License-Identifier: Apache-2.0

package sftp_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/connectors/sftp"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	stOK     = connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
	stAppErr = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
	stFail   = connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE
)

func opStart(o connectorv1.OperationType, custom string) *connectorv1.ExecuteStart {
	return &connectorv1.ExecuteStart{OperationType: o, CustomType: custom}
}

var (
	opCreate = func() *connectorv1.ExecuteStart { return opStart(connectorv1.OperationType_OPERATION_TYPE_CREATE, "") }
	opUpsert = func() *connectorv1.ExecuteStart { return opStart(connectorv1.OperationType_OPERATION_TYPE_UPSERT, "") }
	opGet    = func() *connectorv1.ExecuteStart { return opStart(connectorv1.OperationType_OPERATION_TYPE_GET, "") }
	opQuery  = func() *connectorv1.ExecuteStart { return opStart(connectorv1.OperationType_OPERATION_TYPE_QUERY, "") }
	opDelete = func() *connectorv1.ExecuteStart { return opStart(connectorv1.OperationType_OPERATION_TYPE_DELETE, "") }
	opRename = func() *connectorv1.ExecuteStart {
		return opStart(connectorv1.OperationType_OPERATION_TYPE_EXECUTE, "RENAME")
	}
)

// connWith builds connection settings with a root path and extra settings.
func connWith(t *testing.T, addr, fp, root string, extra map[string]any) *connectorv1.ConnectionConfig {
	t.Helper()
	c := settings(t, addr, fp, testPassword)
	m := c.Settings.AsMap()
	if root != "" {
		m["rootPath"] = root
	}
	for k, v := range extra {
		m[k] = v
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return &connectorv1.ConnectionConfig{Settings: s}
}

// admin opens a direct SFTP client on the same server (for seeding and inspecting state).
func admin(t *testing.T, addr, fp string) *pkgsftp.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.Password(testPassword)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if ssh.FingerprintSHA256(k) != fp {
				return errors.New("fp")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := pkgsftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close(); _ = c.Close() })
	return fs
}

func seed(t *testing.T, fs *pkgsftp.Client, p, content string) {
	t.Helper()
	_ = fs.MkdirAll(pathDir(p))
	f, err := fs.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func pathDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func readRemote(t *testing.T, fs *pkgsftp.Client, p string) (string, error) {
	t.Helper()
	f, err := fs.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	return string(b), err
}

func listNames(t *testing.T, fs *pkgsftp.Client, dir string) []string {
	t.Helper()
	infos, err := fs.ReadDir(dir)
	if err != nil {
		return nil
	}
	var n []string
	for _, i := range infos {
		n = append(n, i.Name())
	}
	sort.Strings(n)
	return n
}

// step is one chunk sent to Execute.
type step struct {
	id    string
	props map[string]string
	data  string
	last  bool
}

type exchange struct {
	t       *testing.T
	s       connectorv1.ConnectorService_ExecuteClient
	cancel  context.CancelFunc
	results map[string][]*connectorv1.DocumentResult
	out     map[string][]byte
}

func openExec(t *testing.T, conn *connectorv1.ConnectionConfig, st *connectorv1.ExecuteStart) *exchange {
	t.Helper()
	cl := dialConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	s, err := cl.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.Connection = conn
	if err := s.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Start{Start: st}}); err != nil {
		t.Fatal(err)
	}
	return &exchange{t: t, s: s, cancel: cancel, results: map[string][]*connectorv1.DocumentResult{}, out: map[string][]byte{}}
}

func (x *exchange) send(st step) {
	x.t.Helper()
	ch := &connectorv1.DocumentChunk{DocumentId: st.id, Properties: st.props, Data: []byte(st.data), Last: st.last}
	if err := x.s.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: ch}}); err != nil {
		x.t.Fatal(err)
	}
}

// finish half-closes (optionally after End) and collects every response until the stream ends.
func (x *exchange) finish(sendEnd bool) error {
	x.t.Helper()
	if sendEnd {
		_ = x.s.Send(&connectorv1.ExecuteRequest{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}})
	}
	_ = x.s.CloseSend()
	for {
		r, err := x.s.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if res := r.GetResult(); res != nil {
			x.results[res.GetDocumentId()] = append(x.results[res.GetDocumentId()], res)
		}
		if o := r.GetOutput(); o != nil {
			x.out[o.GetDocumentId()] = append(x.out[o.GetDocumentId()], o.GetData()...)
		}
	}
}

// one runs a single complete document and returns its only result.
func one(t *testing.T, conn *connectorv1.ConnectionConfig, st *connectorv1.ExecuteStart, id string, props map[string]string, data string) (*connectorv1.DocumentResult, []byte) {
	t.Helper()
	x := openExec(t, conn, st)
	x.send(step{id: id, props: props, data: data, last: true})
	if err := x.finish(true); err != nil {
		t.Fatal(err)
	}
	if len(x.results[id]) != 1 {
		t.Fatalf("want exactly one result for %s, got %d", id, len(x.results[id]))
	}
	return x.results[id][0], x.out[id+".out"]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func partFiles(names []string) []string {
	var p []string
	for _, n := range names {
		if strings.HasPrefix(n, ".nurtuo-") && strings.HasSuffix(n, ".part") {
			p = append(p, n)
		}
	}
	return p
}

// ---- B1: unfinished uploads ----

func TestUnfinishedUploadSwitchedAwayIsAbortedAndFailed(t *testing.T) {
	addr, fp := startServer(t)
	fs := admin(t, addr, fp)
	x := openExec(t, connWith(t, addr, fp, "", nil), opUpsert())
	x.send(step{id: "a", props: map[string]string{"path": "/u/a.txt"}, data: "partial", last: false})
	x.send(step{id: "b", props: map[string]string{"path": "/u/b.txt"}, data: "whole", last: true})
	if err := x.finish(true); err != nil {
		t.Fatal(err)
	}
	if len(x.results["a"]) != 1 || x.results["a"][0].Status != stFail {
		t.Fatalf("a: %v", x.results["a"])
	}
	if len(x.results["b"]) != 1 || x.results["b"][0].Status != stOK {
		t.Fatalf("b: %v", x.results["b"])
	}
	if n := listNames(t, fs, "/u"); len(n) != 1 || n[0] != "b.txt" {
		t.Fatalf("leftover files: %v", n)
	}
}

func TestUnfinishedUploadAtStreamEnd(t *testing.T) {
	for _, sendEnd := range []bool{true, false} {
		addr, fp := startServer(t)
		fs := admin(t, addr, fp)
		x := openExec(t, connWith(t, addr, fp, "", nil), opUpsert())
		x.send(step{id: "c", props: map[string]string{"path": "/u/c.txt"}, data: "partial", last: false})
		if err := x.finish(sendEnd); err != nil {
			t.Fatal(err)
		}
		if len(x.results["c"]) != 1 || x.results["c"][0].Status != stFail {
			t.Fatalf("end=%v c: %v", sendEnd, x.results["c"])
		}
		if n := listNames(t, fs, "/u"); len(n) != 0 {
			t.Fatalf("end=%v leftover files: %v", sendEnd, n)
		}
	}
}

// ---- B2: rename fallback safety ----

type faultCmd struct {
	base pkgsftp.FileCmder
	fail func(method, from, to string) error
}

func (f *faultCmd) Filecmd(r *pkgsftp.Request) error {
	if err := f.fail(r.Method, r.Filepath, r.Target); err != nil {
		return err
	}
	return f.base.Filecmd(r)
}

// noPosix wraps the command handler so the posix-rename extension is unsupported (exercising the
// remove+rename fallback) and injects failures.
func noPosix(fail func(method, from, to string) error) func(pkgsftp.Handlers) pkgsftp.Handlers {
	return func(h pkgsftp.Handlers) pkgsftp.Handlers {
		h.FileCmd = &faultCmd{base: h.FileCmd, fail: fail}
		return h
	}
}

func TestUpsertFailedRenameKeepsExistingDestination(t *testing.T) {
	addr, fp, _ := startServerH(t, noPosix(func(m, from, _ string) error {
		if m == "Rename" && strings.HasSuffix(from, ".part") {
			return errors.New("injected rename failure")
		}
		return nil
	}))
	fs := admin(t, addr, fp)
	seed(t, fs, "/d/x.txt", "old")
	res, _ := one(t, connWith(t, addr, fp, "", nil), opUpsert(), "u", map[string]string{"path": "/d/x.txt"}, "new")
	if res.Status == stOK {
		t.Fatal("upload must fail when the final rename fails")
	}
	got, err := readRemote(t, fs, "/d/x.txt")
	if err != nil || got != "old" {
		t.Fatalf("destination lost or changed: %q %v", got, err)
	}
	if n := listNames(t, fs, "/d"); len(n) != 1 {
		t.Fatalf("leftover files: %v", n)
	}
}

func TestUpsertFallbackReplacesDestination(t *testing.T) {
	addr, fp, _ := startServerH(t, noPosix(func(string, string, string) error { return nil }))
	fs := admin(t, addr, fp)
	seed(t, fs, "/d/x.txt", "old")
	res, _ := one(t, connWith(t, addr, fp, "", nil), opUpsert(), "u", map[string]string{"path": "/d/x.txt"}, "new")
	if res.Status != stOK {
		t.Fatalf("res %v", res)
	}
	if got, _ := readRemote(t, fs, "/d/x.txt"); got != "new" {
		t.Fatalf("content %q", got)
	}
	if n := listNames(t, fs, "/d"); len(n) != 1 {
		t.Fatalf("leftover files: %v", n)
	}
}

// ---- B3: symlinks and root protection ----

func TestSymlinkEscapeAndRootProtection(t *testing.T) {
	addr, fp := startServer(t)
	fs := admin(t, addr, fp)
	seed(t, fs, "/secret.txt", "top secret")
	seed(t, fs, "/jail/sub/f.txt", "inside")
	if err := fs.Symlink("/", "/jail/esc"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("../secret.txt", "/jail/rel"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("/jail/sub", "/jail/ok"); err != nil {
		t.Fatal(err)
	}
	conn := connWith(t, addr, fp, "/jail", nil)

	expectApp := func(name string, st *connectorv1.ExecuteStart, props map[string]string) {
		t.Helper()
		res, _ := one(t, conn, st, name, props, "payload")
		if res.Status != stAppErr {
			t.Fatalf("%s: want APPLICATION_ERROR, got %v %q", name, res.Status, res.Message)
		}
	}
	expectApp("get-esc", opGet(), map[string]string{"path": "/esc/secret.txt"})
	expectApp("get-rel", opGet(), map[string]string{"path": "/rel"})
	expectApp("put-esc", opUpsert(), map[string]string{"path": "/esc/new.txt"})
	expectApp("del-esc", opDelete(), map[string]string{"path": "/esc/secret.txt"})
	expectApp("ren-esc", opRename(), map[string]string{"path": "/sub/f.txt", "newPath": "/esc/moved.txt"})
	expectApp("ren-from-esc", opRename(), map[string]string{"path": "/esc/secret.txt", "newPath": "/stolen.txt"})
	expectApp("list-esc", opQuery(), map[string]string{"path": "/esc"})
	expectApp("del-root", opDelete(), map[string]string{"path": "/"})
	expectApp("del-root-dots", opDelete(), map[string]string{"path": "/sub/../.."})
	expectApp("ren-root", opRename(), map[string]string{"path": "/", "newPath": "/elsewhere"})
	expectApp("ren-onto-root", opRename(), map[string]string{"path": "/sub/f.txt", "newPath": "/"})

	if got, err := readRemote(t, fs, "/secret.txt"); err != nil || got != "top secret" {
		t.Fatalf("outside file touched: %q %v", got, err)
	}
	for _, p := range []string{"/new.txt", "/moved.txt", "/stolen.txt", "/elsewhere"} {
		if _, err := fs.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must not exist (err %v)", p, err)
		}
	}
	if _, err := fs.Stat("/jail"); err != nil {
		t.Fatalf("root removed or renamed: %v", err)
	}
	// a symlink that stays inside the root still works
	res, out := one(t, conn, opGet(), "ok", map[string]string{"path": "/ok/f.txt"}, "")
	if res.Status != stOK || string(out) != "inside" {
		t.Fatalf("in-root symlink: %v %q", res, out)
	}
}

// ---- B4: create semantics, listings, caps, temp names, dial ctx ----

func TestCreateFailsIfExistsUpsertOverwrites(t *testing.T) {
	addr, fp := startServer(t)
	fs := admin(t, addr, fp)
	seed(t, fs, "/c.txt", "old")
	conn := connWith(t, addr, fp, "", nil)
	res, _ := one(t, conn, opCreate(), "c", map[string]string{"path": "/c.txt"}, "new")
	if res.Status != stAppErr {
		t.Fatalf("create over existing: %v", res)
	}
	if got, _ := readRemote(t, fs, "/c.txt"); got != "old" {
		t.Fatalf("content %q", got)
	}
	if n := listNames(t, fs, "/"); len(n) != 1 {
		t.Fatalf("leftover files: %v", n)
	}
	res, _ = one(t, conn, opUpsert(), "u", map[string]string{"path": "/c.txt"}, "new")
	if res.Status != stOK {
		t.Fatalf("upsert: %v", res)
	}
	if got, _ := readRemote(t, fs, "/c.txt"); got != "new" {
		t.Fatalf("content %q", got)
	}
	res, _ = one(t, conn, opCreate(), "n", map[string]string{"path": "/fresh.txt"}, "x")
	if res.Status != stOK {
		t.Fatalf("create new: %v", res)
	}
}

func TestListExcludesPartFilesAndCaps(t *testing.T) {
	addr, fp := startServer(t)
	fs := admin(t, addr, fp)
	seed(t, fs, "/l/a.txt", "1")
	seed(t, fs, "/l/.nurtuo-a.txt.deadbeef.part", "tmp")
	conn := connWith(t, addr, fp, "", nil)
	res, out := one(t, conn, opQuery(), "q", map[string]string{"path": "/l"}, "")
	if res.Status != stOK || strings.Contains(string(out), ".part") || !strings.Contains(string(out), "a.txt") {
		t.Fatalf("list: %v %s", res, out)
	}
	if res.Tracked["truncated"] != "" {
		t.Fatalf("unexpected truncation: %v", res.Tracked)
	}

	for _, n := range []string{"b", "c", "d", "e"} {
		seed(t, fs, "/l/"+n+".txt", n)
	}
	capped := connWith(t, addr, fp, "", map[string]any{"maxListEntries": 3})
	res, out = one(t, capped, opQuery(), "q2", map[string]string{"path": "/l"}, "")
	if res.Status != stOK || res.Tracked["truncated"] != "true" {
		t.Fatalf("cap: %v", res)
	}
	if c := strings.Count(string(out), `"name"`); c != 3 {
		t.Fatalf("want 3 entries, got %d: %s", c, out)
	}
	// default cap is 10000: well under it nothing is truncated
	res, _ = one(t, conn, opQuery(), "q3", map[string]string{"path": "/l"}, "")
	if res.Tracked["truncated"] != "" {
		t.Fatalf("default cap truncated: %v", res.Tracked)
	}
}

func TestUploadUsesPartFileThenRenames(t *testing.T) {
	addr, fp := startServer(t)
	fs := admin(t, addr, fp)
	long := strings.Repeat("n", 200) + ".txt"
	conn := connWith(t, addr, fp, "", nil)
	x1 := openExec(t, conn, opUpsert())
	x2 := openExec(t, conn, opUpsert())
	x1.send(step{id: "a", props: map[string]string{"path": "/p/" + long}, data: "first-half", last: false})
	x2.send(step{id: "b", props: map[string]string{"path": "/p/" + long}, data: "other-half", last: false})
	// mid-upload: temp files exist (unique per upload), the destination does not
	waitFor(t, "two part files", func() bool { return len(partFiles(listNames(t, fs, "/p"))) == 2 })
	names := listNames(t, fs, "/p")
	for _, n := range names {
		if n == long {
			t.Fatal("destination written directly before the upload finished")
		}
		if len(n) > 140 {
			t.Fatalf("temp name not bounded: %d chars", len(n))
		}
	}
	if len(names) != 2 {
		t.Fatalf("names %v", names)
	}
	// complete upload a
	x1.send(step{id: "a", data: "", last: true})
	if err := x1.finish(true); err != nil {
		t.Fatal(err)
	}
	if x1.results["a"][0].Status != stOK {
		t.Fatalf("a: %v", x1.results["a"][0])
	}
	if got, _ := readRemote(t, fs, "/p/"+long); got != "first-half" {
		t.Fatalf("content %q", got)
	}
	_ = x2.finish(true) // b is aborted
	if n := listNames(t, fs, "/p"); len(n) != 1 || n[0] != long {
		t.Fatalf("leftovers %v", n)
	}
}

func TestDialHonoursContextCancellation(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // accept and stay silent: the SSH handshake never completes
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	conn := connWith(t, l.Addr().String(), "SHA256:x", "", map[string]any{"timeoutMs": 60000})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan *connectorv1.TestResponse, 1)
	go func() {
		r, _ := sftp.New().Test(ctx, &connectorv1.TestRequest{Connection: conn})
		done <- r
	}()
	select {
	case r := <-done:
		if r.Ok {
			t.Fatal("must not succeed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Test did not honour context cancellation during dial")
	}
}

// ---- B5: mutation killers ----

func TestConfinementBelowRootPath(t *testing.T) {
	addr, fp := startServer(t)
	fs := admin(t, addr, fp)
	seed(t, fs, "/secret.txt", "outside")
	seed(t, fs, "/jail/inside.txt", "inside")
	conn := connWith(t, addr, fp, "/jail", nil)
	res, out := one(t, conn, opGet(), "t", map[string]string{"path": "../secret.txt"}, "")
	if res.Status == stOK || strings.Contains(string(out), "outside") {
		t.Fatalf("traversal escaped the root: %v %q", res, out)
	}
	res, out = one(t, conn, opGet(), "t2", map[string]string{"path": "/a/../../secret.txt"}, "")
	if res.Status == stOK || strings.Contains(string(out), "outside") {
		t.Fatalf("traversal escaped the root: %v %q", res, out)
	}
	// uploads cannot escape either
	_, _ = one(t, conn, opUpsert(), "t3", map[string]string{"path": "../escaped.txt"}, "x")
	if _, err := fs.Stat("/escaped.txt"); err == nil {
		t.Fatal("upload escaped the root")
	}
	// positive control: in-root access works
	res, out = one(t, conn, opGet(), "ok", map[string]string{"path": "inside.txt"}, "")
	if res.Status != stOK || string(out) != "inside" {
		t.Fatalf("control: %v %q", res, out)
	}
}

func TestSecretsNeverLeakInResults(t *testing.T) {
	addr, fp := startServer(t)
	conn := connWith(t, addr, fp, "/jail", nil)
	fs := admin(t, addr, fp)
	seed(t, fs, "/jail/x", "x")
	if err := fs.Symlink("/", "/jail/"+testPassword); err != nil {
		t.Fatal(err)
	}
	// the secret ends up inside the error text (it is part of the requested path) and must be masked
	res, _ := one(t, conn, opGet(), "s", map[string]string{"path": "/" + testPassword + "/x"}, "")
	if res.Status != stAppErr || !strings.Contains(res.Message, "resolves outside") {
		t.Fatalf("res %v", res)
	}
	if strings.Contains(res.Message, testPassword) {
		t.Fatalf("secret leaked: %q", res.Message)
	}
	if !strings.Contains(res.Message, "***") {
		t.Fatalf("expected masked secret in %q", res.Message)
	}
	// ordinary failures must not carry the secret either
	res, _ = one(t, conn, opGet(), "s2", map[string]string{"path": "/missing"}, "")
	if res.Message == "" || strings.Contains(res.Message, testPassword) {
		t.Fatalf("message %q", res.Message)
	}
	// connection failures (Execute level) are redacted too
	bad := connWith(t, addr, fp, "", map[string]any{"password": "wrong-" + testPassword})
	x := openExec(t, bad, opGet())
	x.send(step{id: "z", props: map[string]string{"path": "/x"}, last: true})
	err := x.finish(true)
	if err == nil || strings.Contains(err.Error(), "wrong-"+testPassword) {
		t.Fatalf("execute error %v", err)
	}
}

func TestTrackedPathValues(t *testing.T) {
	addr, fp := startServer(t)
	fs := admin(t, addr, fp)
	seed(t, fs, "/t/g.txt", "g")
	seed(t, fs, "/t/r.txt", "r")
	seed(t, fs, "/t/d.txt", "d")
	conn := connWith(t, addr, fp, "", nil)
	cases := []struct {
		name  string
		st    *connectorv1.ExecuteStart
		props map[string]string
		want  string
	}{
		{"put", opUpsert(), map[string]string{"path": "/t/new.txt"}, "/t/new.txt"},
		{"get", opGet(), map[string]string{"path": "/t/g.txt"}, "/t/g.txt"},
		{"list", opQuery(), map[string]string{"path": "/t"}, "/t"},
		{"rename", opRename(), map[string]string{"path": "/t/r.txt", "newPath": "/t/r2.txt"}, "/t/r.txt"},
		{"delete", opDelete(), map[string]string{"path": "/t/d.txt"}, "/t/d.txt"},
	}
	for _, c := range cases {
		res, _ := one(t, conn, c.st, c.name, c.props, "data")
		if res.Status != stOK || res.Tracked["path"] != c.want {
			t.Errorf("%s: status %v tracked %v", c.name, res.Status, res.Tracked)
		}
	}
}
