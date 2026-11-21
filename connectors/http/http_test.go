// SPDX-License-Identifier: Apache-2.0

package http_test

import (
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/sdk/go/connector"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

const secret = "s3cr3t-token"

func newServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var flaky atomic.Int32
	mux := nethttp.NewServeMux()
	auth := func(h nethttp.HandlerFunc) nethttp.HandlerFunc {
		return func(w nethttp.ResponseWriter, r *nethttp.Request) {
			if r.Header.Get("Authorization") != "Bearer "+secret {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/", auth(func(w nethttp.ResponseWriter, r *nethttp.Request) { w.WriteHeader(200) }))
	mux.HandleFunc("/items/", auth(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/items/missing":
			w.WriteHeader(404)
		case "/items/boom":
			w.WriteHeader(500)
		case "/items/flaky":
			if flaky.Add(1) < 3 {
				w.WriteHeader(503)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(201)
			_, _ = w.Write([]byte(r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("X-Trace") + " " + string(b)))
		}
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &flaky
}

func conn(t *testing.T, base string) *connectorv1.ConnectionConfig {
	s, err := structpb.NewStruct(map[string]any{
		"baseUrl":        base,
		"auth":           map[string]any{"type": "bearer", "token": secret},
		"retries":        float64(2),
		"retryBackoffMs": float64(1),
		"timeoutMs":      float64(5000),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &connectorv1.ConnectionConfig{Settings: s}
}

func TestContract(t *testing.T) {
	srv, flaky := newServer(t)
	opts, _ := structpb.NewStruct(map[string]any{
		"path":         "/items/{{id}}",
		"headers":      map[string]any{"X-Trace": "t-{{id}}"},
		"bodyTemplate": `{"id":"{{id}}"}`,
	})
	statuses := map[string]connectorv1.DocumentStatus{
		"ok":      connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS,
		"missing": connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
		"boom":    connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
		"flaky":   connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS, // 503, 503, then 200
		"nopath":  connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR,
	}
	contracttest.Run(t, contracttest.Options{
		New:        func(testing.TB) connector.Connector { return http.New() },
		Connection: conn(t, srv.URL),
		Secrets:    []string{secret},
		Execute: []contracttest.ExecuteCase{{
			Name: "status mapping, templating and retries",
			Start: &connectorv1.ExecuteStart{
				OperationType: connectorv1.OperationType_OPERATION_TYPE_EXECUTE,
				CustomType:    "POST",
				Options:       opts,
			},
			Documents: []contracttest.Document{
				{ID: "ok", Properties: map[string]string{"id": "ok"}},
				{ID: "missing", Properties: map[string]string{"id": "missing"}},
				{ID: "boom", Properties: map[string]string{"id": "boom"}},
				{ID: "flaky", Properties: map[string]string{"id": "flaky"}},
				{ID: "nopath"}, // template references unknown property "id"
			},
			Want: statuses,
		}},
	})
	if got := flaky.Load(); got != 3 {
		t.Errorf("flaky endpoint hit %d times, want 3 (2 retries)", got)
	}
}

func TestMethodsBodyAndOutput(t *testing.T) {
	srv, _ := newServer(t)
	c := http.New()
	for _, m := range []string{"GET", "PUT", "PATCH", "DELETE"} {
		st := &connectorv1.ExecuteStart{Connection: conn(t, srv.URL), CustomType: m}
		res, outs := run(t, c, st, contracttest.Document{
			ID: "d", Data: []byte("payload"),
			Properties: map[string]string{"http.path": "/items/x", "http.query.q": "1", "http.header.X-Trace": "tr"},
		})
		if res["d"].GetStatus() != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
			t.Fatalf("%s: %v %s", m, res["d"].GetStatus(), res["d"].GetMessage())
		}
		want := m + " /items/x?q=1 tr payload"
		if m == "GET" {
			want = "GET /items/x?q=1 tr " // GET drops the body
		}
		if got := string(outs["d.response"]); got != want {
			t.Errorf("%s: response %q, want %q", m, got, want)
		}
		if res["d"].GetTracked()["status_code"] != "201" {
			t.Errorf("tracked status_code = %q", res["d"].GetTracked()["status_code"])
		}
	}
}

func TestAuthRejected(t *testing.T) {
	srv, _ := newServer(t)
	s, _ := structpb.NewStruct(map[string]any{"baseUrl": srv.URL, "auth": map[string]any{"type": "basic", "username": "u", "password": "pw"}})
	resp, err := http.New().Test(t.Context(), &connectorv1.TestRequest{Connection: &connectorv1.ConnectionConfig{Settings: s}})
	if err != nil || resp.GetOk() {
		t.Fatalf("expected failed Test, got %v %v", resp, err)
	}
}

func TestAPIKeyInQuery(t *testing.T) {
	var got string
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) { got = r.URL.Query().Get("k") }))
	defer srv.Close()
	s, _ := structpb.NewStruct(map[string]any{"baseUrl": srv.URL, "auth": map[string]any{"type": "apikey", "apiKeyName": "k", "apiKeyValue": "v", "apiKeyIn": "query"}})
	resp, _ := http.New().Test(t.Context(), &connectorv1.TestRequest{Connection: &connectorv1.ConnectionConfig{Settings: s}})
	if !resp.GetOk() || got != "v" {
		t.Fatalf("ok=%v key=%q", resp.GetOk(), got)
	}
}

func TestParseOpenAPI(t *testing.T) {
	doc := `{"openapi":"3.0.3","paths":{"/pets":{"post":{"operationId":"addPet","requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Pet"}}}},"responses":{"201":{"content":{"application/json":{"schema":{"type":"object"}}}}}},"get":{"summary":"List"}}},"components":{"schemas":{"Pet":{"type":"object","properties":{"name":{"type":"string"}}}}}}`
	ops, err := http.ParseOpenAPI([]byte(doc))
	if err != nil || len(ops) != 2 {
		t.Fatalf("ops=%v err=%v", ops, err)
	}
	if ops[0].ID != "GET /pets" || ops[1].ID != "addPet" || ops[1].Method != "POST" || len(ops[1].RequestBody) == 0 || len(ops[1].Response) == 0 {
		t.Errorf("unexpected ops: %+v", ops)
	}
}

// run drives Execute directly through a minimal in-memory stream.
func run(t *testing.T, c *http.Connector, st *connectorv1.ExecuteStart, d contracttest.Document) (map[string]*connectorv1.DocumentResult, map[string][]byte) {
	t.Helper()
	s := &memStream{t: t, in: []*connectorv1.ExecuteRequest{
		{Msg: &connectorv1.ExecuteRequest_Start{Start: st}},
		{Msg: &connectorv1.ExecuteRequest_Chunk{Chunk: &connectorv1.DocumentChunk{DocumentId: d.ID, Properties: d.Properties, Data: d.Data, Last: true}}},
		{Msg: &connectorv1.ExecuteRequest_End{End: &connectorv1.ExecuteEnd{}}},
	}, res: map[string]*connectorv1.DocumentResult{}, out: map[string][]byte{}}
	if err := c.Execute(s); err != nil {
		t.Fatal(err)
	}
	return s.res, s.out
}
