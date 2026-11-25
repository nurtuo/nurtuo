// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Re-exec pattern: the test binary acts as the fake child when NURTUO_FAKE_CHILD is set.
func TestMain(m *testing.M) {
	switch os.Getenv("NURTUO_FAKE_CHILD") {
	case "exit1":
		_, _ = os.Stdout.WriteString("hello from child dsn=" + os.Getenv("DATABASE_URL") + "\n")
		os.Exit(1)
	case "sleep":
		_, _ = os.Stdout.WriteString("up dsn=" + os.Getenv("DATABASE_URL") + "\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func fakeSpec(mode string, log *syncBuf) ChildSpec {
	return ChildSpec{
		Name: "fake", Command: []string{os.Args[0]}, Log: log,
		Env:        []string{"NURTUO_FAKE_CHILD=" + mode, "DATABASE_URL=postgres://x"},
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, StopGrace: time.Second,
	}
}

func TestChildRestartsWithBackoffAndCapturesLogs(t *testing.T) {
	log := &syncBuf{}
	spec := fakeSpec("exit1", log)
	spec.MaxRestarts = 2
	c := NewChild(spec)
	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeded 2 restarts") {
		t.Fatalf("want restart budget error, got %v", err)
	}
	if c.Starts() != 3 {
		t.Fatalf("starts = %d, want 3", c.Starts())
	}
	out := log.String()
	if !strings.Contains(out, "[fake] hello from child dsn=postgres://x") || !strings.Contains(out, "restarting in") {
		t.Fatalf("log capture missing: %q", out)
	}
}

func TestChildStopsOnCancel(t *testing.T) {
	log := &syncBuf{}
	c := NewChild(fakeSpec("sleep", log))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "up dsn=") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not stop")
	}
}

func TestWaitHealthy(t *testing.T) {
	n := 0
	probe := func(context.Context) error {
		if n++; n < 3 {
			return errors.New("not yet")
		}
		return nil
	}
	if err := WaitHealthy(context.Background(), probe, time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	bad := func(context.Context) error { return errors.New("never") }
	if err := WaitHealthy(context.Background(), bad, time.Millisecond, 20*time.Millisecond); err == nil {
		t.Fatal("want timeout error")
	}
}

// --- fakes and ordering ---

type events struct {
	mu sync.Mutex
	l  []string
}

func (e *events) add(s string) { e.mu.Lock(); e.l = append(e.l, s); e.mu.Unlock() }
func (e *events) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.l...)
}

type fakeDB struct {
	ev       *events
	started  bool
	startErr error
}

func (f *fakeDB) Start(context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	f.ev.add("db.start")
	return nil
}
func (f *fakeDB) DSN() string { return "postgres://fake/db" }
func (f *fakeDB) Stop(context.Context) error {
	f.started = false
	f.ev.add("db.stop")
	return nil
}

type fakeService struct {
	ev        *events
	dsn       string
	healthErr error
	started   atomic.Bool
}

func (f *fakeService) Run(ctx context.Context) error {
	f.ev.add("cp.start")
	f.started.Store(true)
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // slow stop: DB must still wait for it
	f.ev.add("cp.stop")
	return nil
}
func (f *fakeService) WaitHealthy(context.Context) error {
	// Run executes in its own goroutine: wait until it has recorded cp.start so the event order is deterministic.
	for i := 0; i < 2000 && !f.started.Load(); i++ {
		time.Sleep(time.Millisecond)
	}
	f.ev.add("cp.healthy")
	return f.healthErr
}

type fakeBoot struct{ ev *events }

func (f fakeBoot) Bootstrap(DataDirs) (*Credentials, error) {
	f.ev.add("bootstrap")
	return &Credentials{DBPassword: "p"}, nil
}

func newStack(t *testing.T, ev *events, db *fakeDB, svc *fakeService, rt RuntimeFunc) *Stack {
	return &Stack{
		Dirs: NewDataDirs(t.TempDir()), Bootstrap: fakeBoot{ev}, DB: db,
		ControlPlane: func(dsn string, _ *Credentials) Service { svc.dsn = dsn; return svc },
		Runtime:      rt,
	}
}

func TestShutdownOrdering(t *testing.T) {
	ev := &events{}
	db, svc := &fakeDB{ev: ev}, &fakeService{ev: ev}
	ctx, cancel := context.WithCancel(context.Background())
	s := newStack(t, ev, db, svc, func(ctx context.Context, _ RuntimeEnv) error {
		ev.add("rt.start")
		cancel() // simulate Ctrl-C once running
		<-ctx.Done()
		ev.add("rt.stop")
		return nil
	})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := "bootstrap,db.start,cp.start,cp.healthy,rt.start,rt.stop,cp.stop,db.stop"
	got := strings.Join(ev.get(), ",")
	if got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
	if svc.dsn != "postgres://fake/db" {
		t.Fatalf("control plane got dsn %q", svc.dsn)
	}
}

func TestUnhealthyControlPlaneStopsDBAfterCP(t *testing.T) {
	ev := &events{}
	db, svc := &fakeDB{ev: ev}, &fakeService{ev: ev, healthErr: errors.New("boom")}
	s := newStack(t, ev, db, svc, func(context.Context, RuntimeEnv) error { t.Fatal("runtime must not start"); return nil })
	if err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
	got := ev.get()
	if n := len(got); n < 2 || got[n-2] != "cp.stop" || got[n-1] != "db.stop" {
		t.Fatalf("bad tail: %v", got)
	}
}

func TestDBStartFailure(t *testing.T) {
	ev := &events{}
	db := &fakeDB{ev: ev, startErr: errors.New("no pg")}
	s := newStack(t, ev, db, &fakeService{ev: ev}, nil)
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want error")
	}
	for _, e := range ev.get() {
		if e == "cp.start" || e == "db.stop" {
			t.Fatalf("unexpected %s", e)
		}
	}
}

// --- layout and bootstrap ---

func TestDefaultDataRoot(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"windows", map[string]string{"ProgramData": `C:\PD`}, filepath.Join(`C:\PD`, "Nurtuo")},
		{"darwin", map[string]string{"HOME": "/h"}, filepath.Join("/h", "Library", "Application Support", "Nurtuo")},
		{"linux", map[string]string{"HOME": "/h"}, filepath.Join("/h", ".local", "share", "nurtuo")},
		{"linux", map[string]string{"XDG_DATA_HOME": "/x", "HOME": "/h"}, filepath.Join("/x", "nurtuo")},
		{"linux", map[string]string{"NURTUO_DATA_DIR": "/o"}, "/o"},
	}
	for _, c := range cases {
		got, err := DefaultDataRoot(c.goos, env(c.env))
		if err != nil || got != c.want {
			t.Errorf("%s %v: got %q,%v want %q", c.goos, c.env, got, err, c.want)
		}
	}
	if _, err := DefaultDataRoot("linux", env(nil)); err == nil {
		t.Error("want error with no env")
	}
}

// TestDataDirsEnsureUnixModes characterizes the Unix layout modes, which the Windows DACL work
// must not change (D8): secrets 0700, the rest 0755, and an existing secrets dir is not re-moded.
func TestDataDirsEnsureUnixModes(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX modes; Windows is covered by standalone_windows_test.go")
	}
	d := NewDataDirs(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	mode := func(p string) os.FileMode {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	if m := mode(d.Secrets); m != 0o700 {
		t.Fatalf("secrets mode %#o, want 0700", m)
	}
	for _, p := range []string{d.Postgres, d.PGRun, d.PGCache, d.Logs} {
		if m := mode(p); m&^0o755 != 0 {
			t.Fatalf("%s mode %#o, want at most 0755 (umask may clear bits)", p, m)
		}
	}
	// An existing 0755 secrets dir is left unchanged (MkdirAll never re-modes).
	d2 := NewDataDirs(t.TempDir())
	if err := os.MkdirAll(d2.Secrets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d2.Secrets, 0o755); err != nil {
		t.Fatal(err)
	}
	repaired, err := d2.EnsureReport()
	if err != nil || repaired != nil {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	if m := mode(d2.Secrets); m != 0o755 {
		t.Fatalf("existing secrets re-moded to %#o", m)
	}
}

func TestDataDirsEnsureBundles(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX modes; Windows ACLs are checked separately")
	}
	d := NewDataDirs(t.TempDir())
	if d.Bundles != filepath.Join(d.Root, "bundles") {
		t.Fatal("unexpected bundle cache path")
	}
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.Chmod(d.Bundles, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.Ensure(); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(d.Bundles)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("bundle cache mode %#o, want 0700", fi.Mode().Perm())
		}
	}
}

func TestBundleSigningKeyStableAndPrivate(t *testing.T) {
	d := NewDataDirs(t.TempDir())
	b := NewLocalBootstrapper()
	c, err := b.Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d.Secrets, "bundle-signing.key")
	if c.BundleSigningKeyFile != path || c.BundlePublicKey == "" {
		t.Fatal("bundle signing credentials missing")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if goruntime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("signing key mode %#o, want 0600", fi.Mode().Perm())
	}

	corrupt := []byte("malformed test fixture")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Bootstrap(d); err == nil || !strings.Contains(err.Error(), "bundle-signing.key is invalid") {
		t.Fatal("bootstrap must reject a corrupt signing key")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(corrupt) {
		t.Fatal("bootstrap replaced the corrupt signing key")
	}
}

func TestLoadOrGenSigningKeyConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle-signing.key")
	start := make(chan struct{})
	const count = 12
	type result struct {
		publicKey string
		err       error
	}
	results := make(chan result, count)
	for range count {
		go func() {
			<-start
			pub, err := loadOrGenSigningKey(path)
			results <- result{pub, err}
		}()
	}
	close(start)
	want, err := loadOrGenSigningKey(path)
	if err != nil {
		t.Fatal(err)
	}
	for range count {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.publicKey != want {
			t.Fatal("concurrent calls returned different signing keys")
		}
	}
}

func TestStandaloneRuntimeConfig(t *testing.T) {
	d := NewDataDirs(t.TempDir())
	c, err := NewLocalBootstrapper().Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	cfg := standaloneRuntimeConfig("127.0.0.1:9443", RuntimeEnv{Dirs: d, Creds: c})
	if cfg.BundleDir != d.Bundles || len(cfg.TrustedKeys) != 1 || cfg.TrustedKeys[0] != c.BundlePublicKey {
		t.Fatal("standalone runtime must use the local bundle cache and trust the install key")
	}
}

func TestLocalBootstrapIdempotentAndUsableForMTLS(t *testing.T) {
	d := NewDataDirs(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	b := NewLocalBootstrapper()
	c1, err := b.Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := b.Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	if c1.DBPassword == "" || c1.AdminToken == "" || c1.BundlePublicKey == "" || *c1 != *c2 {
		t.Fatal("secrets missing or not stable")
	}
	if _, err := tls.LoadX509KeyPair(c1.ServerCertFile, c1.ServerKeyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(c1.ClientCertFile, c1.ClientKeyFile); err != nil {
		t.Fatal(err)
	}
}

func TestResolveControlPlaneEntry(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("core", "control-plane", "dist", "main.js")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "bin", "x")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveControlPlaneEntry(rel, deep, t.TempDir()); got != filepath.Join(root, rel) {
		t.Fatalf("exe-relative: got %q", got)
	}
	if got := resolveControlPlaneEntry(rel, t.TempDir(), deep); got != filepath.Join(root, rel) {
		t.Fatalf("cwd-relative: got %q", got)
	}
	if got := resolveControlPlaneEntry("", deep, deep); got != "" {
		t.Fatalf("empty: got %q", got)
	}
	if got := resolveControlPlaneEntry("nope/none.js", deep, deep); got != "nope/none.js" {
		t.Fatalf("missing: got %q", got)
	}
}

func TestControlPlaneEnvWiresGatewayAndAuth(t *testing.T) {
	c := &Credentials{CAFile: "ca.pem", ServerCertFile: "s.pem", ServerKeyFile: "s.key", BundleSigningKeyFile: "bundle-signing.key", AdminToken: strings.Repeat("a", 48)}
	got := strings.Join(controlPlaneEnv("postgres://x", c, 8080, "127.0.0.1:9443"), "\n")
	for _, want := range []string{
		"DATABASE_URL=postgres://x", "PORT=8080", "GATEWAY_PORT=9443", "GATEWAY_CERT=s.pem", "GATEWAY_KEY=s.key",
		"GATEWAY_CA=ca.pem", "NURTUO_BUNDLE_SIGNING_KEY_FILE=bundle-signing.key", "NURTUO_AUTH_SECRET=" + c.AdminToken, "NURTUO_BOOTSTRAP_EMAIL=admin@nurtuo.local",
	} {
		if !strings.Contains(got, want) {
			t.Error("control plane environment is missing an expected setting")
		}
	}
}

func TestPublishSigningKeyWithoutHardLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle-signing.key")
	staged := filepath.Join(dir, "staged")
	for _, seed := range []string{"complete first seed", "complete second seed"} {
		if err := os.WriteFile(staged, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		err := publishSigningKey(staged, path)
		if seed == "complete first seed" {
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("publisher did not move staged file: %v", err)
			}
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatalf("second publication returned %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "complete first seed" {
			t.Fatalf("published file did not preserve the complete first seed: %v", err)
		}
	}
}
