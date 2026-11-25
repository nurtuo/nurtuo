// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
// deploys through the authenticated REST API with an empty cache, then restarts the
// stack and confirms it runs from the same verified archive. The control plane uses
// its in-memory repository unless NURTUO_TEST_POSTGRES_URL points to an external server.
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

	var componentID, digest string
	t.Run("fetch", func(t *testing.T) {
		componentID = standaloneE2ERun(t, node, entry, dirs, dsn, "", "")
	})
	if t.Failed() {
		return
	}
	archives, err := filepath.Glob(filepath.Join(dirs.Bundles, "*.bundle"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("want one fetched bundle, got %d (glob error %v)", len(archives), err)
	}
	archive := archives[0]
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	digest = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	if filepath.Base(archive) != strings.TrimPrefix(digest, "sha256:")+".bundle" {
		t.Fatal("cached bundle filename does not match its archive digest")
	}
	// Mark the file so the restart distinguishes a cache hit from a fresh download.
	marker := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(archive, marker, marker); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dirs.Secrets, "bundle-signing.key")
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("cache hit after restart", func(t *testing.T) {
		standaloneE2ERun(t, node, entry, dirs, dsn, componentID, digest)
	})
	after, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("signing key changed on restart")
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(marker) {
		t.Fatal("restart replaced the cached archive")
	}
	archives, err = filepath.Glob(filepath.Join(dirs.Bundles, "*.bundle"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("want the original cached bundle after restart, got %d (glob error %v)", len(archives), err)
	}
}

func standaloneE2ERun(t *testing.T, node, entry string, dirs DataDirs, dsn, compID, digest string) string {
	t.Helper()
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
	// The in-memory repository starts empty each time. On restart point the new
	// revision at the first run's digest, which can only be served by the local cache.
	// With Postgres, redeploy the original revision and let the server derive its digest.
	if compID == "" || dsn == "" {
		sfx := make([]byte, 4)
		_, _ = rand.Read(sfx)
		comp := call(http.MethodPost, "v1/components", map[string]any{"name": "e2e-" + hex.EncodeToString(sfx), "kind": "flow"}, http.StatusCreated)
		compID = comp["id"].(string)
		revision := map[string]any{"content": json.RawMessage(`{
			"schema":"nurtuo.flow/v1","id":"standalone-e2e","name":"Standalone E2E","revision":1,
			"steps":[{"id":"start","kind":"start","config":{"type":"manual"}},
			         {"id":"return","kind":"returnDocuments","config":{}}],
			"edges":{"start:default":"return"}
		}`)}
		if digest != "" {
			revision["digest"] = digest
		}
		call(http.MethodPost, "v1/components/"+compID+"/revisions", revision, http.StatusCreated)
	}
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
	if status != "running" {
		t.Fatalf("deployment status %q, want running (message %q)\nlog:\n%s", status, message, log)
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
	return compID
}
