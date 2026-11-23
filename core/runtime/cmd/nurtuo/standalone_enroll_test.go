// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		t.Fatalf("%s: no PEM block", path)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runtimeIDFromCert mirrors core/control-plane/src/gateway.ts runtimeIdFromCert: the URI SAN
// spiffe://nurtuo/runtime/<id> wins, else the subject CN.
func runtimeIDFromCert(c *x509.Certificate) string {
	for _, u := range c.URIs {
		if u.Scheme == "spiffe" && u.Host == "nurtuo" && strings.HasPrefix(u.Path, "/runtime/") && len(u.Path) > len("/runtime/") {
			return strings.TrimPrefix(u.Path, "/runtime/")
		}
	}
	return c.Subject.CommonName
}

func TestStandaloneClientCertIdentityMatchesHelloRuntimeID(t *testing.T) {
	d := NewDataDirs(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	c, err := NewLocalBootstrapper().Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	cl := readCert(t, c.ClientCertFile)
	if len(cl.URIs) != 1 || cl.URIs[0].String() != "spiffe://nurtuo/runtime/standalone" {
		t.Fatalf("client cert URIs = %v", cl.URIs)
	}
	if cl.Subject.CommonName != "nurtuo-runtime" {
		t.Fatalf("client CN = %q", cl.Subject.CommonName)
	}
	if got := runtimeIDFromCert(cl); got != standaloneRuntimeID {
		t.Fatalf("gateway identity %q != Hello runtime_id %q", got, standaloneRuntimeID)
	}
	if len(cl.ExtKeyUsage) != 1 || cl.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("client EKU = %v", cl.ExtKeyUsage)
	}
	if sv := readCert(t, c.ServerCertFile); len(sv.URIs) != 0 {
		t.Fatalf("server cert must carry no runtime identity: %v", sv.URIs)
	}
}

// An install bootstrapped before #56 has a client cert without the URI SAN; the next bootstrap
// reissues the PKI (the CA key is not kept) and keeps the DB password and admin token.
func TestBootstrapReissuesLegacyClientCertWithoutURISAN(t *testing.T) {
	d := NewDataDirs(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	b := localBootstrapper{now: time.Now}
	c1, err := b.Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the legacy layout: CA + leaves from one CA, client leaf without URIs.
	caKey, caCert, caDER, err := b.newCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(c1.CAFile, "CERTIFICATE", caDER, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.leaf(caKey, caCert, "nurtuo-control-plane", x509.ExtKeyUsageServerAuth, nil, c1.ServerCertFile, c1.ServerKeyFile); err != nil {
		t.Fatal(err)
	}
	if err := b.leaf(caKey, caCert, "nurtuo-runtime", x509.ExtKeyUsageClientAuth, nil, c1.ClientCertFile, c1.ClientKeyFile); err != nil {
		t.Fatal(err)
	}
	legacyCA := readCert(t, c1.CAFile)
	if hasURISAN(c1.ClientCertFile, standaloneRuntimeURI()) {
		t.Fatal("setup: legacy cert has the SAN")
	}

	c2, err := b.Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	if *c1 != *c2 {
		t.Fatal("DB password / admin token / paths changed on reissue")
	}
	if !hasURISAN(c2.ClientCertFile, standaloneRuntimeURI()) {
		t.Fatal("client cert not reissued with the URI SAN")
	}
	ca := readCert(t, c2.CAFile)
	if ca.Equal(legacyCA) {
		t.Fatal("CA not regenerated")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := readCert(t, c2.ClientCertFile).Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client cert does not chain to the new CA: %v", err)
	}
	if _, err := readCert(t, c2.ServerCertFile).Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatalf("server cert does not chain to the new CA: %v", err)
	}
	// Idempotent once fixed.
	before := readCert(t, c2.ClientCertFile)
	if _, err := b.Bootstrap(d); err != nil {
		t.Fatal(err)
	}
	if !readCert(t, c2.ClientCertFile).Equal(before) {
		t.Fatal("bootstrap reissued a cert that already had the SAN")
	}
}

// fakeCP is a minimal control plane REST API: login, runtime get/register, logout.
type fakeCP struct {
	mu       sync.Mutex
	runtimes map[string]map[string]any
	posts    int
	logouts  int
	badAuth  int
}

const fakeJWT = "jwt-abc"

func (f *fakeCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/v1/auth/login" && r.Method == http.MethodPost {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["email"] != standaloneAdminEmail || b["password"] != "secret-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: fakeJWT, Path: "/", HttpOnly: true, Secure: true})
		_, _ = w.Write([]byte(`{"csrfToken":"c"}`))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+fakeJWT {
		f.badAuth++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/v1/auth/logout" && r.Method == http.MethodPost:
		f.logouts++
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(r.URL.Path, "/v1/runtimes/") && r.Method == http.MethodGet:
		rt, ok := f.runtimes[strings.TrimPrefix(r.URL.Path, "/v1/runtimes/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(rt)
	case r.URL.Path == "/v1/runtimes" && r.Method == http.MethodPost:
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.posts++
		f.runtimes[b["id"].(string)] = b
		_ = json.NewEncoder(w).Encode(b)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func TestRegisterStandaloneRuntimeIsIdempotent(t *testing.T) {
	f := &fakeCP{runtimes: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ { // first start + two restarts
		if err := registerStandaloneRuntime(ctx, srv.Client(), srv.URL+"/", standaloneAdminEmail, "secret-pw"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatalf("registered %d times, want 1", f.posts)
	}
	rt := f.runtimes[standaloneRuntimeID]
	if rt == nil || rt["name"] != standaloneRuntimeID || rt["environment"] != standaloneEnvironment {
		t.Fatalf("runtime = %v", rt)
	}
	if f.logouts != 3 || f.badAuth != 0 {
		t.Fatalf("logouts=%d badAuth=%d", f.logouts, f.badAuth)
	}
}

func TestRegisterStandaloneRuntimeErrors(t *testing.T) {
	f := &fakeCP{runtimes: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if err := registerStandaloneRuntime(context.Background(), srv.Client(), srv.URL, standaloneAdminEmail, "wrong"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want login error, got %v", err)
	}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "x"})
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer fail.Close()
	if err := registerStandaloneRuntime(context.Background(), fail.Client(), fail.URL, "a", "b"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want 403 error, got %v", err)
	}
}

func TestStackRegistersRuntimeBeforeStartingIt(t *testing.T) {
	ev := &events{}
	db, svc := &fakeDB{ev: ev}, &fakeService{ev: ev}
	ctx, cancel := context.WithCancel(context.Background())
	s := newStack(t, ev, db, svc, func(ctx context.Context, _ RuntimeEnv) error {
		ev.add("rt.start")
		cancel()
		<-ctx.Done()
		return nil
	})
	s.CPURL = "http://cp/"
	s.Register = func(_ context.Context, env RuntimeEnv) error {
		if env.ControlPlaneURL != "http://cp/" || env.Creds == nil {
			t.Errorf("register env = %+v", env)
		}
		ev.add("rt.register")
		return nil
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := "bootstrap,db.start,cp.start,cp.healthy,rt.register,rt.start,cp.stop,db.stop"
	if got := strings.Join(ev.get(), ","); got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
}

func TestStackRegisterFailureStopsBeforeRuntime(t *testing.T) {
	ev := &events{}
	db, svc := &fakeDB{ev: ev}, &fakeService{ev: ev}
	s := newStack(t, ev, db, svc, func(context.Context, RuntimeEnv) error { t.Error("runtime must not start"); return nil })
	s.Register = func(context.Context, RuntimeEnv) error { return errors.New("nope") }
	if err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "register runtime: nope") {
		t.Fatalf("got %v", err)
	}
	got := ev.get()
	if n := len(got); n < 2 || got[n-2] != "cp.stop" || got[n-1] != "db.stop" {
		t.Fatalf("bad tail: %v", got)
	}
}
