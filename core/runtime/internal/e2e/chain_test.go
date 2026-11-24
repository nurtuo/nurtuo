// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
)

type backend struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	calls  []string
}

func newBackend(t *testing.T) *backend {
	b := &backend{status: 200}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls = append(b.calls, r.Method+" "+r.URL.Path)
		st := b.status
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = io.WriteString(w, `{"id":"42","name":"Ada Lovelace"}`)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *backend) set(status int) { b.mu.Lock(); b.status = status; b.mu.Unlock() }
func (b *backend) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func post(t *testing.T, addr string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Post("http://"+addr+"/employee", "application/json", strings.NewReader(`{"q":1}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func refused(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = c.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), "actively")
}

func waitRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if refused(addr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener %s still accepting connections", addr)
}

func TestChain(t *testing.T) {
	for _, mode := range []struct {
		name   string
		signed bool
	}{{"json-bundle", false}, {"signed-bundle", true}} {
		t.Run(mode.name, func(t *testing.T) {
			be := newBackend(t)
			e := startRuntime(t, be.URL, mode.signed)
			n := 0
			put := func(flows ...string) string {
				n++
				if mode.signed {
					return e.putSigned(t, nil, flows...)
				}
				return e.putJSON(t, "d"+string(rune('0'+n)), flows...)
			}

			// 1-3: desired state -> deployment running -> request goes through the connector.
			good := put(employeeFlow)
			e.gw.setDesired(1, dep("dep-1", good))
			e.gw.statusFor(t, "dep-1", "running")
			code, body := post(t, e.httpAddr)
			if code != 200 || !strings.Contains(body, `"name":"Ada Lovelace"`) {
				t.Fatalf("got %d %q; backend saw %v", code, body, be.seen())
			}
			if got := be.seen(); len(got) != 1 || got[0] != "GET /employees/42" {
				t.Fatalf("backend calls = %v, want [GET /employees/42]", got)
			}

			// 5a: backend 500 -> listener 500.
			be.set(500)
			if code, _ := post(t, e.httpAddr); code != 500 {
				t.Fatalf("backend 500 -> listener %d", code)
			}
			be.set(200)

			// 5b: pause via control-plane command -> 503; resume -> 200.
			if r := e.gw.command("c1", "pause_listener", map[string]string{"listener": "dep-1/employee"}); !r.Ok {
				t.Fatalf("pause: %s", r.Message)
			}
			if code, _ := post(t, e.httpAddr); code != 503 {
				t.Fatalf("paused listener -> %d, want 503", code)
			}
			if r := e.gw.command("c2", "pause_listener", map[string]string{"listener": "nope/x"}); r.Ok {
				t.Fatal("pausing an unknown listener must fail")
			}
			if r := e.gw.command("c3", "resume_listener", map[string]string{"listener": "dep-1/employee"}); !r.Ok {
				t.Fatalf("resume: %s", r.Message)
			}
			if code, _ := post(t, e.httpAddr); code != 200 {
				t.Fatalf("resumed listener -> %d, want 200", code)
			}

			// 5c: an invalid flow in a bundle reports an error status; the healthy deployment keeps serving.
			bad := put(`{"schema":"nurtuo.flow/v1","id":"bad"}`)
			e.gw.setDesired(2, dep("dep-1", good), dep("dep-bad", bad))
			if st := e.gw.statusFor(t, "dep-bad", "error"); st.Message == "" {
				t.Fatal("error status without message")
			}
			if code, _ := post(t, e.httpAddr); code != 200 {
				t.Fatalf("healthy deployment disturbed by failed sibling: %d", code)
			}

			// 4: removing every deployment stops the listener.
			e.gw.setDesired(3)
			waitRefused(t, e.httpAddr)
		})
	}
}

func TestSignedBundleRejections(t *testing.T) {
	be := newBackend(t)
	e := startRuntime(t, be.URL, true)

	// Valid signature from a key that is not trusted.
	_, other, err := bundle.GenerateKey()
	must(t, err)
	untrusted := e.putSigned(t, other, employeeFlow)
	wrongKey, err := os.ReadFile(filepath.Join(e.bundles, strings.TrimPrefix(untrusted, "sha256:")+".bundle"))
	must(t, err)
	e.gw.serveArtifact(untrusted, wrongKey)
	e.gw.setDesired(1, dep("untrusted", untrusted))
	if st := e.gw.statusFor(t, "untrusted", "error"); !strings.Contains(st.Message, "rejected") {
		t.Fatalf("message %q", st.Message)
	}

	// Content that does not match the digest (tampered after signing).
	d := e.putSigned(t, nil, employeeFlow)
	p := filepath.Join(e.bundles, strings.TrimPrefix(d, "sha256:")+".bundle")
	raw, err := os.ReadFile(p)
	must(t, err)
	raw[len(raw)/2] ^= 0xff
	must(t, os.WriteFile(p, raw, 0o600))
	e.gw.serveArtifact(d, raw)
	e.gw.setDesired(2, dep("tampered", d))
	if st := e.gw.statusFor(t, "tampered", "error"); !strings.Contains(st.Message, "fetched artifact does not match digest") {
		t.Fatalf("message %q", st.Message)
	}

	// Tampered AND re-addressed under its own hash: the signature check must still reject it.
	d2 := e.writeBundle(t, raw)
	e.gw.serveArtifact(d2, raw)
	e.gw.setDesired(3, dep("readdressed", d2))
	if st := e.gw.statusFor(t, "readdressed", "error"); !strings.Contains(st.Message, "rejected") {
		t.Fatalf("message %q", st.Message)
	}

	// A plain JSON bundle is never accepted in trusted_keys mode.
	must(t, os.WriteFile(filepath.Join(e.bundles, strings.Repeat("a", 64)+".json"), []byte(employeeFlow), 0o600))
	e.gw.setDesired(4, dep("plain", "sha256:"+strings.Repeat("a", 64)))
	if st := e.gw.statusFor(t, "plain", "error"); !strings.Contains(st.Message, "control plane does not implement FetchArtifact") {
		t.Fatalf("message %q", st.Message)
	}

	if !refused(e.httpAddr) {
		t.Fatal("a rejected bundle started a listener")
	}
}
