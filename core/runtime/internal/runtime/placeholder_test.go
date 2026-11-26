// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
)

// TestNoBundleDirNeverRunning: a deployment with no bundle source is never reported running (#58).
func TestNoBundleDirNeverRunning(t *testing.T) {
	dep := newUnconfiguredDeployer()
	ctx := context.Background()
	if err := dep.Apply(ctx, supervisor.Deployment{ID: "d1"}); err == nil || !strings.Contains(err.Error(), "bundle_dir") {
		t.Fatalf("Apply error = %v, want bundle_dir message", err)
	}
	if err := dep.Remove(ctx, "unknown"); err != nil {
		t.Fatalf("Remove unknown: %v", err)
	}

	var mu sync.Mutex
	var got []supervisor.Status
	report := func(st supervisor.Status) { mu.Lock(); got = append(got, st); mu.Unlock() }
	s := supervisor.New(dep)
	desired := []supervisor.Deployment{{ID: "d1", BundleDigest: "sha256:aa"}}
	for i := 0; i < 3; i++ { // repeated desired state / reconnect
		if err := s.Reconcile(ctx, 1, desired, report); err == nil {
			t.Fatal("Reconcile must fail without a bundle source")
		}
		s.ReportChanges(ctx, report)
	}
	if s.Revision() != 0 {
		t.Fatalf("revision advanced to %d despite failure", s.Revision())
	}
	if a, _ := dep.Actual(ctx); len(a) != 0 {
		t.Fatalf("Actual = %v, want empty", a)
	}
	if len(got) != 6 {
		t.Fatalf("statuses = %+v, want 3x(applying,error)", got)
	}
	for i, st := range got {
		want := supervisor.StateApplying
		if i%2 == 1 {
			want = supervisor.StateError
			if !strings.Contains(st.Message, "bundle_dir") {
				t.Errorf("status %d message = %q", i, st.Message)
			}
		}
		if st.State != want || st.State == supervisor.StateRunning {
			t.Errorf("status %d = %+v, want %s", i, st, want)
		}
	}
}

type placeholderGateway struct {
	gatewayv1.UnimplementedRuntimeGatewayServiceServer
	statuses chan *gatewayv1.DeploymentStatus
	desired  *gatewayv1.DesiredState
}

func (g *placeholderGateway) Connect(stream gatewayv1.RuntimeGatewayService_ConnectServer) error {
	hello, err := stream.Recv()
	if err != nil {
		return err
	}
	desired := g.desired
	if desired == nil {
		desired = &gatewayv1.DesiredState{
			Revision: 1, Deployments: []*gatewayv1.Deployment{{DeploymentId: "d1", BundleDigest: "sha256:aa"}},
		}
	}
	// Match the control-plane gateway's Hello replay rule.
	if hello.GetHello().GetDesiredStateRevision() != desired.GetRevision() {
		if err := stream.Send(&gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_DesiredState{DesiredState: desired}}); err != nil {
			return err
		}
	}
	for {
		m, err := stream.Recv()
		if err != nil {
			return err
		}
		if x, ok := m.Msg.(*gatewayv1.ConnectRequest_DeploymentStatus); ok {
			g.statuses <- x.DeploymentStatus
		}
	}
}

// RunWith with an empty BundleDir must report error (not running) for a delivered deployment.
func TestRunWithoutBundleDirReportsError(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	g := &placeholderGateway{statuses: make(chan *gatewayv1.DeploymentStatus, 32)}
	gs := grpc.NewServer()
	gatewayv1.RegisterRuntimeGatewayServiceServer(gs, g)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := Config{RuntimeID: "r", ControlPlaneURL: "bufnet", CertFile: "c", KeyFile: "k", CAFile: "ca"}
	go func() {
		done <- RunWith(ctx, cfg, Options{Gateway: func(Config) (gateway.Config, error) {
			return gateway.Config{
				Target: "passthrough:///bufnet",
				DialOpts: []grpc.DialOption{
					grpc.WithTransportCredentials(insecure.NewCredentials()),
					grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
				},
			}, nil
		}})
	}()

	var sawError bool
	timeout := time.After(10 * time.Second)
	for !sawError {
		select {
		case st := <-g.statuses:
			if strings.EqualFold(st.State, supervisor.StateRunning) {
				t.Fatalf("reported running: %v", st)
			}
			if strings.EqualFold(st.State, supervisor.StateError) {
				if !strings.Contains(st.Message, "bundle_dir") {
					t.Fatalf("error message = %q", st.Message)
				}
				sawError = true
			}
		case <-timeout:
			t.Fatal("no error status received")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWith = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWith did not stop")
	}
}

type injectedDeployer struct{ applied chan string }

func (*injectedDeployer) Actual(context.Context) (map[string]supervisor.Deployment, error) {
	return map[string]supervisor.Deployment{}, nil
}
func (d *injectedDeployer) Apply(_ context.Context, dp supervisor.Deployment) error {
	d.applied <- dp.ID
	return nil
}
func (*injectedDeployer) Remove(context.Context, string) error { return nil }

// An injected Options.Deployer is used as before, even without bundle_dir.
func TestRunWithDeployerOverrideUnchanged(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	g := &placeholderGateway{statuses: make(chan *gatewayv1.DeploymentStatus, 32)}
	gs := grpc.NewServer()
	gatewayv1.RegisterRuntimeGatewayServiceServer(gs, g)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	inj := &injectedDeployer{applied: make(chan string, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := Config{RuntimeID: "r", ControlPlaneURL: "bufnet", CertFile: "c", KeyFile: "k", CAFile: "ca"}
	go func() {
		done <- RunWith(ctx, cfg, Options{Deployer: inj, Gateway: func(Config) (gateway.Config, error) {
			return gateway.Config{
				Target: "passthrough:///bufnet",
				DialOpts: []grpc.DialOption{
					grpc.WithTransportCredentials(insecure.NewCredentials()),
					grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
				},
			}, nil
		}})
	}()
	select {
	case id := <-inj.applied:
		if id != "d1" {
			t.Fatalf("applied %q", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("injected deployer not used")
	}
	cancel()
	// RunWith returns the cancellation error; not under test here.
	if err := <-done; err != nil {
		t.Logf("RunWith returned after cancel: %v", err)
	}
}
