// SPDX-License-Identifier: AGPL-3.0-only

package listener_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/listener"
)

// Regression: request metadata must not use the property names the HTTP connector treats as
// per-document overrides (http.method / http.path / http.header.* / http.query.*), otherwise a
// flow that calls a backend forwards the inbound method, path, headers and query to it.
func TestHTTPRequestPropsDoNotCollideWithConnectorOverrides(t *testing.T) {
	var got map[string]string
	l, err := listener.NewHTTP(listener.HTTPConfig{Path: "/hook"})
	if err != nil {
		t.Fatal(err)
	}
	h := func(_ context.Context, in *docs.Set) (*engine.Result, error) {
		got = in.At(0).Props
		return &engine.Result{Returned: docs.NewSet(), Failed: docs.NewSet()}, nil
	}
	if err := l.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Stop(context.Background()) }()
	req, _ := http.NewRequest("POST", "http://"+l.Status().Addr+"/hook?who=x", strings.NewReader("b"))
	req.Header.Set("Authorization", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for k := range got {
		if k == "http.method" || k == "http.path" || strings.HasPrefix(k, "http.header.") || strings.HasPrefix(k, "http.query.") {
			t.Errorf("listener sets connector override property %q", k)
		}
	}
	if got["http.request.method"] != "POST" || got["http.request.header.authorization"] != "secret" || got["http.request.query.who"] != "x" {
		t.Fatalf("props = %v", got)
	}
}
