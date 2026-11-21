// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/listener"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/memory"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
)

func signedFixture(t *testing.T) (dir, digest string, data []byte, pubHex string) {
	t.Helper()
	pub, priv, err := bundle.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	data, err = bundle.Pack(bundle.Input{Name: "t", CreatedAt: time.Unix(1, 0), Components: []bundle.Component{
		{Kind: "flow", ID: "web", Revision: 1, Document: json.RawMessage(flowJSON("web", `{"type":"listener","connector":"http","path":"/p"}`))},
		{Kind: "mapping", ID: "m", Revision: 1, Document: json.RawMessage(`{}`)},
	}}, priv)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".bundle"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, "sha256:" + hex.EncodeToString(sum[:]), data, bundle.EncodePublicKey(pub)
}

func TestSignedBundleSource(t *testing.T) {
	dir, digest, data, pubHex := signedFixture(t)
	pub, _ := bundle.DecodePublicKey(pubHex)
	keys := []ed25519.PublicKey{pub}
	ctx := context.Background()

	if raw, err := SignedBundleSource(dir, nil)(ctx, digest); err == nil {
		t.Fatalf("no trusted keys must reject, got %s", raw)
	}
	raw, err := SignedBundleSource(dir, keys)(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	flows, err := ParseBundleFlows(raw)
	if err != nil || len(flows) != 1 || flows[0].ID != "web" {
		t.Fatalf("flows=%v err=%v", flows, err)
	}

	other, _, _ := bundle.GenerateKey()
	if _, err := SignedBundleSource(dir, []ed25519.PublicKey{other})(ctx, digest); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("wrong key: %v", err)
	}
	// Tampered content under the original name -> digest mismatch.
	tam := append([]byte(nil), data...)
	tam[len(tam)/2] ^= 1
	if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")+".bundle"), tam, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SignedBundleSource(dir, keys)(ctx, digest); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tamper: %v", err)
	}
	for _, bad := range []string{"", "sha256:../x", "sha256:abc", "sha256:" + strings.Repeat("G", 64)} {
		if _, err := SignedBundleSource(dir, keys)(ctx, bad); err == nil {
			t.Errorf("digest %q accepted", bad)
		}
	}
}

func TestConfigTrustedKeys(t *testing.T) {
	_, _, _, pubHex := signedFixture(t)
	c := Config{RuntimeID: "r", ControlPlaneURL: "h:1", CertFile: "c", KeyFile: "k", CAFile: "ca"}
	c.BundleDir, c.TrustedKeys = "/b", []string{pubHex}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.TrustedKeys = []string{"zz"}
	if err := c.Validate(); err == nil {
		t.Fatal("bad key must fail")
	}
	c.BundleDir, c.TrustedKeys = "", []string{pubHex}
	if err := c.Validate(); err == nil {
		t.Fatal("trusted_keys without bundle_dir must fail")
	}
}

func TestInvalidFlowErrorCarriesDiagnostics(t *testing.T) {
	_, err := ParseBundleFlows([]byte(`{"schema":"nurtuo.flow/v1","id":"bad"}`))
	if err == nil || !strings.Contains(err.Error(), "schema.") {
		t.Fatalf("error must name the diagnostics, got %v", err)
	}
}

// Regression: a paused listener must stay paused when another deployment is applied or removed
// (reapply used to resume everything); removing its own deployment forgets the pause.
func TestPausedListenerSurvivesSiblingApply(t *testing.T) {
	ctx := context.Background()
	d := NewEngineDeployer(nil, memory.New(), func(context.Context, string) ([]byte, error) {
		return []byte(flowJSON("web", `{"type":"listener","connector":"http","path":"/hook","method":"POST"}`)), nil
	})
	defer func() { _ = d.Shutdown(ctx) }()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.Apply(ctx, supervisor.Deployment{ID: "a", BundleDigest: "x"}))
	must(d.Commands()["pause_listener"](ctx, map[string]string{"listener": "a/web"}))
	must(d.Apply(ctx, supervisor.Deployment{ID: "b", BundleDigest: "y"}))
	a, _ := d.Manager().Get("a/web")
	if st := a.Status().State; st != listener.StatePaused {
		t.Fatalf("state after sibling apply = %s, want paused", st)
	}
	must(d.Remove(ctx, "b"))
	if st := a.Status().State; st != listener.StatePaused {
		t.Fatalf("state after sibling remove = %s, want paused", st)
	}
	must(d.Commands()["resume_listener"](ctx, map[string]string{"listener": "a/web"}))
	if st := a.Status().State; st != listener.StateRunning {
		t.Fatalf("state after resume = %s", st)
	}
	if err := d.Commands()["pause_listener"](ctx, map[string]string{"listener": "zzz/web"}); err == nil {
		t.Fatal("unknown listener must error")
	}
	if err := d.Commands()["pause_listener"](ctx, nil); err == nil {
		t.Fatal("missing arg must error")
	}
}
