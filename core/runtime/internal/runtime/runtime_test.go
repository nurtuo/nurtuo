// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigYAMLAndJSON(t *testing.T) {
	abs := filepath.ToSlash(filepath.Join(t.TempDir(), "k.pem"))
	y := write(t, "runtime.yaml", "runtime_id: rt-1\ncontrol_plane_url: cp.example:443\ncert_file: c.pem\nkey_file: "+abs+"\nca_file: ca.pem\n")
	c, err := LoadConfig(y)
	if err != nil {
		t.Fatal(err)
	}
	if c.RuntimeID != "rt-1" || c.ControlPlaneURL != "cp.example:443" || c.KeyFile != abs {
		t.Fatalf("%+v", c)
	}
	if want := filepath.Join(filepath.Dir(y), "c.pem"); c.CertFile != want {
		t.Fatalf("relative path not resolved: %q want %q", c.CertFile, want)
	}
	j := write(t, "runtime.json", `{"runtime_id":"rt-2","control_plane_url":"h:1","cert_file":"/c","key_file":"/k","ca_file":"/ca"}`)
	c, err = LoadConfig(j)
	if err != nil || c.RuntimeID != "rt-2" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("want read error")
	}
	_, err := LoadConfig(write(t, "a.yaml", "runtime_id: x\n"))
	if err == nil || !strings.Contains(err.Error(), "control_plane_url") || !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("want missing-fields error, got %v", err)
	}
	if _, err := LoadConfig(write(t, "b.json", "{not json")); err == nil {
		t.Fatal("want parse error")
	}
}

// Run must return nil promptly on context cancel even though the gateway is unreachable.
func TestRunGracefulShutdown(t *testing.T) {
	cfg := Config{RuntimeID: "r", ControlPlaneURL: "127.0.0.1:1", CertFile: "c", KeyFile: "k", CAFile: "ca"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWith(ctx, cfg, Options{Gateway: func(Config) (gateway.Config, error) {
			return gateway.Config{
				Target:   "127.0.0.1:1",
				DialOpts: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
			}, nil
		}})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("want nil on graceful stop, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	if err := Run(context.Background(), Config{}); err == nil {
		t.Fatal("want validation error")
	}
}
