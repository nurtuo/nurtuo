// SPDX-License-Identifier: AGPL-3.0-only

// Package interop proves the real Go runtime (runtime.RunWith) talks to the real TypeScript
// control-plane gateway: the TS side runs as a node child process (REST API + mTLS gateway over an
// in-memory repository), the Go side is driven exactly as in production, and the test steers the
// control plane over its REST API. Tests skip when node or the built control-plane dist is missing.
package interop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	httpconn "github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	rt "github.com/nurtuo/nurtuo/core/runtime/internal/runtime"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- paths ----

func here() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Dir(f)
}

func controlPlaneDir() string { return filepath.Join(here(), "..", "..", "..", "control-plane") }
func certsDir() string        { return filepath.Join(controlPlaneDir(), "testdata", "certs") }

// ---- TS control plane child ----

type controlPlane struct {
	cmd         *exec.Cmd
	restPort    int
	gatewayPort int
	stderr      *syncBuf
	done        chan struct{}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("interop: node not found on PATH; install Node 20+ to run the cross-language test")
	}
	if _, err := os.Stat(filepath.Join(controlPlaneDir(), "dist", "index.js")); err != nil {
		t.Skip("interop: control-plane dist missing; run `pnpm --filter @nurtuo/control-plane... build` first")
	}
	return node
}

// startControlPlane launches the TS child; ports 0 pick random ones. It is killed on cleanup
// (or earlier by stop).
func startControlPlane(t *testing.T, restPort, gatewayPort int, extraEnv ...string) *controlPlane {
	t.Helper()
	node := requireNode(t)
	cmd := exec.Command(node, filepath.Join(here(), "testdata", "run-gateway.mjs"))
	cmd.Env = append(os.Environ(), "CERT_DIR="+certsDir(), fmt.Sprintf("REST_PORT=%d", restPort), fmt.Sprintf("GATEWAY_PORT=%d", gatewayPort))
	cmd.Env = append(cmd.Env, extraEnv...)
	cp := &controlPlane{cmd: cmd, stderr: &syncBuf{}, done: make(chan struct{})}
	cmd.Stderr = cp.stderr
	out, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	go func() { _ = cmd.Wait(); close(cp.done) }()
	t.Cleanup(cp.stop)

	line := make(chan string, 1)
	go func() {
		r := bufio.NewReader(out)
		s, _ := r.ReadString('\n')
		line <- s
		_, _ = io.Copy(io.Discard, r)
	}()
	select {
	case s := <-line:
		var ports struct{ RestPort, GatewayPort int }
		if err := json.Unmarshal([]byte(s), &ports); err != nil {
			t.Fatalf("control plane did not print ports (%q): %v\nstderr: %s", s, err, cp.stderr)
		}
		cp.restPort, cp.gatewayPort = ports.RestPort, ports.GatewayPort
	case <-time.After(20 * time.Second):
		t.Fatalf("control plane did not start\nstderr: %s", cp.stderr)
	}
	return cp
}

func (cp *controlPlane) stop() {
	_ = cp.cmd.Process.Kill()
	select {
	case <-cp.done:
	case <-time.After(5 * time.Second):
	}
}

func (cp *controlPlane) gatewayAddr() string { return fmt.Sprintf("127.0.0.1:%d", cp.gatewayPort) }

// call performs a REST request and decodes the JSON response into a generic map.
func (cp *controlPlane) call(t testing.TB, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", cp.restPort, path), rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func (cp *controlPlane) mustCall(t testing.TB, method, path string, body any, want int) map[string]any {
	t.Helper()
	code, m := cp.call(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s -> %d %v (want %d)", method, path, code, m, want)
	}
	return m
}

func (cp *controlPlane) registerRuntime(t testing.TB, id string) {
	cp.mustCall(t, "POST", "/v1/runtimes", map[string]any{"id": id, "name": id, "environment": "dev"}, 200)
}

// component creates a flow component with one revision. digest/"" lets the control plane derive it.
func (cp *controlPlane) component(t testing.TB, name string, content json.RawMessage, digest string) (componentID, revDigest string) {
	c := cp.mustCall(t, "POST", "/v1/components", map[string]any{"name": name, "kind": "flow"}, 201)
	id := c["id"].(string)
	body := map[string]any{"content": content}
	if digest != "" {
		body["digest"] = digest
	}
	r := cp.mustCall(t, "POST", "/v1/components/"+id+"/revisions", body, 201)
	return id, r["digest"].(string)
}

func (cp *controlPlane) deploy(t testing.TB, componentID string, revision int, runtimeID string) (deploymentID string) {
	d := cp.mustCall(t, "POST", "/v1/deployments", map[string]any{"componentId": componentID, "revision": revision, "runtimeId": runtimeID}, 202)
	return d["deployment"].(map[string]any)["id"].(string)
}

func (cp *controlPlane) deploymentStatus(t testing.TB, id string) (string, string) {
	m := cp.mustCall(t, "GET", "/v1/deployments/"+id, nil, 200)
	msg, _ := m["message"].(string)
	return m["status"].(string), msg
}

func (cp *controlPlane) waitStatus(t testing.TB, id, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var st, msg string
	for time.Now().Before(deadline) {
		st, msg = cp.deploymentStatus(t, id)
		if st == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("deployment %s status = %q (%s), want %q\ncontrol plane stderr: %s", id, st, msg, want, cp.stderr)
}

func eventually(t testing.TB, what string, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// ---- backend + Go runtime ----

type backend struct {
	*httptest.Server
}

func newBackend(t *testing.T) *backend {
	b := &backend{httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"42","name":"Ada Lovelace"}`)
	}))}
	t.Cleanup(b.Close)
	return b
}

func freeAddr(t testing.TB) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

type runtimeProc struct {
	cfg      rt.Config
	httpAddr string
	bundles  string
	cancel   context.CancelFunc
	done     chan error
	logs     *syncBuf
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
}

// startRuntime runs the real runtime.RunWith with the given client cert (runtimeID/cert name
// decoupled so tests can present rt-2's cert while claiming rt-1). signed selects trusted_keys mode.
func startRuntime(t *testing.T, cp *controlPlane, runtimeID, certName, backendURL string, signed bool, sharedBundles ...string) *runtimeProc {
	t.Helper()
	dir := t.TempDir()
	p := &runtimeProc{bundles: filepath.Join(dir, "bundles"), httpAddr: freeAddr(t), done: make(chan error, 1), logs: &syncBuf{}}
	if len(sharedBundles) > 0 {
		p.bundles = sharedBundles[0]
	}
	must(t, os.MkdirAll(p.bundles, 0o755))
	cfg := rt.Config{RuntimeID: runtimeID, ControlPlaneURL: cp.gatewayAddr(),
		CertFile: filepath.Join(certsDir(), certName+".pem"), KeyFile: filepath.Join(certsDir(), certName+".key"),
		CAFile: filepath.Join(certsDir(), "ca.pem"), ServerName: "localhost", BundleDir: p.bundles, HTTPAddr: p.httpAddr}
	if signed {
		var err error
		p.pub, p.priv, err = bundle.GenerateKey()
		must(t, err)
		cfg.TrustedKeys = []string{bundle.EncodePublicKey(p.pub)}
	}
	must(t, cfg.Validate())
	p.cfg = cfg

	eng := engine.New()
	conns := engine.NewConnectors()
	settings, _ := structpb.NewStruct(map[string]any{"baseUrl": backendURL, "retries": float64(0), "timeoutMs": float64(5000)})
	opts, _ := structpb.NewStruct(map[string]any{"path": "/employees/42"})
	conns.Register("http", engine.ConnectorEntry{Connector: httpconn.New(), Connection: &connectorv1.ConnectionConfig{Settings: settings}, Options: opts})
	eng.Registry.RegisterConnectorRunner(conns)

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	logger := slog.New(slog.NewTextHandler(p.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() { p.done <- rt.RunWith(ctx, cfg, rt.Options{Engine: eng, Logger: logger}) }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *runtimeProc) stop(t testing.TB) {
	p.cancel()
	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("RunWith returned %v", err)
		}
		p.done <- nil
	case <-time.After(15 * time.Second):
		t.Error("runtime did not stop")
	}
}

// putPlain stores {"flows":[doc]} under <digest hex>.json (the plain bundle_dir format).
func (p *runtimeProc) putPlain(t testing.TB, digest string, flow json.RawMessage) {
	b, _ := json.Marshal(map[string]any{"flows": []json.RawMessage{flow}})
	must(t, os.WriteFile(filepath.Join(p.bundles, strings.TrimPrefix(digest, "sha256:")+".json"), b, 0o600))
}

// putSigned packs and signs flow with the runtime's trusted key, stores it as <sha256>.bundle and
// returns the artifact digest.
func (p *runtimeProc) putSigned(t testing.TB, flowID string, flow json.RawMessage) string {
	data, err := bundle.Pack(bundle.Input{Name: "interop", CreatedAt: time.Unix(1700000000, 0),
		Components: []bundle.Component{{Kind: "flow", ID: flowID, Revision: 1, Document: flow}}}, p.priv)
	must(t, err)
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	must(t, os.WriteFile(filepath.Join(p.bundles, h+".bundle"), data, 0o600))
	return "sha256:" + h
}

func (p *runtimeProc) post(t testing.TB) (int, string) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Post("http://"+p.httpAddr+"/employee", "application/json", strings.NewReader(`{"q":1}`))
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (p *runtimeProc) waitServing(t testing.TB) {
	t.Helper()
	eventually(t, "listener to serve the backend body", 15*time.Second, func() bool {
		code, body := p.post(t)
		return code == 200 && strings.Contains(body, `"name":"Ada Lovelace"`)
	})
}

func (p *runtimeProc) waitRefused(t testing.TB) {
	t.Helper()
	eventually(t, "listener to stop", 15*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", p.httpAddr, time.Second)
		if err == nil {
			_ = c.Close()
			return false
		}
		return true
	})
}

// flow builds the e2e employee flow (HTTP listener -> http connector -> Return) with a name so
// different revisions get different digests.
func flow(name string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"schema":"nurtuo.flow/v1","id":"employee","name":%q,"revision":1,
"steps":[{"id":"start","kind":"start","config":{"type":"listener","connector":"http","path":"/employee","method":"POST"}},
{"id":"call","kind":"connector","config":{"connector":"http","operation":"GET"}},
{"id":"return","kind":"returnDocuments","config":{}}],
"edges":{"start:default":"call","call:default":"return"}}`, name))
}
