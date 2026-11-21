// SPDX-License-Identifier: AGPL-3.0-only

// Package e2e drives the whole runtime chain in one process: a fake control-plane gateway
// (real TCP + mTLS) -> internal/runtime.RunWith -> supervisor -> EngineDeployer -> HTTP listener
// -> engine -> HTTP connector -> httptest backend. No external services are used.
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/structpb"

	httpconn "github.com/nurtuo/nurtuo/connectors/http"
	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/runtime"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
)

// ---- fake control-plane gateway ----

type gw struct {
	gatewayv1.UnimplementedRuntimeGatewayServiceServer
	mu       sync.Mutex
	desired  *gatewayv1.DesiredState
	push     chan *gatewayv1.ConnectResponse
	statuses chan *gatewayv1.DeploymentStatus
	results  chan *gatewayv1.CommandResult
	hellos   chan *gatewayv1.Hello
	// transcript is the text of every status / command result the runtime sent (guarded by mu).
	transcript []string
}

// sent returns everything the runtime reported upstream so far, as one string.
func (g *gw) sent() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.transcript, "\n")
}

func newGW() *gw {
	return &gw{push: make(chan *gatewayv1.ConnectResponse, 16), statuses: make(chan *gatewayv1.DeploymentStatus, 64),
		results: make(chan *gatewayv1.CommandResult, 16), hellos: make(chan *gatewayv1.Hello, 8)}
}

func (g *gw) Connect(stream gatewayv1.RuntimeGatewayService_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	g.hellos <- first.GetHello()
	g.mu.Lock()
	ds := g.desired
	g.mu.Unlock()
	if ds != nil {
		if err := stream.Send(&gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_DesiredState{DesiredState: ds}}); err != nil {
			return err
		}
	}
	go func() {
		for {
			select {
			case m := <-g.push:
				_ = stream.Send(m)
			case <-stream.Context().Done():
				return
			}
		}
	}()
	for {
		m, err := stream.Recv()
		if err != nil {
			return err
		}
		switch x := m.Msg.(type) {
		case *gatewayv1.ConnectRequest_DeploymentStatus:
			g.mu.Lock()
			g.transcript = append(g.transcript, x.DeploymentStatus.String())
			g.mu.Unlock()
			g.statuses <- x.DeploymentStatus
		case *gatewayv1.ConnectRequest_CommandResult:
			g.mu.Lock()
			g.transcript = append(g.transcript, x.CommandResult.String())
			g.mu.Unlock()
			g.results <- x.CommandResult
		}
	}
}

// setDesired pushes a desired state (and remembers it for reconnects).
func (g *gw) setDesired(rev uint64, deps ...*gatewayv1.Deployment) {
	ds := &gatewayv1.DesiredState{Revision: rev, Deployments: deps}
	g.mu.Lock()
	g.desired = ds
	g.mu.Unlock()
	g.push <- &gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_DesiredState{DesiredState: ds}}
}

func (g *gw) command(id, name string, args map[string]string) *gatewayv1.CommandResult {
	g.push <- &gatewayv1.ConnectResponse{Msg: &gatewayv1.ConnectResponse_Command{Command: &gatewayv1.Command{CommandId: id, Name: name, Args: args}}}
	return recv(g.results, "command result")
}

func recv[T any](ch <-chan T, what string) T {
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		panic("timeout waiting for " + what)
	}
}

// statusFor waits for a status of deployment id in the given state.
func (g *gw) statusFor(t *testing.T, id, state string) *gatewayv1.DeploymentStatus {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case s := <-g.statuses:
			if s.DeploymentId == id && s.State == state {
				return s
			}
		case <-deadline:
			t.Fatalf("no %q status for %s", state, id)
		}
	}
}

// ---- test PKI ----

type pki struct {
	certFile, keyFile, caFile string
	server                    tls.Certificate
	pool                      *x509.CertPool
}

func newPKI(t *testing.T, dir, runtimeID string) pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "e2e-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	issue := func(cn string, usage x509.ExtKeyUsage, serial int64) ([]byte, []byte) {
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
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	p := pki{server: srv, pool: pool, certFile: filepath.Join(dir, "client.pem"), keyFile: filepath.Join(dir, "client.key"), caFile: filepath.Join(dir, "ca.pem")}
	must(t, os.WriteFile(p.certFile, cc, 0o600))
	must(t, os.WriteFile(p.keyFile, ck, 0o600))
	must(t, os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- the whole chain ----

type env struct {
	gw       *gw
	bundles  string
	httpAddr string
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	cancel   context.CancelFunc
	done     chan error
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// startRuntime boots the fake gateway and the real runtime (RunWith + bundle_dir). signed selects
// trusted_keys mode (<sha256>.bundle archives) instead of plain <digest>.json bundles.
func startRuntime(t *testing.T, backendURL string, signed bool) *env {
	t.Helper()
	eng := engine.New()
	conns := engine.NewConnectors()
	settings, _ := structpb.NewStruct(map[string]any{"baseUrl": backendURL, "retries": float64(0), "timeoutMs": float64(5000)})
	opts, _ := structpb.NewStruct(map[string]any{"path": "/employees/42"})
	conns.Register("http", engine.ConnectorEntry{Connector: httpconn.New(), Connection: &connectorv1.ConnectionConfig{Settings: settings}, Options: opts})
	eng.Registry.RegisterConnectorRunner(conns)
	return boot(t, signed, runtime.Options{Engine: eng, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil)
}

// boot starts the fake gateway and RunWith with the given options; tweak may adjust the config.
func boot(t *testing.T, signed bool, o runtime.Options, tweak func(*runtime.Config)) *env {
	t.Helper()
	dir := t.TempDir()
	p := newPKI(t, dir, "rt-e2e")
	g := newGW()
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{p.server}, ClientCAs: p.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	})))
	gatewayv1.RegisterRuntimeGatewayServiceServer(srv, g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	pub, priv, err := bundle.GenerateKey()
	must(t, err)
	e := &env{gw: g, bundles: filepath.Join(dir, "bundles"), httpAddr: freeAddr(t), pub: pub, priv: priv, done: make(chan error, 1)}
	must(t, os.MkdirAll(e.bundles, 0o755))

	cfg := runtime.Config{RuntimeID: "rt-e2e", ControlPlaneURL: ln.Addr().String(), CertFile: p.certFile, KeyFile: p.keyFile,
		CAFile: p.caFile, ServerName: "localhost", BundleDir: e.bundles, HTTPAddr: e.httpAddr}
	if signed {
		cfg.TrustedKeys = []string{bundle.EncodePublicKey(pub)}
	}
	if tweak != nil {
		tweak(&cfg)
	}
	must(t, cfg.Validate())

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() { e.done <- runtime.RunWith(ctx, cfg, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-e.done:
			if err != nil {
				t.Errorf("RunWith: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("runtime did not stop")
		}
	})
	if h := recv(g.hellos, "hello"); h.GetRuntimeId() != "rt-e2e" {
		t.Fatalf("hello runtime id %q", h.GetRuntimeId())
	}
	return e
}

// putJSON stores flow documents as a plain <name>.json bundle and returns its digest.
func (e *env) putJSON(t *testing.T, name string, flows ...string) string {
	t.Helper()
	var raws []json.RawMessage
	for _, f := range flows {
		raws = append(raws, json.RawMessage(f))
	}
	b, _ := json.Marshal(map[string]any{"flows": raws})
	must(t, os.WriteFile(filepath.Join(e.bundles, name+".json"), b, 0o600))
	return "sha256:" + name
}

// putSigned packs and signs flows with key (default the trusted key) and returns the digest.
func (e *env) putSigned(t *testing.T, key ed25519.PrivateKey, flows ...string) string {
	t.Helper()
	if key == nil {
		key = e.priv
	}
	var comps []bundle.Component
	for i, f := range flows {
		var h struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(f), &h)
		id := h.ID
		if id == "" {
			id = "flow" + string(rune('a'+i))
		}
		comps = append(comps, bundle.Component{Kind: "flow", ID: id, Revision: 1, Document: json.RawMessage(f)})
	}
	data, err := bundle.Pack(bundle.Input{Name: "e2e", CreatedAt: time.Unix(1700000000, 0), Components: comps}, key)
	must(t, err)
	return e.writeBundle(t, data)
}

func (e *env) writeBundle(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	hexd := hex.EncodeToString(sum[:])
	must(t, os.WriteFile(filepath.Join(e.bundles, hexd+".bundle"), data, 0o600))
	return "sha256:" + hexd
}

func dep(id, digest string) *gatewayv1.Deployment {
	return &gatewayv1.Deployment{DeploymentId: id, BundleDigest: digest}
}

// employeeFlow: POST /employee -> HTTP connector GET against the backend -> Return.
const employeeFlow = `{"schema":"nurtuo.flow/v1","id":"employee","name":"Employee","revision":1,
"steps":[{"id":"start","kind":"start","config":{"type":"listener","connector":"http","path":"/employee","method":"POST"}},
{"id":"call","kind":"connector","config":{"connector":"http","operation":"GET"}},
{"id":"return","kind":"returnDocuments","config":{}}],
"edges":{"start:default":"call","call:default":"return"}}`
