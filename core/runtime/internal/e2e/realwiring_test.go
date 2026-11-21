// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/runtime"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
)

// The secret the bundle references as secret://erp-token. It only ever exists in the runtime's
// environment (the env provider) and in the backend.
const bearer = "s3cr3t-BEARER-token-0123456789"

// authBackend answers 200 only for the right bearer token (401 otherwise) and records what it saw.
type authBackend struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
	auths []string
}

func newAuthBackend(t *testing.T, want string) *authBackend {
	b := &authBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls = append(b.calls, r.Method+" "+r.URL.Path)
		b.auths = append(b.auths, r.Header.Get("Authorization"))
		b.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"42","name":"Ada Lovelace"}`)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *authBackend) snapshot() (calls, auths []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...), append([]string(nil), b.auths...)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// connFlow is employeeFlow plus a setProperties step that picks the request path and a connector
// step bound to the given connection ("" = the bundle's default connection for http).
func connFlow(id, route, connection string) string {
	conn := ""
	if connection != "" {
		conn = fmt.Sprintf(`,"connection":%q`, connection)
	}
	return fmt.Sprintf(`{"schema":"nurtuo.flow/v1","id":%q,"name":%q,"revision":1,
"steps":[{"id":"start","kind":"start","config":{"type":"listener","connector":"http","path":%q,"method":"POST"}},
{"id":"path","kind":"setProperties","config":{"properties":[{"name":"http.path","value":"/employees/42"}]}},
{"id":"call","kind":"connector","config":{"connector":"http","operation":"GET"%s}},
{"id":"return","kind":"returnDocuments","config":{}}],
"edges":{"start:default":"path","path:default":"call","call:default":"return"}}`, id, id, route, conn)
}

// connDoc is a connection component; the token is only a secret:// reference.
func connDoc(id, baseURL, tokenRef string) string {
	return fmt.Sprintf(`{"schema":"nurtuo.connection/v1","id":%q,"connector":"http","settings":{"baseUrl":%q,"retries":0,"timeoutMs":5000,"testPath":"/health","auth":{"type":"bearer","token":%q}}}`,
		id, baseURL, tokenRef)
}

// putWith stores flows and connections as a bundle (signed archive or plain JSON) and returns the digest.
func (e *env) putWith(t *testing.T, signed bool, name string, conns, flows []string) string {
	t.Helper()
	if !signed {
		var fr, cr []json.RawMessage
		for _, f := range flows {
			fr = append(fr, json.RawMessage(f))
		}
		for _, c := range conns {
			cr = append(cr, json.RawMessage(c))
		}
		b, _ := json.Marshal(map[string]any{"flows": fr, "connections": cr})
		must(t, os.WriteFile(filepath.Join(e.bundles, name+".json"), b, 0o600))
		return "sha256:" + name
	}
	var comps []bundle.Component
	idOf := func(doc string) string {
		var h struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(doc), &h)
		return h.ID
	}
	for _, f := range flows {
		comps = append(comps, bundle.Component{Kind: "flow", ID: idOf(f), Revision: 1, Document: json.RawMessage(f)})
	}
	for _, c := range conns {
		comps = append(comps, bundle.Component{Kind: "connection", ID: idOf(c), Revision: 1, Document: json.RawMessage(c)})
	}
	data, err := bundle.Pack(bundle.Input{Name: name, CreatedAt: time.Unix(1700000000, 0), Components: comps}, e.priv)
	must(t, err)
	sum := sha256.Sum256(data)
	hexd := hex.EncodeToString(sum[:])
	must(t, os.WriteFile(filepath.Join(e.bundles, hexd+".bundle"), data, 0o600))
	return "sha256:" + hexd
}

// TestRealWiringHTTPConnector runs the production wiring (no injected engine): the bundle carries
// a flow and a connection component, the bearer token comes from the env secret provider.
func TestRealWiringHTTPConnector(t *testing.T) {
	for _, mode := range []struct {
		name   string
		signed bool
	}{{"json-bundle", false}, {"signed-bundle", true}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("NURTUO_SECRET_ERP_TOKEN", bearer)
			t.Setenv("NURTUO_SECRET_WRONG_TOKEN", "some-other-token-value")
			be := newAuthBackend(t, bearer)
			logs := &syncBuf{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			e := boot(t, mode.signed, runtime.Options{Logger: logger}, nil)
			n := 0
			put := func(conns, flows []string) string {
				n++
				return e.putWith(t, mode.signed, fmt.Sprintf("b%d", n), conns, flows)
			}

			// A valid deployment: connection resolved, Test()ed at deploy time, status reports it.
			good := put([]string{connDoc("erp-api", be.URL, "secret://erp-token")}, []string{connFlow("employee", "/employee", "erp-api")})
			e.gw.setDesired(1, dep("dep-1", good))
			st := e.gw.statusFor(t, "dep-1", "running")
			if !strings.Contains(st.Message, "connections: erp-api ok") {
				t.Fatalf("running status message %q lacks the connection test result", st.Message)
			}
			code, body := post(t, e.httpAddr)
			if code != 200 || !strings.Contains(body, `"name":"Ada Lovelace"`) {
				t.Fatalf("got %d %q", code, body)
			}
			calls, auths := be.snapshot()
			if len(calls) != 2 || calls[0] != "GET /health" || calls[1] != "GET /employees/42" {
				t.Fatalf("backend calls = %v (want the deploy-time test, then the flow request)", calls)
			}
			for i, a := range auths {
				if a != "Bearer "+bearer {
					t.Fatalf("call %d: Authorization = %q, want the secret from the env provider", i, a)
				}
			}

			// Missing secret: deployment fails with a clear message naming connection and secret.
			noSecret := put([]string{connDoc("erp-api", be.URL, "secret://does-not-exist")}, []string{connFlow("other", "/other", "erp-api")})
			e.gw.setDesired(2, dep("dep-1", good), dep("dep-nosecret", noSecret))
			st = e.gw.statusFor(t, "dep-nosecret", "error")
			for _, want := range []string{`"does-not-exist"`, `"erp-api"`, "not found"} {
				if !strings.Contains(st.Message, want) {
					t.Fatalf("missing-secret message %q lacks %q", st.Message, want)
				}
			}

			// Missing connection definition in the bundle.
			noConn := put(nil, []string{connFlow("third", "/third", "ghost")})
			e.gw.setDesired(3, dep("dep-1", good), dep("dep-noconn", noConn))
			if st = e.gw.statusFor(t, "dep-noconn", "error"); !strings.Contains(st.Message, `"ghost"`) || !strings.Contains(st.Message, "not defined") {
				t.Fatalf("missing-connection message %q", st.Message)
			}

			// Connection Test fails (backend rejects the token): strict mode refuses the deployment.
			badAuth := put([]string{connDoc("erp-api", be.URL, "secret://wrong-token")}, []string{connFlow("fourth", "/fourth", "erp-api")})
			e.gw.setDesired(4, dep("dep-1", good), dep("dep-badauth", badAuth))
			if st = e.gw.statusFor(t, "dep-badauth", "error"); !strings.Contains(st.Message, "test failed") || !strings.Contains(st.Message, "authentication rejected") {
				t.Fatalf("failed-test message %q", st.Message)
			}

			// A literal credential in the bundle is refused (and never echoed).
			lit := put([]string{connDoc("erp-api", be.URL, "plain-literal-token-AAAA")}, []string{connFlow("fifth", "/fifth", "erp-api")})
			e.gw.setDesired(5, dep("dep-1", good), dep("dep-literal", lit))
			st = e.gw.statusFor(t, "dep-literal", "error")
			if !strings.Contains(st.Message, "auth.token") || strings.Contains(st.Message, "plain-literal-token-AAAA") {
				t.Fatalf("literal-secret message %q", st.Message)
			}

			// The healthy deployment kept serving through all of that.
			if code, _ := post(t, e.httpAddr); code != 200 {
				t.Fatalf("healthy deployment disturbed: %d", code)
			}

			// Pause is reported back as a deployment state, resume as running again.
			if r := e.gw.command("c1", "pause_listener", map[string]string{"listener": "dep-1/employee"}); !r.Ok {
				t.Fatal(r.Message)
			}
			if st = e.gw.statusFor(t, "dep-1", "paused"); !strings.Contains(st.Message, "dep-1/employee") {
				t.Fatalf("paused message %q", st.Message)
			}
			if r := e.gw.command("c2", "resume_listener", map[string]string{"listener": "dep-1/employee"}); !r.Ok {
				t.Fatal(r.Message)
			}
			e.gw.statusFor(t, "dep-1", "running")

			// Removing the deployment stops the listener.
			e.gw.setDesired(6)
			waitRefused(t, e.httpAddr)

			// The token must not appear in any log line or in anything reported to the control plane.
			for _, v := range []string{bearer, "some-other-token-value", "plain-literal-token-AAAA"} {
				if strings.Contains(logs.String(), v) {
					t.Fatalf("secret %q leaked into the logs:\n%s", v, logs.String())
				}
				if strings.Contains(e.gw.sent(), v) {
					t.Fatalf("secret %q leaked into status messages:\n%s", v, e.gw.sent())
				}
			}
			if len(logs.String()) == 0 {
				t.Fatal("expected some log output (the leak check would be vacuous)")
			}
		})
	}
}

// TestRealWiringTestModeWarnAndEncryptedFileSecrets covers the other config keys end to end:
// secrets.provider=encrypted-file and connection_test=warn (a rejected token still deploys).
func TestRealWiringTestModeWarnAndEncryptedFileSecrets(t *testing.T) {
	be := newAuthBackend(t, "the-right-token")
	dir := t.TempDir()
	e := boot(t, false, runtime.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, func(c *runtime.Config) {
		c.Secrets.Provider = "encrypted-file"
		c.Secrets.Path = dir
		c.ConnectionTest = "warn"
	})
	// Store the secret the way `nurtuo secret set` does (a different process in real life).
	store, err := secrets.Open(secrets.Config{Provider: "encrypted-file", Path: dir})
	must(t, err)
	must(t, store.Set(context.Background(), "erp-token", secrets.New("a-wrong-token-value")))

	d := e.putWith(t, false, "w1", []string{connDoc("erp-api", be.URL, "secret://erp-token")}, []string{connFlow("employee", "/employee", "")})
	e.gw.setDesired(1, dep("dep-w", d))
	st := e.gw.statusFor(t, "dep-w", "running")
	if !strings.Contains(st.Message, "warning") || !strings.Contains(st.Message, "authentication rejected") {
		t.Fatalf("warn-mode message %q", st.Message)
	}
	if strings.Contains(st.Message, "a-wrong-token-value") {
		t.Fatalf("token in status: %q", st.Message)
	}
}
