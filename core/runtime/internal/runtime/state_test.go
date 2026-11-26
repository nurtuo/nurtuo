// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
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
		"absolute": {"state_dir: " + strconv.Quote(filepath.Join(dir, "st")) + "\n", filepath.Join(dir, "st")},
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
	stateDir := filepath.Join(dir, "state")
	js := `{"runtime_id":"r","control_plane_url":"cp:1","cert_file":"/c","key_file":"/k","ca_file":"/ca","state_dir":` + strconv.Quote(stateDir) + `}`
	if err := os.WriteFile(j, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(j); err != nil || c.StateDir != stateDir {
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
	for _, persistentQueues := range []bool{true, false} {
		name := "revision-only runtime holds lock"
		if persistentQueues {
			name = "JetStream runtime holds lock"
		}
		t.Run(name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.StateDir = t.TempDir()
			if persistentQueues {
				cfg.BundleDir = t.TempDir()
			}
			ready := make(chan struct{})
			build := func(cfg Config) (gateway.Config, error) {
				close(ready)
				return unreachableGateway(cfg)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- RunWith(ctx, cfg, Options{Gateway: build}) }()
			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("first runtime stopped: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("first runtime did not start")
			}
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("first runtime: %v", err)
				}
			})
			if err := runBriefly(t, cfg, Options{Gateway: unreachableGateway}); err == nil || !strings.Contains(err.Error(), "is in use") {
				t.Fatalf("second runtime: got %v, want state_dir lock contention", err)
			}
		})
	}
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
	cfg := baseCfg()
	for _, bundleDir := range []string{"", t.TempDir()} {
		cfg.BundleDir = bundleDir
		buf := &syncBuf{}
		log := slog.New(slog.NewTextHandler(buf, nil))
		if err := runBriefly(t, cfg, Options{Logger: log, Gateway: unreachableGateway}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "state_dir is not set") {
			t.Fatalf("missing startup warning with bundle_dir %q:\n%s", bundleDir, buf.String())
		}
	}
	cfg.BundleDir = t.TempDir()

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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cfg := baseCfg()
	cfg.StateDir, cfg.BundleDir = t.TempDir(), t.TempDir()
	queueName := strings.Repeat("jobs.eu.", 24) + "orders"
	bundle := `{"flows":[` + flowJSON("jobs", `{"type":"queue","queue":"`+queueName+`"}`) + `]}`
	bundle = strings.Replace(bundle, `"edges":{"s:default":"r"}`, `"edges":{"s:default":"m","m:default":"r"}`, 1)
	bundle = strings.Replace(bundle, `{"id":"r"`, `{"id":"m","kind":"message","config":{"template":"seen"}},{"id":"r"`, 1)
	if err := os.WriteFile(filepath.Join(cfg.BundleDir, "abc.json"), []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}

	g := &placeholderGateway{
		statuses: make(chan *gatewayv1.DeploymentStatus, 8),
		desired: &gatewayv1.DesiredState{
			Revision:    7,
			Deployments: []*gatewayv1.Deployment{{DeploymentId: "dep1", BundleDigest: "sha256:abc"}},
		},
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	gatewayv1.RegisterRuntimeGatewayServiceServer(gs, g)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	build := func(Config) (gateway.Config, error) {
		return gateway.Config{
			Target:    "passthrough:///bufnet",
			RuntimeID: cfg.RuntimeID,
			DialOpts: []grpc.DialOption{
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			},
		}, nil
	}

	run := func(eng *engine.Engine, verify func()) {
		t.Helper()
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- RunWith(runCtx, cfg, Options{Gateway: build, Engine: eng}) }()
		defer func() {
			stop()
			if err := <-done; err != nil {
				t.Errorf("RunWith: %v", err)
			}
		}()
		for {
			select {
			case st := <-g.statuses:
				switch st.GetState() {
				case supervisor.StateError:
					t.Fatalf("deployment error: %v", st)
				case supervisor.StateRunning:
					verify()
					return
				}
			case <-ctx.Done():
				t.Fatal("gateway did not report a running deployment")
			}
		}
	}

	revisionPath := filepath.Join(cfg.StateDir, supervisor.RevisionFile)
	run(nil, func() {
		// The status may reach the gateway before the revision file is committed.
		for {
			rev, err := supervisor.LoadRevision(revisionPath)
			if err != nil {
				t.Fatal(err)
			}
			if rev == g.desired.GetRevision() {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("saved revision = %d, want %d", rev, g.desired.GetRevision())
			case <-time.After(10 * time.Millisecond):
			}
		}
	})

	// Seed a queued message with no active listener, then reopen the same state directory.
	func() {
		qb, err := openPersistentQueues(cfg.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = qb.Close() }()
		if err := qb.Ensure(ctx, queue.QueueDef{Name: queueName}); err != nil {
			t.Fatal(err)
		}
		if err := qb.Publish(ctx, queueName, queue.Message{ID: "j1", Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}()

	eng := engine.New()
	delivered := make(chan string, 1)
	eng.Registry.Register(flow.KindMessage, engine.PerDocument(func(_ context.Context, _ *engine.StepContext, d *docs.Document) (*docs.Document, error) {
		delivered <- d.ID
		return d, nil
	}))
	run(eng, func() {
		select {
		case id := <-delivered:
			if id != "j1" {
				t.Fatalf("delivered message = %q, want j1", id)
			}
		case <-ctx.Done():
			t.Fatal("restarted listener did not process the queued message")
		}
	})
}
