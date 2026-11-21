// SPDX-License-Identifier: AGPL-3.0-only

package listener_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/listener"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

func mk(t *testing.T, steps []string, edges map[string]string) *flow.Flow {
	t.Helper()
	var ss, es []string
	for _, s := range steps {
		p := strings.SplitN(s, ":", 3)
		ss = append(ss, fmt.Sprintf(`{"id":%q,"kind":%q,"config":%s}`, p[0], p[1], p[2]))
	}
	for k, v := range edges {
		es = append(es, fmt.Sprintf("%q:%q", k, v))
	}
	raw := fmt.Sprintf(`{"schema":"nurtuo.flow/v1","id":"f","name":"N","revision":1,"steps":[%s],"edges":{%s}}`,
		strings.Join(ss, ","), strings.Join(es, ","))
	f, diags, err := flow.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err, diags)
	}
	return f
}

const startHTTP = `{"type":"listener","connector":"http","path":"/hook","method":"POST"}`

func engineWithStart() *engine.Engine {
	e := engine.New()
	listener.Register(e.Registry)
	return e
}

func startHTTPListener(t *testing.T, f *flow.Flow, cfg listener.HTTPConfig) (listener.Listener, string) {
	t.Helper()
	cfg.Path = "/hook"
	l, err := listener.NewHTTP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Start(context.Background(), listener.FlowHandler(engineWithStart(), f)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Stop(context.Background()) })
	return l, "http://" + l.Status().Addr + "/hook"
}

func post(t *testing.T, url, body string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Post(url, "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestHTTPEchoWithProps(t *testing.T) {
	f := mk(t, []string{
		"s:start:" + startHTTP,
		"m:message:" + `{"template":"got {{doc.props.http.request.query.who}} via {{doc.props.http.request.method}}"}`,
		"p:setProperties:" + `{"properties":[{"name":"http.status","value":"201"},{"name":"http.response.header.X-Flow","value":"yes"}]}`,
		"r:returnDocuments:{}",
	}, map[string]string{"s:default": "m", "m:default": "p", "p:default": "r"})
	_, url := startHTTPListener(t, f, listener.HTTPConfig{})
	code, body, h := post(t, url+"?who=ann", "ignored")
	if code != 201 || body != "got ann via POST" || h.Get("X-Flow") != "yes" {
		t.Fatalf("got %d %q %v", code, body, h)
	}
}

func TestHTTPRequestBodyBecomesDocument(t *testing.T) {
	f := mk(t, []string{"s:start:" + startHTTP, "r:returnDocuments:{}"}, map[string]string{"s:default": "r"})
	_, url := startHTTPListener(t, f, listener.HTTPConfig{})
	code, body, _ := post(t, url, `{"a":1}`)
	if code != 200 || body != `{"a":1}` {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestHTTPFlowFailureAndStop(t *testing.T) {
	bad := mk(t, []string{
		"s:start:" + startHTTP, "m:message:" + `{"template":"{{bogus("}`, "r:returnDocuments:{}",
	}, map[string]string{"s:default": "m", "m:default": "r"})
	_, url := startHTTPListener(t, bad, listener.HTTPConfig{})
	if code, _, _ := post(t, url, "x"); code != 500 {
		t.Fatalf("failure: got %d want 500", code)
	}
	stop := mk(t, []string{"s:start:" + startHTTP, "x:stop:" + `{"status":418,"message":"teapot"}`},
		map[string]string{"s:default": "x"})
	_, url2 := startHTTPListener(t, stop, listener.HTTPConfig{})
	if code, body, _ := post(t, url2, "x"); code != 418 || !strings.Contains(body, "teapot") {
		t.Fatalf("stop: got %d %q", code, body)
	}
}

func TestHTTPLimitsMethodPause(t *testing.T) {
	f := mk(t, []string{"s:start:" + startHTTP, "r:returnDocuments:{}"}, map[string]string{"s:default": "r"})
	l, url := startHTTPListener(t, f, listener.HTTPConfig{MaxBodyBytes: 8})
	if code, _, _ := post(t, url, "0123456789"); code != 413 {
		t.Fatalf("oversize: got %d", code)
	}
	if resp, err := http.Get(url); err != nil || resp.StatusCode != 405 {
		t.Fatalf("method: %v %v", resp, err)
	}
	if err := l.Pause(); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := post(t, url, "ok"); code != 503 || l.Status().State != listener.StatePaused {
		t.Fatalf("paused: got %d state %s", code, l.Status().State)
	}
	if err := l.Resume(); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := post(t, url, "ok"); code != 200 {
		t.Fatalf("resumed: got %d", code)
	}
}

func TestHTTPGracefulShutdownWaitsForInflight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	l, err := listener.NewHTTP(listener.HTTPConfig{Path: "/hook"})
	if err != nil {
		t.Fatal(err)
	}
	h := func(_ context.Context, in *docs.Set) (*engine.Result, error) {
		close(started)
		<-release
		return &engine.Result{Returned: in, Failed: docs.NewSet()}, nil
	}
	if err := l.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	url := "http://" + l.Status().Addr + "/hook"
	res := make(chan int, 1)
	go func() { c, _, _ := post(t, url, "x"); res <- c }()
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- l.Stop(context.Background()) }()
	select {
	case serr := <-stopped:
		t.Fatalf("Stop returned (err=%v) before in-flight request finished", serr)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if c := <-res; c != 200 {
		t.Fatalf("in-flight got %d", c)
	}
	if l.Status().State != listener.StateStopped {
		t.Fatal("not stopped")
	}
}

func TestManagerReconcile(t *testing.T) {
	var starts atomic.Int32
	f := mk(t, []string{"s:start:" + startHTTP, "r:returnDocuments:{}"}, map[string]string{"s:default": "r"})
	factory := func() (listener.Listener, listener.Handler, error) {
		starts.Add(1)
		l, err := listener.NewHTTP(listener.HTTPConfig{Path: "/hook"})
		return l, listener.FlowHandler(engineWithStart(), f), err
	}
	m := listener.NewManager()
	ctx := context.Background()
	t.Cleanup(func() { _ = m.Shutdown(ctx) })
	d := listener.Desired{Key: "f/1", Spec: "a", Factory: factory}
	if err := m.Apply(ctx, []listener.Desired{d}); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(ctx, []listener.Desired{d}); err != nil || starts.Load() != 1 {
		t.Fatalf("idempotent apply: starts=%d err=%v", starts.Load(), err)
	}
	d.Paused = true
	_ = m.Apply(ctx, []listener.Desired{d})
	if m.Status()["f/1"].State != listener.StatePaused {
		t.Fatal("want paused")
	}
	d.Spec, d.Paused = "b", false
	_ = m.Apply(ctx, []listener.Desired{d})
	if starts.Load() != 2 || m.Status()["f/1"].State != listener.StateRunning {
		t.Fatalf("restart: starts=%d", starts.Load())
	}
	if err := m.Apply(ctx, nil); err != nil || len(m.Status()) != 0 {
		t.Fatalf("remove: %v %v", err, m.Status())
	}
}

type chanSource struct{ ch chan *docs.Set }

func (s chanSource) Consume(ctx context.Context, deliver func(context.Context, *docs.Set) error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case in := <-s.ch:
			_ = deliver(ctx, in)
		}
	}
}

func TestSourceListenerPauseResume(t *testing.T) {
	var handled atomic.Int32
	h := func(_ context.Context, in *docs.Set) (*engine.Result, error) {
		handled.Add(1)
		return &engine.Result{Returned: in, Failed: docs.NewSet()}, nil
	}
	src := chanSource{ch: make(chan *docs.Set, 4)}
	l := listener.NewSourceListener(src)
	if err := l.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Stop(context.Background()) }()
	_ = l.Pause()
	src.ch <- docs.NewSet(docs.New("1", nil, nil))
	time.Sleep(50 * time.Millisecond)
	if handled.Load() != 0 {
		t.Fatal("delivered while paused")
	}
	_ = l.Resume()
	deadline := time.Now().Add(2 * time.Second)
	for handled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if handled.Load() != 1 || l.Status().Received != 1 {
		t.Fatalf("handled=%d", handled.Load())
	}
}
