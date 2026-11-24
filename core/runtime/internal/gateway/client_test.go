// SPDX-License-Identifier: AGPL-3.0-only

package gateway_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nurtuo/nurtuo/core/runtime/internal/gateway"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
)

// ---- fake in-process gateway server ----

type fakeServer struct {
	gatewayv1.UnimplementedRuntimeGatewayServiceServer
	mu       sync.Mutex
	desired  *gatewayv1.DesiredState
	hellos   chan *gatewayv1.Hello
	statuses chan *gatewayv1.DeploymentStatus
	results  chan *gatewayv1.CommandResult
	beats    chan struct{}
	push     chan *gatewayv1.ConnectResponse // server-initiated messages
	drop     chan struct{}                   // close the current stream
	certCN   chan string
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		hellos: make(chan *gatewayv1.Hello, 8), statuses: make(chan *gatewayv1.DeploymentStatus, 32),
		results: make(chan *gatewayv1.CommandResult, 8), beats: make(chan struct{}, 8),
		push: make(chan *gatewayv1.ConnectResponse, 8), drop: make(chan struct{}, 1), certCN: make(chan string, 8),
	}
}

func (f *fakeServer) Connect(stream gatewayv1.RuntimeGatewayService_ConnectServer) error {
	if p, ok := peer.FromContext(stream.Context()); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			f.certCN <- ti.State.PeerCertificates[0].Subject.CommonName
		}
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return context.Canceled
	}
	f.hellos <- hello
	f.mu.Lock()
	ds := f.desired
	f.mu.Unlock()
	if ds != nil && ds.Revision != hello.DesiredStateRevision {
		if err := stream.Send(&gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_DesiredState{DesiredState: ds}}); err != nil {
			return err
		}
	}
	go func() {
		for {
			select {
			case m := <-f.push:
				_ = stream.Send(m)
			case <-stream.Context().Done():
				return
			}
		}
	}()
	recv := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				recv <- err
				return
			}
			switch x := m.Msg.(type) {
			case *gatewayv1.ConnectRequest_DeploymentStatus:
				f.statuses <- x.DeploymentStatus
			case *gatewayv1.ConnectRequest_CommandResult:
				f.results <- x.CommandResult
			case *gatewayv1.ConnectRequest_Heartbeat:
				select {
				case f.beats <- struct{}{}:
				default:
				}
			}
		}
	}()
	select {
	case <-f.drop:
		return nil
	case err := <-recv:
		return err
	}
}

// ---- fake deployer ----

type fakeDeployer struct {
	mu      sync.Mutex
	running map[string]supervisor.Deployment
}

func (f *fakeDeployer) Actual(context.Context) (map[string]supervisor.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]supervisor.Deployment{}
	for k, v := range f.running {
		out[k] = v
	}
	return out, nil
}
func (f *fakeDeployer) Apply(_ context.Context, d supervisor.Deployment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[d.ID] = d
	return nil
}
func (f *fakeDeployer) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, id)
	return nil
}
func (f *fakeDeployer) has(id, digest string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.running[id]
	return ok && d.BundleDigest == digest
}

// ---- test PKI ----

func writePKI(t *testing.T, dir, runtimeID string) (srv tls.Certificate, pool *x509.CertPool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	issue := func(cn string, usage x509.ExtKeyUsage, serial int64) (certPEM, keyPEM []byte) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kd, _ := x509.MarshalECPrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
	}
	sc, sk := issue("gateway", x509.ExtKeyUsageServerAuth, 2)
	cc, ck := issue(runtimeID, x509.ExtKeyUsageClientAuth, 3)
	srv, err := tls.X509KeyPair(sc, sk)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(caCert)
	w := func(n string, b []byte) { _ = os.WriteFile(filepath.Join(dir, n), b, 0o600) }
	w("client.pem", cc)
	w("client.key", ck)
	w("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return srv, pool
}

func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		panic("unreachable")
	}
}

func TestHandshakeReconcileCommandReconnect(t *testing.T) {
	dir := t.TempDir()
	srvCert, pool := writePKI(t, dir, "rt-1")
	creds, err := gateway.MTLSCredentials(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.pem"), "localhost")
	if err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	fs := newFakeServer()
	fs.desired = &gatewayv1.DesiredState{Revision: 5, Deployments: []*gatewayv1.Deployment{{DeploymentId: "d1", BundleDigest: "sha256:aa", QueueIds: []string{"q"}}}}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{srvCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	})))
	gatewayv1.RegisterRuntimeGatewayServiceServer(gs, fs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	fd := &fakeDeployer{running: map[string]supervisor.Deployment{}}
	h := &supervisor.Handler{S: supervisor.New(fd), Commands: map[string]supervisor.CommandFunc{
		"run_now": func(context.Context, map[string]string) error { return nil },
	}}
	c := gateway.NewClient(gateway.Config{
		Target: "passthrough:///bufnet", RuntimeID: "rt-1", Version: "test", TLS: creds, Handler: h,
		Heartbeat: 20 * time.Millisecond,
		Backoff:   gateway.Backoff{Initial: 10 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2},
		DialOpts:  []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })},
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-runDone; err != nil {
			t.Logf("Run returned: %v", err)
		}
	})

	// Handshake: identity from mTLS cert, Hello with revision 0, server pushes DesiredState.
	if cn := within(t, fs.certCN, "client cert"); cn != "rt-1" {
		t.Fatalf("client cert CN = %q", cn)
	}
	hello := within(t, fs.hellos, "hello")
	if hello.RuntimeId != "rt-1" || hello.DesiredStateRevision != 0 {
		t.Fatalf("hello = %v", hello)
	}
	// Reconcile cycle: applying -> running statuses, deployment started.
	if s := within(t, fs.statuses, "status"); s.State != supervisor.StateApplying || s.DeploymentId != "d1" || s.Revision != 5 {
		t.Fatalf("status = %v", s)
	}
	if s := within(t, fs.statuses, "status"); s.State != supervisor.StateRunning {
		t.Fatalf("status = %v", s)
	}
	if !fd.has("d1", "sha256:aa") {
		t.Fatal("deployment not started")
	}
	within(t, fs.beats, "heartbeat")

	// Commands: known and unknown.
	fs.push <- &gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_Command{Command: &gatewayv1.Command{CommandId: "c1", Name: "run_now"}}}
	if r := within(t, fs.results, "result"); r.CommandId != "c1" || !r.Ok {
		t.Fatalf("result = %v", r)
	}
	fs.push <- &gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_Command{Command: &gatewayv1.Command{CommandId: "c2", Name: "bogus"}}}
	if r := within(t, fs.results, "result"); r.CommandId != "c2" || r.Ok {
		t.Fatalf("unknown command should fail: %v", r)
	}

	// Disconnect; the client reconnects with backoff and reports the applied revision, so the
	// server (same revision) sends nothing. Then a new revision removes d1 and adds d2.
	fs.mu.Lock()
	fs.desired = &gatewayv1.DesiredState{Revision: 6, Deployments: []*gatewayv1.Deployment{{DeploymentId: "d2", BundleDigest: "sha256:bb"}}}
	fs.mu.Unlock()
	fs.drop <- struct{}{}
	within(t, fs.certCN, "reconnect cert")
	hello = within(t, fs.hellos, "second hello")
	if hello.DesiredStateRevision != 5 {
		t.Fatalf("reconnect hello revision = %d, want 5", hello.DesiredStateRevision)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !fd.has("d2", "sha256:bb") || fd.has("d1", "sha256:aa") {
		if time.Now().After(deadline) {
			t.Fatal("reconcile to revision 6 did not converge")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.Revision() != 6 {
		t.Fatalf("revision = %d", h.Revision())
	}
}

func TestMTLSRejectsClientWithoutCert(t *testing.T) {
	dir := t.TempDir()
	srvCert, pool := writePKI(t, dir, "rt-1")
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{srvCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	})))
	gatewayv1.RegisterRuntimeGatewayServiceServer(gs, newFakeServer())
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	noCert := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(noCert),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := gatewayv1.NewRuntimeGatewayServiceClient(conn).Connect(ctx)
	if err == nil {
		_ = stream.Send(&gatewayv1.ConnectRequest{Msg: &gatewayv1.ConnectRequest_Hello{Hello: &gatewayv1.Hello{RuntimeId: "rt-1"}}})
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatal("expected failure without a client certificate")
	}
}

// ---- apply runs off the receive loop ----

// blockingDeployer blocks in Apply of the first deployment until release is closed.
type blockingDeployer struct {
	fakeDeployer
	started chan string   // deployment digest, sent when Apply is entered
	release chan struct{} // closed to let blocked Applies finish
	paused  atomic.Bool
	once    sync.Once
	applied []string // digests, in order
}

func (b *blockingDeployer) Apply(ctx context.Context, d supervisor.Deployment) error {
	b.started <- d.BundleDigest
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	b.applied = append(b.applied, d.BundleDigest)
	b.mu.Unlock()
	return b.fakeDeployer.Apply(ctx, d)
}

func (b *blockingDeployer) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *blockingDeployer) State(string) (string, string) {
	if b.paused.Load() {
		return supervisor.StatePaused, ""
	}
	return supervisor.StateRunning, ""
}

func startSlowApplyClient(t *testing.T) (*fakeServer, *blockingDeployer, *supervisor.Handler) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	fs := newFakeServer()
	gs := grpc.NewServer()
	gatewayv1.RegisterRuntimeGatewayServiceServer(gs, fs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	bd := &blockingDeployer{
		fakeDeployer: fakeDeployer{running: map[string]supervisor.Deployment{}},
		started:      make(chan string, 8), release: make(chan struct{}),
	}
	t.Cleanup(bd.unblock)
	h := &supervisor.Handler{
		S: supervisor.New(bd),
		Commands: map[string]supervisor.CommandFunc{
			"pause_listener": func(context.Context, map[string]string) error { bd.paused.Store(true); return nil },
		},
		Report: func(*gatewayv1.DeploymentStatus) {},
	}
	c := gateway.NewClient(gateway.Config{
		Target: "passthrough:///bufnet", RuntimeID: "rt-1", Version: "test", Handler: h,
		Heartbeat: time.Hour,
		Backoff:   gateway.Backoff{Initial: 10 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2},
		DialOpts: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		bd.unblock()
		if err := <-runDone; err != nil {
			t.Logf("client run: %v", err)
		}
	})
	within(t, fs.hellos, "hello")
	return fs, bd, h
}

func desired(rev uint64, digest string) *gatewayv1.ConnectResponse {
	return &gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_DesiredState{DesiredState: &gatewayv1.DesiredState{
		Revision: rev, Deployments: []*gatewayv1.Deployment{{DeploymentId: "d1", BundleDigest: digest}},
	}}}
}

func TestCommandHandledDuringSlowApply(t *testing.T) {
	fs, bd, _ := startSlowApplyClient(t)

	fs.push <- desired(1, "sha256:aa")
	if d := within(t, bd.started, "apply start"); d != "sha256:aa" {
		t.Fatalf("applying %q", d)
	}
	// The apply is now blocked in the deployer. The command must still be read and answered.
	fs.push <- &gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_Command{Command: &gatewayv1.Command{CommandId: "c1", Name: "pause_listener"}}}
	if r := within(t, fs.results, "command result during slow apply"); r.CommandId != "c1" || !r.Ok {
		t.Fatalf("result = %v", r)
	}
	if !bd.paused.Load() {
		t.Fatal("pause_listener did not run")
	}
}

func TestLatestDesiredStateWins(t *testing.T) {
	fs, bd, h := startSlowApplyClient(t)

	fs.push <- desired(1, "sha256:r1")
	within(t, bd.started, "apply start")
	// While revision 1 is blocked, 2 and 3 arrive. A command round trip afterwards proves the
	// receive loop has consumed them (messages are handled in order).
	fs.push <- desired(2, "sha256:r2")
	fs.push <- desired(3, "sha256:r3")
	fs.push <- &gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_Command{Command: &gatewayv1.Command{CommandId: "c1", Name: "pause_listener"}}}
	within(t, fs.results, "command result")

	bd.unblock()
	if d := within(t, bd.started, "apply of latest"); d != "sha256:r3" {
		t.Fatalf("second apply = %q, want sha256:r3 (revision 2 must be superseded)", d)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.Revision() != 3 {
		if time.Now().After(deadline) {
			t.Fatalf("revision = %d, want 3", h.Revision())
		}
		time.Sleep(10 * time.Millisecond)
	}
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if want := []string{"sha256:r1", "sha256:r3"}; !slices.Equal(bd.applied, want) {
		t.Fatalf("applied = %v, want %v", bd.applied, want)
	}
}
