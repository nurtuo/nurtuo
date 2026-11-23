// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// staticDB is a Database that is already running: DSN "" makes the control plane use its
// in-memory repository, a postgres:// DSN an external server.
type staticDB struct{ dsn string }

func (s staticDB) Start(context.Context) error { return nil }
func (s staticDB) DSN() string                 { return s.dsn }
func (s staticDB) Stop(context.Context) error  { return nil }

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..", "..")
}

// TestStandaloneE2EHandshakeAndDeploy runs the standalone stack as `nurtuo standalone` wires it
// (real bootstrapper, the real control plane main.js as a supervised node child with auth and
// the mTLS gateway, runtime registration, runtime.Run with the standalone client certificate),
// deploys a flow over the authenticated REST API and waits for the runtime to report it failed
// ("no bundle_dir configured") through the gateway session. It then restarts the stack on the same data directory and deploys
// again. Embedded PostgreSQL is replaced by the control plane's in-memory repository, or by an
// external server when NURTUO_TEST_POSTGRES_URL is set (the test seeds a fixed admin token so
// reruns against the same database can log in).
//
// runRealRuntime sets no BundleDir until FetchArtifact lands (#64), so the placeholder deployer
// refuses the deployment; the test proves session acceptance, desired-state delivery and honest
// status reporting, not that the flow's engine starts.
func TestStandaloneE2EHandshakeAndDeploy(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped with -short")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("e2e: node not found on PATH")
	}
	entry := filepath.Join(repoRoot(), defaultControlPlaneEntry)
	if _, err := os.Stat(entry); err != nil {
		t.Skip("e2e: control-plane dist missing; run `pnpm --filter @nurtuo/control-plane... build` first")
	}

	dirs := NewDataDirs(t.TempDir())
	if err := dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("NURTUO_TEST_POSTGRES_URL")
	if dsn != "" {
		// The bootstrap owner persists in the database; keep its password stable across reruns.
		if err := os.WriteFile(filepath.Join(dirs.Secrets, "admin-token"), []byte("nurtuo-standalone-e2e-admin-token-0123456789abcdef"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for run := 1; run <= 2; run++ {
		t.Run(fmt.Sprintf("start%d", run), func(t *testing.T) { standaloneE2ERun(t, node, entry, dirs, dsn) })
	}
}

func standaloneE2ERun(t *testing.T, node, entry string, dirs DataDirs, dsn string) {
	restPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	gwPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	gatewayAddr := "127.0.0.1:" + strconv.Itoa(int(gwPort))
	base := "http://127.0.0.1:" + strconv.Itoa(int(restPort)) + "/"
	// runStandalone bootstraps before building the stack too (the DB needs the password).
	creds, err := NewLocalBootstrapper().Bootstrap(dirs)
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuf{}
	stack := &Stack{
		Dirs: dirs, Bootstrap: NewLocalBootstrapper(), DB: staticDB{dsn}, CPURL: base, Log: log,
		HealthWait: 30 * time.Second,
		ControlPlane: func(dsn string, cr *Credentials) Service {
			return controlPlaneService{
				child: NewChild(ChildSpec{
					Name: "control-plane", Command: []string{node, entry}, Log: log, MaxRestarts: 2,
					Env: controlPlaneEnv(dsn, cr, int(restPort), gatewayAddr),
				}),
				probe: HTTPProbe(base),
			}
		},
		Register: registerRealRuntime,
		Runtime:  runRealRuntime(gatewayAddr),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- stack.Run(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("stack.Run: %v\nlog:\n%s", err, log)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("stack did not stop\nlog:\n%s", log)
		}
	}
	defer stop()

	hc := &http.Client{Timeout: 10 * time.Second}
	var jwt string
	ready := false
	for deadline := time.Now().Add(45 * time.Second); !ready && time.Now().Before(deadline); {
		// The runtime is registered before it starts; wait until the stack got that far.
		if jwt == "" {
			jwt, _ = standaloneLogin(ctx, hc, strings.TrimSuffix(base, "/"), standaloneAdminEmail, creds.AdminToken)
		}
		if jwt != "" {
			code, _, _ := apiCall(ctx, hc, http.MethodGet, base+"v1/runtimes/"+standaloneRuntimeID, jwt, nil)
			ready = code == http.StatusOK
		}
		if !ready {
			select {
			case err := <-done:
				stopped = true
				t.Fatalf("stack exited early: %v\nlog:\n%s", err, log)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if !ready {
		t.Fatalf("control plane login / runtime registration not ready (logged in: %v)\nlog:\n%s", jwt != "", log)
	}

	call := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		code, raw, err := apiCall(ctx, hc, method, base+path, jwt, body)
		if err != nil || code != want {
			t.Fatalf("%s %s -> %d %s (%v), want %d\nlog:\n%s", method, path, code, raw, err, want, log)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(raw), &m)
		return m
	}
	sfx := make([]byte, 4)
	_, _ = rand.Read(sfx)
	comp := call(http.MethodPost, "v1/components", map[string]any{"name": "e2e-" + hex.EncodeToString(sfx), "kind": "flow"}, http.StatusCreated)
	compID := comp["id"].(string)
	call(http.MethodPost, "v1/components/"+compID+"/revisions", map[string]any{"content": map[string]any{"name": "e2e", "steps": []any{}}}, http.StatusCreated)
	dep := call(http.MethodPost, "v1/deployments", map[string]any{"componentId": compID, "revision": 1, "runtimeId": standaloneRuntimeID}, http.StatusAccepted)
	depID := dep["deployment"].(map[string]any)["id"].(string)

	var status, message string
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		got := call(http.MethodGet, "v1/deployments/"+depID, nil, http.StatusOK)
		status, _ = got["status"].(string)
		message, _ = got["message"].(string)
		if status == "failed" || status == "running" {
			break
		}
	}
	if status != "failed" {
		t.Fatalf("deployment status %q, want failed (no bundle_dir; never running) (gateway handshake or desired-state delivery failed)\nlog:\n%s", status, log)
	}
	if !strings.Contains(message, "bundle_dir") {
		t.Fatalf("deployment message %q, want it to name bundle_dir\nlog:\n%s", message, log)
	}
	if strings.Contains(log.String(), "does not match certificate identity") {
		t.Fatalf("gateway refused the runtime identity\nlog:\n%s", log)
	}
	rt := call(http.MethodGet, "v1/runtimes/"+standaloneRuntimeID, nil, http.StatusOK)
	if rt["environment"] != standaloneEnvironment {
		t.Errorf("runtime = %v", rt)
	}
	_, _, _ = apiCall(ctx, hc, http.MethodPost, base+"v1/auth/logout", jwt, nil)
	stop()
}
