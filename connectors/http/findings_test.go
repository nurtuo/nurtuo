// SPDX-License-Identifier: Apache-2.0

package http_test

import (
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/sdk/go/connector/contracttest"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func execOne(t *testing.T, base string, extra map[string]any) (*connectorv1.DocumentResult, map[string][]byte) {
	t.Helper()
	m := map[string]any{"baseUrl": base, "retries": float64(0)}
	for k, v := range extra {
		m[k] = v
	}
	s, _ := structpb.NewStruct(m)
	st := &connectorv1.ExecuteStart{Connection: &connectorv1.ConnectionConfig{Settings: s}, CustomType: "GET"}
	res, outs := run(t, http.New(), st, contracttest.Document{ID: "d", Properties: map[string]string{"http.path": "/x"}})
	return res["d"], outs
}

func TestAPIKeyInQueryNotLeakedInTracked(t *testing.T) {
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	r, _ := execOne(t, srv.URL, map[string]any{
		"auth": map[string]any{"type": "apikey", "apiKeyName": "api_key", "apiKeyValue": "TOPSECRETKEY", "apiKeyIn": "query"},
	})
	if u := r.GetTracked()["url"]; u == "" || strings.Contains(u, "TOPSECRETKEY") {
		t.Errorf("tracked url leaks or empty: %q", u)
	}
}

func TestCrossHostRedirectStripsCredentials(t *testing.T) {
	var gotKey, gotCustom, gotAuth string
	target := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		gotKey, gotCustom, gotAuth = r.Header.Get("X-Api-Key"), r.Header.Get("X-Custom"), r.Header.Get("Authorization")
	}))
	defer target.Close()
	origin := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		nethttp.Redirect(w, r, target.URL+"/y", nethttp.StatusFound)
	}))
	defer origin.Close()
	r, _ := execOne(t, origin.URL, map[string]any{
		"auth":    map[string]any{"type": "apikey", "apiKeyName": "X-Api-Key", "apiKeyValue": "k1"},
		"headers": map[string]any{"X-Custom": "c1"},
	})
	if r.GetStatus() != connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
		t.Fatalf("status %v %s", r.GetStatus(), r.GetMessage())
	}
	if gotKey != "" || gotCustom != "" || gotAuth != "" {
		t.Errorf("credentials forwarded cross-host: key=%q custom=%q auth=%q", gotKey, gotCustom, gotAuth)
	}
}

func TestRedirectLoopCapped(t *testing.T) {
	var n atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		n.Add(1)
		nethttp.Redirect(w, r, srv.URL+"/again", nethttp.StatusFound)
	}))
	defer srv.Close()
	r, _ := execOne(t, srv.URL, nil)
	if r.GetStatus() == connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS || n.Load() > 11 {
		t.Errorf("status=%v hits=%d", r.GetStatus(), n.Load())
	}
}

func TestOversizeResponseNotSuccess(t *testing.T) {
	chunk := make([]byte, 1<<20)
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		for i := 0; i < 33; i++ {
			_, _ = w.Write(chunk)
		}
	}))
	defer srv.Close()
	r, _ := execOne(t, srv.URL, nil)
	if r.GetStatus() == connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS {
		t.Fatalf("truncated response reported SUCCESS")
	}
	if !strings.Contains(r.GetMessage(), "exceeds") {
		t.Errorf("message not clear: %q", r.GetMessage())
	}
}
