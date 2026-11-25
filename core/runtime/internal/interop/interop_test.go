// SPDX-License-Identifier: AGPL-3.0-only

package interop

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRuntimeAgainstTSGateway is the headline test: real Go runtime <-> real TS gateway over mTLS,
// steered through the TS REST API. Plain <digest>.json bundles.
func TestRuntimeAgainstTSGateway(t *testing.T) {
	cp := startControlPlane(t, 0, 0)
	be := newBackend(t)
	cp.registerRuntime(t, "rt-1")
	p := startRuntime(t, cp, "rt-1", "rt-1", be.URL, false)

	// Deploy while connected: only the TS outbox publisher can push this (the REST API does not).
	comp, digest := cp.component(t, "employee", flow("v1"), "")
	p.putPlain(t, digest, flow("v1"))
	dep1 := cp.deploy(t, comp, 1, "rt-1")
	cp.waitStatus(t, dep1, "running")
	p.waitServing(t)

	// Gateway restart: the runtime keeps serving its last state, reconnects with backoff and resyncs.
	rest, gw := cp.restPort, cp.gatewayPort
	cp.stop()
	if code, body := p.post(t); code != 200 {
		t.Fatalf("listener must keep serving while the control plane is down: %d %s", code, body)
	}
	cp2 := startControlPlane(t, rest, gw)
	cp2.registerRuntime(t, "rt-1") // in-memory repo: the new instance starts empty

	// The new instance is at generation 0; two deploys of the component bring it to generation 2,
	// which differs from the runtime's applied revision (1), so Hello reconcile / outbox deliver it.
	comp2, digest2 := cp2.component(t, "employee", flow("v2"), "")
	p.putPlain(t, digest2, flow("v2"))
	cp2.deploy(t, comp2, 1, "rt-1")
	r2 := cp2.mustCall(t, "POST", "/v1/components/"+comp2+"/revisions", map[string]any{"content": flow("v3")}, 201)
	p.putPlain(t, r2["digest"].(string), flow("v3"))
	dep2 := cp2.deploy(t, comp2, 2, "rt-1")
	cp2.waitStatus(t, dep2, "running") // proves the runtime reconnected and reported to the new instance
	p.waitServing(t)

	// Undeploy through the control plane: the desired state no longer lists the components.
	cp2.mustCall(t, "DELETE", "/v1/runtimes/rt-1/deployments/"+comp2, nil, 202)
	p.waitRefused(t)
}

// TestWrongIdentityRejected: rt-2's certificate claiming runtime id rt-1 is refused by the gateway;
// the Go runtime keeps retrying (no crash) and surfaces the PermissionDenied error in its logs.
func TestWrongIdentityRejected(t *testing.T) {
	cp := startControlPlane(t, 0, 0)
	be := newBackend(t)
	cp.registerRuntime(t, "rt-1")
	cp.registerRuntime(t, "rt-2")
	imposter := startRuntime(t, cp, "rt-1", "rt-2", be.URL, false)

	// Deploy to rt-1: the imposter must never receive it.
	comp, digest := cp.component(t, "employee", flow("v1"), "")
	imposter.putPlain(t, digest, flow("v1"))
	dep := cp.deploy(t, comp, 1, "rt-1")

	eventually(t, "PermissionDenied surfaced by the runtime client", 15*time.Second, func() bool {
		return strings.Contains(imposter.logs.String(), "PermissionDenied")
	})
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-imposter.done:
		t.Fatalf("runtime exited on a rejected identity: %v", err)
	default:
	}
	if st, _ := cp.deploymentStatus(t, dep); st != "pending" {
		t.Fatalf("deployment status %q: the imposter must not have applied rt-1's state", st)
	}
	if code, _ := imposter.post(t); code == 200 {
		t.Fatal("imposter serves rt-1's flow")
	}

	// The legitimate rt-1 still connects and applies it.
	good := startRuntime(t, cp, "rt-1", "rt-1", be.URL, false, imposter.bundles)
	cp.waitStatus(t, dep, "running")
	good.waitServing(t)
}

// TestSignedBundleArtifactPath: the artifact flow. The packager signs a bundle, stores it under its
// sha256 digest in the runtime's bundle_dir, and the control plane's revision references that digest.
// The runtime verifies the manually placed archive before running it.
func TestSignedBundleArtifactPath(t *testing.T) {
	cp, _ := startSigningControlPlane(t)
	be := newBackend(t)
	cp.registerRuntime(t, "rt-1")
	p := startRuntime(t, cp, "rt-1", "rt-1", be.URL, true)
	doc := flow("signed")
	artifact := p.putSigned(t, "employee", doc)
	comp, digest := cp.component(t, "employee-signed", doc, artifact)
	if digest != artifact {
		t.Fatalf("revision digest %s != artifact digest %s", digest, artifact)
	}
	dep := cp.deploy(t, comp, 1, "rt-1")
	cp.waitStatus(t, dep, "running")
	p.waitServing(t)

	// A digest with no artifact in bundle_dir is reported as failed with the runtime's message.
	comp2, _ := cp.component(t, "missing", flow("missing"), "sha256:"+strings.Repeat("c", 64))
	dep2 := cp.deploy(t, comp2, 1, "rt-1")
	cp.waitStatus(t, dep2, "failed")
	if _, msg := cp.deploymentStatus(t, dep2); !strings.Contains(msg, "artifact not available from the control plane for this runtime") {
		t.Fatalf("unexpected failure: %s", msg)
	}
}

// TestCertificateIdentities: the URI SAN spiffe://nurtuo/runtime/<id> identifies a runtime even when
// the CN is a decoy, and a certificate from a foreign CA is refused at the TLS layer.
func TestCertificateIdentities(t *testing.T) {
	cp := startControlPlane(t, 0, 0)
	be := newBackend(t)
	cp.registerRuntime(t, "rt-san")
	cp.registerRuntime(t, "rt-1")

	rogue := startRuntime(t, cp, "rt-1", "rogue", be.URL, false)
	comp, digest := cp.component(t, "employee", flow("v1"), "")
	rogue.putPlain(t, digest, flow("v1"))
	dep := cp.deploy(t, comp, 1, "rt-1")
	eventually(t, "TLS refusal logged", 15*time.Second, func() bool {
		return strings.Contains(rogue.logs.String(), "reconnecting")
	})
	time.Sleep(300 * time.Millisecond)
	if st, _ := cp.deploymentStatus(t, dep); st != "pending" {
		t.Fatalf("foreign-CA runtime reached the gateway: status %q", st)
	}

	san := startRuntime(t, cp, "rt-san", "rt-san", be.URL, false)
	comp2, digest2 := cp.component(t, "employee-san", flow("san"), "")
	san.putPlain(t, digest2, flow("san"))
	dep2 := cp.deploy(t, comp2, 1, "rt-san")
	cp.waitStatus(t, dep2, "running")
	san.waitServing(t)
}

func startSigningControlPlane(t *testing.T) (*controlPlane, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := bundle.GenerateKey()
	must(t, err)
	path := filepath.Join(t.TempDir(), "signing.key")
	must(t, os.WriteFile(path, []byte(bundle.EncodePrivateKey(priv)), 0o600))
	return startControlPlane(t, 0, 0, "NURTUO_BUNDLE_SIGNING_KEY_FILE="+path), pub
}

func TestFetchArtifactSignedByControlPlane(t *testing.T) {
	cp, pub := startSigningControlPlane(t)
	cp.registerRuntime(t, "rt-1")
	p := startRuntimeWithKey(t, cp, "rt-1", "rt-1", newBackend(t).URL, true, pub)
	comp, _ := cp.component(t, "employee", flow("v1"), "")
	for rev := 1; rev <= 2; rev++ {
		if rev == 2 {
			cp.mustCall(t, "POST", "/v1/components/"+comp+"/revisions", map[string]any{"content": flow("v2")}, 201)
		}
		dep := cp.deploy(t, comp, rev, "rt-1")
		cp.waitStatus(t, dep, "running")
		p.waitServing(t)
		archives, err := filepath.Glob(filepath.Join(p.bundles, "*.bundle"))
		must(t, err)
		if len(archives) != rev {
			t.Fatalf("cached %d archives after revision %d", len(archives), rev)
		}
	}
}

func TestFetchArtifactWrongKeyRejected(t *testing.T) {
	cp, _ := startSigningControlPlane(t)
	testFetchFailure(t, cp, "signature verification failed")
}

func TestFetchArtifactNoSigningKey(t *testing.T) {
	testFetchFailure(t, startControlPlane(t, 0, 0), "no bundle signing key")
}

func testFetchFailure(t *testing.T, cp *controlPlane, want string) {
	t.Helper()
	cp.registerRuntime(t, "rt-1")
	p := startRuntime(t, cp, "rt-1", "rt-1", newBackend(t).URL, true)
	comp, _ := cp.component(t, "employee", flow("v1"), "")
	dep := cp.deploy(t, comp, 1, "rt-1")
	cp.waitStatus(t, dep, "failed")
	if _, msg := cp.deploymentStatus(t, dep); !strings.Contains(msg, want) {
		t.Fatalf("failure = %q, want %q", msg, want)
	}
	files, err := os.ReadDir(p.bundles)
	must(t, err)
	if len(files) != 0 {
		t.Fatalf("failed download left %d cache files", len(files))
	}
	p.waitRefused(t)
}

func TestFetchArtifactForeignDigestNotFound(t *testing.T) {
	cp, pub := startSigningControlPlane(t)
	cp.registerRuntime(t, "rt-1")
	cp.registerRuntime(t, "rt-2")
	p := startRuntimeWithKey(t, cp, "rt-1", "rt-1", newBackend(t).URL, true, pub)
	comp, _ := cp.component(t, "employee", flow("v1"), "")
	cp.waitStatus(t, cp.deploy(t, comp, 1, "rt-1"), "running")
	archives, err := os.ReadDir(p.bundles)
	must(t, err)
	if len(archives) != 1 || !strings.HasSuffix(archives[0].Name(), ".bundle") {
		t.Fatal("expected one fetched archive")
	}
	digest := "sha256:" + strings.TrimSuffix(archives[0].Name(), ".bundle")
	creds, err := gateway.MTLSCredentials(filepath.Join(certsDir(), "rt-2.pem"), filepath.Join(certsDir(), "rt-2.key"), filepath.Join(certsDir(), "ca.pem"), "localhost")
	must(t, err)
	conn, err := grpc.NewClient(cp.gatewayAddr(), grpc.WithTransportCredentials(creds))
	must(t, err)
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := gatewayv1.NewRuntimeGatewayServiceClient(conn).FetchArtifact(ctx, &gatewayv1.FetchArtifactRequest{Digest: digest})
	must(t, err)
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("foreign digest returned %v, want NotFound", err)
	}
}
