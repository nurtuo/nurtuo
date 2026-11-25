// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestFetchingBundleSource(t *testing.T) {
	_, digest, archive, pubHex := signedFixture(t)
	pub, err := bundle.DecodePublicKey(pubHex)
	checkFetchTestError(t, err)
	other, _, err := bundle.GenerateKey()
	checkFetchTestError(t, err)

	for _, tc := range []struct {
		name     string
		existing []byte
		download []byte
		key      ed25519.PublicKey
		wantErr  string
		calls    int
	}{
		{name: "cache hit", existing: archive, key: pub},
		{name: "cache miss", download: archive, key: pub, calls: 1},
		{name: "replace corrupt file", existing: []byte("corrupt"), download: archive, key: pub, calls: 1},
		{name: "digest mismatch", download: []byte("invalid"), key: pub, wantErr: "does not match digest", calls: 1},
		{name: "wrong signature", download: archive, key: other, wantErr: "signature verification failed", calls: 1},
		{name: "preserve file on rejected replacement", existing: []byte("existing"), download: archive, key: other, wantErr: "signature verification failed", calls: 1},
		{name: "oversize", key: pub, wantErr: "larger than", calls: 1},
		{name: "invalid digest", key: pub, wantErr: "invalid bundle digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")+".bundle")
			if tc.existing != nil {
				checkFetchTestError(t, os.WriteFile(path, tc.existing, 0o600))
			}
			calls := 0
			src := FetchingBundleSource(dir, func(_ context.Context, got string, w io.Writer, limit int64) (int64, error) {
				calls++
				if got != digest || limit != bundle.MaxBundleSize {
					t.Fatalf("fetch digest=%q limit=%d", got, limit)
				}
				if tc.download == nil {
					return 0, fmt.Errorf("artifact larger than %d bytes", limit)
				}
				return io.Copy(w, bytes.NewReader(tc.download))
			}, []ed25519.PublicKey{tc.key})
			request := digest
			if tc.name == "invalid digest" {
				request = "sha256:../escape"
			}
			_, err := src(context.Background(), request)
			if tc.wantErr == "" {
				checkFetchTestError(t, err)
				info, err := os.Stat(path)
				checkFetchTestError(t, err)
				if goruntime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
					t.Fatalf("cache mode = %o", info.Mode().Perm())
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("fetch error = %v, want %q", err, tc.wantErr)
			}
			want := archive
			if tc.wantErr != "" {
				want = tc.existing
			}
			got, err := os.ReadFile(path)
			if want == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected fetch left a cache file: %v", err)
				}
			} else if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("cache content differs: %v", err)
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".fetch-*"))
			checkFetchTestError(t, err)
			if len(temps) != 0 || calls != tc.calls {
				t.Fatalf("left %d temp files and made %d fetches, want %d fetches", len(temps), calls, tc.calls)
			}
		})
	}
}

type artifactGateway struct {
	placeholderGateway
	archive []byte
	calls   atomic.Int32
}

func (g *artifactGateway) FetchArtifact(_ *gatewayv1.FetchArtifactRequest, stream gatewayv1.RuntimeGatewayService_FetchArtifactServer) error {
	g.calls.Add(1)
	return stream.Send(&gatewayv1.FetchArtifactResponse{Data: g.archive})
}

func TestRunWithFetchesSignedBundle(t *testing.T) {
	_, digest, archive, pubHex := signedFixture(t)
	for _, tc := range []struct {
		name  string
		keys  []string
		want  string
		calls int32
	}{
		{name: "signed", keys: []string{pubHex}, want: supervisor.StateRunning, calls: 1},
		{name: "unsigned", want: supervisor.StateError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stale := filepath.Join(dir, ".fetch-stale")
			checkFetchTestError(t, os.WriteFile(stale, nil, 0o600))
			lis := bufconn.Listen(1 << 20)
			g := &artifactGateway{placeholderGateway: placeholderGateway{digest: digest, statuses: make(chan *gatewayv1.DeploymentStatus, 8)}, archive: archive}
			gs := grpc.NewServer()
			gatewayv1.RegisterRuntimeGatewayServiceServer(gs, g)
			go func() { _ = gs.Serve(lis) }()
			t.Cleanup(gs.Stop)

			cfg := Config{RuntimeID: "r", ControlPlaneURL: "bufnet", CertFile: "c", KeyFile: "k", CAFile: "ca", BundleDir: dir, TrustedKeys: tc.keys, HTTPAddr: "127.0.0.1:0"}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- RunWith(ctx, cfg, Options{Gateway: func(Config) (gateway.Config, error) {
					return gateway.Config{Target: "passthrough:///bufnet", DialOpts: []grpc.DialOption{
						grpc.WithTransportCredentials(insecure.NewCredentials()),
						grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
					}}, nil
				}})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					checkFetchTestError(t, err)
				case <-time.After(5 * time.Second):
					t.Error("runtime did not stop")
				}
			})
			for _, want := range []string{supervisor.StateApplying, tc.want} {
				select {
				case got := <-g.statuses:
					if got.GetState() != want {
						t.Fatalf("status=%v, want %s", got, want)
					}
				case <-ctx.Done():
					t.Fatalf("missing status %s", want)
				}
			}
			if got := g.calls.Load(); got != tc.calls {
				t.Fatalf("fetch count = %d, want %d", got, tc.calls)
			}
			if tc.calls > 0 {
				if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale download was not removed: %v", err)
				}
			}
		})
	}
}

func checkFetchTestError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
