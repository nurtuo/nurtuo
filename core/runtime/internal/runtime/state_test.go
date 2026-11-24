// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
)

func unreachableGateway(Config) (gateway.Config, error) {
	return gateway.Config{
		Target:   "127.0.0.1:1",
		DialOpts: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	}, nil
}

func baseCfg() Config {
	return Config{RuntimeID: "r", ControlPlaneURL: "127.0.0.1:1", CertFile: "c", KeyFile: "k", CAFile: "ca"}
}

// runBriefly runs RunWith until it returns or 300ms pass, then cancels and returns its result.
func runBriefly(t *testing.T, cfg Config, o Options) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunWith(ctx, cfg, o) }()
	select {
	case err := <-done:
		return err
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("RunWith did not stop")
		return nil
	}
}

func TestLoadConfigStateDir(t *testing.T) {
	dir := t.TempDir()
	body := "runtime_id: rt-1\ncontrol_plane_url: cp:443\ncert_file: c.pem\nkey_file: k.pem\nca_file: ca.pem\n"
	p := filepath.Join(dir, "runtime.yaml")
	for name, tc := range map[string]struct{ extra, want string }{
		"unset":    {"", ""},
		"absolute": {"state_dir: " + filepath.ToSlash(filepath.Join(dir, "st")) + "\n", filepath.Join(dir, "st")},
		"relative": {"state_dir: st\n", filepath.Join(dir, "st")},
	} {
		if err := os.WriteFile(p, []byte(body+tc.extra), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.StateDir != tc.want {
			t.Errorf("%s: StateDir = %q, want %q", name, c.StateDir, tc.want)
		}
	}
	j := filepath.Join(dir, "runtime.json")
	js := `{"runtime_id":"r","control_plane_url":"cp:1","cert_file":"/c","key_file":"/k","ca_file":"/ca","state_dir":"/var/lib/x"}`
	if err := os.WriteFile(j, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(j); err != nil || c.StateDir != "/var/lib/x" {
		t.Fatalf("json: %+v %v", c, err)
	}
}

func TestRunFailsWhenStateDirUnusable(t *testing.T) {
	t.Run("state dir is a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := baseCfg()
		cfg.StateDir, cfg.BundleDir = f, t.TempDir()
		err := RunWith(context.Background(), cfg, Options{Gateway: unreachableGateway})
		if err == nil || !strings.Contains(err.Error(), "state_dir") {
			t.Fatalf("err = %v, want state_dir error", err)
		}
	})
	t.Run("jetstream dir locked by another backend", func(t *testing.T) {
		state := t.TempDir()
		st, err := prepareStateDir(state)
		if err != nil {
			t.Fatal(err)
		}
		_, closeQ, err := openPersistentQueues(st)
		if err != nil {
			t.Fatal(err)
		}
		defer closeQ()
		cfg := baseCfg()
		cfg.StateDir, cfg.BundleDir = state, t.TempDir()
		if err := runBriefly(t, cfg, Options{Gateway: unreachableGateway}); err == nil {
			t.Fatal("want error: second backend on the same state_dir must not start (no memory fallback)")
		}
	})
	t.Run("corrupt revision file", func(t *testing.T) {
		state := t.TempDir()
		if err := os.WriteFile(filepath.Join(state, supervisor.RevisionFile), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := baseCfg()
		cfg.StateDir, cfg.BundleDir = state, t.TempDir()
		if err := RunWith(context.Background(), cfg, Options{Gateway: unreachableGateway}); err == nil {
			t.Fatal("want error for corrupt revision file")
		}
	})
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRunWithoutStateDirWarns(t *testing.T) {
	buf := &syncBuf{}
	cfg := baseCfg()
	cfg.BundleDir = t.TempDir()
	log := slog.New(slog.NewTextHandler(buf, nil))
	if err := runBriefly(t, cfg, Options{Logger: log, Gateway: unreachableGateway}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "state_dir is not set") {
		t.Fatalf("missing startup warning in log:\n%s", buf.String())
	}

	// With state_dir set there is no warning, and the JetStream directory is created.
	buf2 := &syncBuf{}
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	if err := runBriefly(t, cfg, Options{Logger: slog.New(slog.NewTextHandler(buf2, nil)), Gateway: unreachableGateway}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), "state_dir is not set") {
		t.Fatalf("unexpected warning:\n%s", buf2.String())
	}
	if fi, err := os.Stat(filepath.Join(cfg.StateDir, "jetstream")); err != nil || !fi.IsDir() {
		t.Fatalf("jetstream dir missing: %v", err)
	}
}

func TestQueueMessagesSurviveRuntimeRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := prepareStateDir(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}

	// First run: a message is accepted by the queue but no listener consumes it before the stop.
	qb, closeQ, err := openPersistentQueues(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := qb.Ensure(ctx, queue.QueueDef{Name: "jobs"}); err != nil {
		t.Fatal(err)
	}
	if err := qb.Publish(ctx, "jobs", queue.Message{ID: "j1", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	closeQ()

	// Second run on the same state dir: the flow's queue listener receives the stored message.
	qb2, closeQ2, err := openPersistentQueues(state)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQ2()
	bundle := `{"flows":[` + flowJSON("jobs", `{"type":"queue","queue":"jobs"}`) + `]}`
	d := NewEngineDeployer(nil, qb2, func(context.Context, string) ([]byte, error) { return []byte(bundle), nil })
	defer func() { _ = d.Shutdown(ctx) }()
	if err := d.Apply(ctx, supervisor.Deployment{ID: "dep1", BundleDigest: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	jobs, ok := d.Manager().Get("dep1/jobs")
	if !ok {
		t.Fatal("queue listener not started")
	}
	deadline := time.Now().Add(10 * time.Second)
	for jobs.Status().Received < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := jobs.Status(); st.Received != 1 || st.Failed != 0 {
		t.Fatalf("queue status after restart = %+v, want the pre-restart message delivered", st)
	}
}
