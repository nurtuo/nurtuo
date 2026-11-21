// SPDX-License-Identifier: AGPL-3.0-only

package engine_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	httpconn "github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeConn mirrors the contracttest fake: documents whose payload contains "bad" are rejected with
// APPLICATION_ERROR, everything else succeeds. Documents containing "echo" also yield an output.
type fakeConn struct{}

func (fakeConn) Describe(context.Context) (*connectorv1.Descriptor, error) {
	return &connectorv1.Descriptor{Id: "fake", Version: "1.0.0", Host: connectorv1.Host_HOST_GO_NATIVE}, nil
}
func (fakeConn) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}
func (fakeConn) Test(context.Context, *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	return &connectorv1.TestResponse{Ok: true}, nil
}
func (fakeConn) Execute(s connectorv1.ConnectorService_ExecuteServer) error {
	var order []string
	data := map[string][]byte{}
	for {
		req, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if ch := req.GetChunk(); ch != nil {
			if _, seen := data[ch.GetDocumentId()]; !seen {
				order = append(order, ch.GetDocumentId())
			}
			data[ch.GetDocumentId()] = append(data[ch.GetDocumentId()], ch.GetData()...)
		}
	}
	for _, id := range order {
		res := &connectorv1.DocumentResult{DocumentId: id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS}
		switch {
		case bytes.Contains(data[id], []byte("bad")):
			res.Status, res.Message = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR, "rejected"
		case bytes.Contains(data[id], []byte("echo")):
			res.OutputDocumentIds = []string{id + ".out"}
			if err := s.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{
				Output: &connectorv1.DocumentChunk{DocumentId: id + ".out", Data: []byte(`{"echoed":true}`), Last: true}}}); err != nil {
				return err
			}
		}
		if err := s.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: res}}); err != nil {
			return err
		}
	}
	return nil
}

func payloadDoc(t *testing.T, e *engine.Engine, id, body string, props map[string]string) *docs.Document {
	t.Helper()
	p, err := e.Spool.FromBytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return docs.New(id, props, p)
}

func body(t *testing.T, d *docs.Document) string {
	t.Helper()
	b, err := d.Payload().Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConnectorRunnerFakeConnector(t *testing.T) {
	e := engine.New()
	conns := engine.NewConnectors()
	conns.Register("fake", engine.ConnectorEntry{Connector: fakeConn{}})
	e.Registry.RegisterConnectorRunner(conns)
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg,
		"c:connector:" + `{"connector":"fake","operation":"CREATE"}`,
		"r:returnDocuments:{}",
	}, map[string]string{"start:default": "c", "c:default": "r"}))
	in := docs.NewSet(
		payloadDoc(t, e, "ok", "hello", map[string]string{"k": "v"}),
		payloadDoc(t, e, "bad", "bad data", nil),
		payloadDoc(t, e, "e", "echo me", map[string]string{"k": "v"}),
	)
	res, err := e.Run(context.Background(), f, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Returned.Close() }()
	defer func() { _ = res.Failed.Close() }()
	if got := ids(res.Returned); len(got) != 2 || got[0] != "ok" || got[1] != "e.out" {
		t.Fatalf("returned %v", got)
	}
	if d := res.Returned.At(1); body(t, d) != `{"echoed":true}` || d.Props["k"] != "v" {
		t.Fatalf("output doc %q props %v", body(t, d), d.Props)
	}
	if got := ids(res.Failed); len(got) != 1 || got[0] != "bad" {
		t.Fatalf("failed %v", got)
	}
	if err := res.Failed.At(0).Err(); err == nil || err.Code != "application_error" || err.Message != "rejected" || err.Step != "c" {
		t.Fatalf("failed err %+v", err)
	}
}

func TestConnectorRunnerUnregistered(t *testing.T) {
	e := engine.New()
	e.Registry.RegisterConnectorRunner(engine.NewConnectors())
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg, "c:connector:" + `{"connector":"nope","operation":"GET"}`, "r:returnDocuments:{}",
	}, map[string]string{"start:default": "c", "c:default": "r"}))
	res, err := e.Run(context.Background(), f, set(map[string]string{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Returned.Len() != 0 || res.Failed.Len() != 1 {
		t.Fatalf("returned %d failed %d", res.Returned.Len(), res.Failed.Len())
	}
}

func TestMapRunner(t *testing.T) {
	e := engine.New()
	f := mustFlow(t, mk("f", []string{
		"start:start:" + startCfg,
		"m:map:" + `{"jsonata":"{ 'name': first & ' ' & last }"}`,
		"r:returnDocuments:{}",
	}, map[string]string{"start:default": "m", "m:default": "r"}))
	in := docs.NewSet(
		payloadDoc(t, e, "a", `{"first":"Ada","last":"Lovelace"}`, nil),
		payloadDoc(t, e, "b", `not json`, nil),
	)
	res, err := e.Run(context.Background(), f, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Returned.Close() }()
	defer func() { _ = res.Failed.Close() }()
	if res.Returned.Len() != 1 || body(t, res.Returned.At(0)) != `{"name":"Ada Lovelace"}` {
		t.Fatalf("returned %v", ids(res.Returned))
	}
	if res.Failed.Len() != 1 || res.Failed.At(0).ID != "b" {
		t.Fatalf("failed %v", ids(res.Failed))
	}
}

func TestCELEvaluator(t *testing.T) {
	ev := &engine.CELEvaluator{}
	d := docs.New("x1", map[string]string{"kind": "a"}, nil)
	p := engine.NewProcess(map[string]string{"k": "v"})
	for _, c := range []struct {
		expr string
		want bool
		err  bool
	}{
		{"doc.id != ''", true, false},
		{"doc.props.kind == 'a' && process.props.k == 'v'", true, false},
		{"'zz' in doc.props", false, false},
		{"int('7') >= 5", true, false},
		{"doc.id", false, true}, // not bool
		{"nope(", false, true},
		{"doc.props.missing == ''", false, true}, // missing key is an error in CEL
	} {
		got, err := ev.Condition(c.expr, d, p)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%q: got %v err %v", c.expr, got, err)
		}
	}
}

// TestGetEmployeeFlowEndToEnd runs examples/flows/get-employee.flow.json unmodified. The flow's
// "postgres" connector is served by the HTTP connector against an httptest server (a stand-in for
// the employee table), the decision uses real CEL and the map step reshapes the row.
func TestGetEmployeeFlowEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/employees/7" {
			_, _ = w.Write([]byte(`{"id":7,"first_name":"Grace","last_name":"Hopper"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	raw, err := os.ReadFile("../../../../examples/flows/get-employee.flow.json")
	if err != nil {
		t.Fatal(err)
	}
	f := mustFlow(t, string(raw))

	settings, _ := structpb.NewStruct(map[string]any{"baseUrl": srv.URL})
	opts, _ := structpb.NewStruct(map[string]any{"path": "/employees/{{id}}"})
	e := engine.New()
	e.Eval = &engine.CELEvaluator{}
	conns := engine.NewConnectors()
	conns.Register("postgres", engine.ConnectorEntry{
		Connector:  httpconn.New(),
		Connection: &connectorv1.ConnectionConfig{Settings: settings},
		Options:    opts,
	})
	e.Registry.RegisterConnectorRunner(conns)

	res, err := e.Run(context.Background(), f, set(map[string]string{"id": "7"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Returned.Close() }()
	defer func() { _ = res.Failed.Close() }()
	if res.Failed.Len() != 0 || res.Returned.Len() != 1 {
		t.Fatalf("returned %d failed %d (%v) err %v", res.Returned.Len(), res.Failed.Len(), res.Stop, res.Failed.At(0).Err())
	}
	got := body(t, res.Returned.At(0))
	if !strings.Contains(got, `"name":"Grace Hopper"`) || !strings.Contains(got, `"id":7`) {
		t.Fatalf("response %s", got)
	}

	// Unknown employee: the connector reports a non-2xx as a document error, so it fails before
	// the decision and never reaches Return.
	res2, err := e.Run(context.Background(), f, set(map[string]string{"id": "8"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res2.Returned.Close() }()
	defer func() { _ = res2.Failed.Close() }()
	if res2.Returned.Len() != 0 || res2.Failed.Len() != 1 {
		t.Fatalf("404 case: returned %d failed %d", res2.Returned.Len(), res2.Failed.Len())
	}
}
