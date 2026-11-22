// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// recorder is a Handler that records the props and body of every delivered document.
type recorder struct {
	mu    sync.Mutex
	props []map[string]string
	body  []string
}

func (r *recorder) handle(_ context.Context, in *docs.Set) (*engine.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := in.At(0)
	b, _ := readDoc(d)
	r.props = append(r.props, d.Props)
	r.body = append(r.body, string(b))
	return &engine.Result{}, nil
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.props)
}

func startAuth(t *testing.T, cfg HTTPConfig) (*recorder, string) {
	t.Helper()
	cfg.Path = "/hook"
	l, err := NewHTTP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if err := l.Start(context.Background(), rec.handle); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Stop(context.Background()) })
	scheme := "http"
	if cfg.TLS != nil {
		scheme = "https"
	}
	return rec, scheme + "://" + l.Status().Addr + "/hook"
}

func send(t *testing.T, c *http.Client, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestHTTPBearerAuth(t *testing.T) {
	rec, url := startAuth(t, HTTPConfig{Auth: &HTTPAuth{Type: flow.AuthBearer, Secret: []byte("tok-123")}})
	for _, h := range []map[string]string{
		nil,
		{"Authorization": "Bearer wrong"},
		{"Authorization": "Bearer tok-1234"},
		{"Authorization": "Basic tok-123"},
		{"Authorization": "tok-123"},
		{"X-API-Key": "tok-123"},
	} {
		resp := send(t, nil, url, "x", h)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%v: status %d, want 401", h, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("%v: no WWW-Authenticate", h)
		}
	}
	if rec.calls() != 0 {
		t.Fatalf("flow ran %d times for unauthenticated requests", rec.calls())
	}
	resp := send(t, nil, url, "x", map[string]string{"Authorization": "bearer tok-123", "X-Other": "kept"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	p := rec.props[0]
	if _, ok := p[PropHeaderPrefix+"authorization"]; ok {
		t.Fatal("authorization header leaked into document props")
	}
	if p[PropHeaderPrefix+"x-other"] != "kept" {
		t.Fatalf("other headers dropped: %v", p)
	}
}

func TestHTTPAPIKeyAuth(t *testing.T) {
	_, url := startAuth(t, HTTPConfig{Auth: &HTTPAuth{Type: flow.AuthAPIKey, Secret: []byte("k-1")}})
	if s := send(t, nil, url, "", map[string]string{"X-API-Key": "k-2"}).StatusCode; s != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", s)
	}
	if s := send(t, nil, url, "", map[string]string{"x-api-key": "k-1"}).StatusCode; s != http.StatusNoContent {
		t.Fatalf("right key: %d", s)
	}

	rec, url := startAuth(t, HTTPConfig{Auth: &HTTPAuth{Type: flow.AuthAPIKey, Secret: []byte("k-1"), Header: "x-hook-token"}})
	if s := send(t, nil, url, "", map[string]string{"X-API-Key": "k-1"}).StatusCode; s != http.StatusUnauthorized {
		t.Fatalf("default header accepted with custom header configured: %d", s)
	}
	if s := send(t, nil, url, "", map[string]string{"X-Hook-Token": "k-1"}).StatusCode; s != http.StatusNoContent {
		t.Fatalf("custom header: %d", s)
	}
	if _, ok := rec.props[0][PropHeaderPrefix+"x-hook-token"]; ok {
		t.Fatal("api key leaked into document props")
	}
}

func TestHTTPHMACAuth(t *testing.T) {
	key := []byte("whsec")
	body := `{"event":"push"}`
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	good := "sha256=" + hex.EncodeToString(m.Sum(nil))

	rec, url := startAuth(t, HTTPConfig{Auth: &HTTPAuth{Type: flow.AuthHMAC, Secret: key, Header: "X-Hub-Signature-256", Prefix: "sha256="}})
	for _, sig := range []string{"", strings.TrimPrefix(good, "sha256="), good[:len(good)-2] + "00", "sha256=zz", good + "00"} {
		if s := send(t, nil, url, body, map[string]string{"X-Hub-Signature-256": sig}).StatusCode; s != http.StatusUnauthorized {
			t.Fatalf("sig %q: status %d, want 401", sig, s)
		}
	}
	if s := send(t, nil, url, body+" ", map[string]string{"X-Hub-Signature-256": good}).StatusCode; s != http.StatusUnauthorized {
		t.Fatalf("tampered body accepted: %d", s)
	}
	if rec.calls() != 0 {
		t.Fatal("flow ran for a bad signature")
	}
	if s := send(t, nil, url, body, map[string]string{"X-Hub-Signature-256": good}).StatusCode; s != http.StatusNoContent {
		t.Fatalf("good signature: %d", s)
	}
	if rec.body[0] != body {
		t.Fatalf("body = %q", rec.body[0])
	}
	if rec.props[0][PropHeaderPrefix+"x-hub-signature-256"] != good {
		t.Fatal("signature header (not a secret) should stay visible to the flow")
	}

	// sha512, base64, default header.
	m5 := hmac.New(sha512.New, key)
	m5.Write([]byte(body))
	_, url = startAuth(t, HTTPConfig{Auth: &HTTPAuth{Type: flow.AuthHMAC, Secret: key, Algorithm: "sha512", Encoding: "base64"}})
	if s := send(t, nil, url, body, map[string]string{DefaultHMACHeader: base64.StdEncoding.EncodeToString(m5.Sum(nil))}).StatusCode; s != http.StatusNoContent {
		t.Fatalf("sha512/base64: %d", s)
	}
	if s := send(t, nil, url, body, map[string]string{DefaultHMACHeader: hex.EncodeToString(m5.Sum(nil))}).StatusCode; s != http.StatusUnauthorized {
		t.Fatalf("hex accepted for base64 encoding: %d", s)
	}
}

func TestHTTPAuthConfigErrors(t *testing.T) {
	for _, a := range []HTTPAuth{
		{Type: flow.AuthBearer},
		{Type: "basic", Secret: []byte("x")},
		{Type: flow.AuthHMAC, Secret: []byte("x"), Algorithm: "md5"},
		{Type: flow.AuthHMAC, Secret: []byte("x"), Encoding: "base32"},
	} {
		a := a
		if _, err := NewHTTP(HTTPConfig{Path: "/x", Auth: &a}); err == nil {
			t.Fatalf("%+v accepted", a)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:0": true, "127.0.0.2:80": true, "[::1]:8080": true, "localhost:1": true, "LOCALHOST:1": true,
		"0.0.0.0:80": false, ":8080": false, "[::]:80": false, "10.0.0.5:80": false, "example.com:80": false, "bad": false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestHTTPRefusesPublicBindWithoutAuth(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		_, err := NewHTTP(HTTPConfig{Addr: addr, Path: "/x"})
		if err == nil || !strings.Contains(err.Error(), "insecure") {
			t.Fatalf("%s: err = %v, want refusal mentioning insecure", addr, err)
		}
		if _, err := NewHTTP(HTTPConfig{Addr: addr, Path: "/x", Insecure: true}); err != nil {
			t.Fatalf("%s insecure: %v", addr, err)
		}
		if _, err := NewHTTP(HTTPConfig{Addr: addr, Path: "/x", Auth: &HTTPAuth{Type: flow.AuthBearer, Secret: []byte("t")}}); err != nil {
			t.Fatalf("%s with auth: %v", addr, err)
		}
	}
	if _, err := NewHTTP(HTTPConfig{Path: "/x"}); err != nil {
		t.Fatalf("default loopback address refused: %v", err)
	}
}

// selfSigned returns PEM cert/key for 127.0.0.1 and a pool trusting it.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte, pool *x509.CertPool) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nurtuo-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(c)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), pool
}

type mapSecrets map[string]string

func (m mapSecrets) ResolveRef(_ context.Context, ref string) (secrets.Secret, error) {
	name, ok := strings.CutPrefix(ref, secrets.Ref)
	if !ok {
		return secrets.Secret{}, fmt.Errorf("%q is not a reference", ref)
	}
	v, ok := m[name]
	if !ok {
		return secrets.Secret{}, fmt.Errorf("secret %q not found", name)
	}
	return secrets.New(v), nil
}

func TestHTTPTLSFromStart(t *testing.T) {
	certPEM, keyPEM, pool := selfSigned(t)
	sc := flow.StartConfig{Type: "listener", Connector: "http", Path: "/hook",
		Auth: &flow.ListenerAuth{Type: flow.AuthBearer, Secret: "secret://hook-token"},
		TLS:  &flow.ListenerTLS{Cert: "secret://tls.crt", Key: "secret://tls.key"}}
	deps := FactoryDeps{Secrets: mapSecrets{"hook-token": "tok", "tls.crt": string(certPEM), "tls.key": string(keyPEM)}}
	l, err := FromStart(context.Background(), sc, deps)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if err := l.Start(context.Background(), rec.handle); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Stop(context.Background()) })
	addr := l.Status().Addr

	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if s := send(t, c, "https://"+addr+"/hook", "hi", map[string]string{"Authorization": "Bearer tok"}).StatusCode; s != http.StatusNoContent {
		t.Fatalf("https: status %d", s)
	}
	if s := send(t, c, "https://"+addr+"/hook", "hi", nil).StatusCode; s != http.StatusUnauthorized {
		t.Fatalf("https without token: status %d", s)
	}

	// TLS 1.1 is refused.
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}}}
	if resp, err := old.Post("https://"+addr+"/hook", "text/plain", strings.NewReader("x")); err == nil {
		_ = resp.Body.Close()
		t.Fatal("TLS 1.1 handshake succeeded")
	}
	// Plain HTTP gets no flow run.
	if resp, err := http.Post("http://"+addr+"/hook", "text/plain", strings.NewReader("x")); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("plain http accepted: %d", resp.StatusCode)
		}
	}
	if rec.calls() != 1 {
		t.Fatalf("flow ran %d times, want 1", rec.calls())
	}
}

func TestFromStartAuthSecretErrors(t *testing.T) {
	ctx := context.Background()
	auth := &flow.ListenerAuth{Type: flow.AuthAPIKey, Secret: "secret://k"}
	if _, err := FromStart(ctx, flow.StartConfig{Type: "listener", Path: "/x", Auth: auth}, FactoryDeps{}); err == nil {
		t.Fatal("auth without a secret provider accepted")
	}
	if _, err := FromStart(ctx, flow.StartConfig{Type: "listener", Path: "/x", Auth: auth}, FactoryDeps{Secrets: mapSecrets{}}); err == nil || !strings.Contains(err.Error(), `"k"`) {
		t.Fatalf("missing secret: %v", err)
	}
	l, err := FromStart(ctx, flow.StartConfig{Type: "listener", Path: "/x", Auth: auth}, FactoryDeps{Secrets: mapSecrets{"k": "v"}, HTTPAddr: "0.0.0.0:0"})
	if err != nil {
		t.Fatalf("public bind with auth: %v", err)
	}
	if h := l.(*httpListener); h.cfg.Auth == nil || string(h.cfg.Auth.Secret) != "v" || h.cfg.Auth.Header != "X-Api-Key" {
		t.Fatalf("auth = %+v", h.cfg.Auth)
	}
	if _, err := FromStart(ctx, flow.StartConfig{Type: "listener", Path: "/x"}, FactoryDeps{HTTPAddr: "0.0.0.0:0"}); err == nil {
		t.Fatal("public bind without auth accepted")
	}
	if _, err := FromStart(ctx, flow.StartConfig{Type: "listener", Path: "/x", Insecure: true}, FactoryDeps{HTTPAddr: "0.0.0.0:0"}); err != nil {
		t.Fatalf("insecure public bind: %v", err)
	}
	bad := flow.StartConfig{Type: "listener", Path: "/x", TLS: &flow.ListenerTLS{Cert: "secret://c", Key: "secret://k"}}
	_, err = FromStart(ctx, bad, FactoryDeps{Secrets: mapSecrets{"c": "not pem", "k": "PRIVATE-KEY-MATERIAL"}})
	if err == nil || strings.Contains(err.Error(), "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("bad pem: %v", err)
	}
}
