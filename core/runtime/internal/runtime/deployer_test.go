// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/listener"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/memory"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
)

func flowJSON(id, start string) string {
	return `{"schema":"nurtuo.flow/v1","id":"` + id + `","name":"N","revision":1,` +
		`"steps":[{"id":"s","kind":"start","config":` + start + `},{"id":"r","kind":"returnDocuments","config":{}}],` +
		`"edges":{"s:default":"r"}}`
}

func TestEngineDeployerStartsHTTPAndQueueListeners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bundle := `{"flows":[` +
		flowJSON("web", `{"type":"listener","connector":"http","path":"/hook","method":"POST"}`) + `,` +
		flowJSON("jobs", `{"type":"queue","queue":"jobs"}`) + `]}`
	qb := memory.New()
	defer func() { _ = qb.Close() }()
	d := NewEngineDeployer(nil, qb, func(_ context.Context, digest string) ([]byte, error) {
		if digest != "sha256:abc" {
			t.Errorf("digest = %q", digest)
		}
		return []byte(bundle), nil
	})
	defer func() { _ = d.Shutdown(ctx) }()

	if err := d.Apply(ctx, supervisor.Deployment{ID: "dep1", BundleDigest: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	if act, _ := d.Actual(ctx); len(act) != 1 {
		t.Fatalf("actual = %v", act)
	}

	web, ok := d.Manager().Get("dep1/web")
	if !ok {
		t.Fatal("http listener not started")
	}
	resp, err := http.Post("http://"+web.Status().Addr+"/hook", "text/plain", strings.NewReader("hi"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hi" {
		t.Fatalf("http: %d %q", resp.StatusCode, body)
	}

	if err := qb.Publish(ctx, "jobs", queue.Message{ID: "j1", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	jobs, ok := d.Manager().Get("dep1/jobs")
	if !ok {
		t.Fatal("queue listener not started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for jobs.Status().Received < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := jobs.Status(); st.Received != 1 || st.Failed != 0 {
		t.Fatalf("queue status = %+v", st)
	}

	if err := d.Remove(ctx, "dep1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Manager().Get("dep1/web"); ok {
		t.Fatal("listener survived Remove")
	}
	if act, _ := d.Actual(ctx); len(act) != 0 {
		t.Fatalf("actual after remove = %v", act)
	}
}

func TestEngineDeployerRejectsInvalidBundle(t *testing.T) {
	d := NewEngineDeployer(nil, memory.New(), func(context.Context, string) ([]byte, error) {
		return []byte(`{"flows":[{"schema":"nurtuo.flow/v1"}]}`), nil
	})
	if err := d.Apply(context.Background(), supervisor.Deployment{ID: "x", BundleDigest: "d"}); err == nil {
		t.Fatal("want error for invalid flow")
	}
	if act, _ := d.Actual(context.Background()); len(act) != 0 {
		t.Fatalf("failed deployment must not be recorded: %v", act)
	}
}

var _ = listener.StartQueue
