// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
)

// standaloneRuntimeID is the runtime id of the local runtime of a standalone install. It is sent
// as Hello.runtime_id and carried in the client certificate as the URI SAN
// spiffe://nurtuo/runtime/<id>, which the control plane gateway takes as the runtime identity
// (it rejects a Hello whose runtime_id differs from the certificate identity).
const standaloneRuntimeID = "standalone"

// standaloneRuntimeURI is the SPIFFE id of the standalone runtime (client certificate URI SAN).
func standaloneRuntimeURI() *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "nurtuo", Path: "/runtime/" + standaloneRuntimeID}
}

// Credentials are the first-run secrets of a standalone install.
type Credentials struct {
	CAFile, ServerCertFile, ServerKeyFile, ClientCertFile, ClientKeyFile string
	DBPassword                                                           string
	AdminToken                                                           string
}

// Bootstrapper creates (first run) or loads (later runs) the install's secrets.
type Bootstrapper interface {
	Bootstrap(dirs DataDirs) (*Credentials, error)
}

// localBootstrapper generates a local ECDSA CA, server and client certs, a DB password and an
// admin token inside dirs.Secrets. It is idempotent: existing material is reused. Installs created
// before the client certificate carried the runtime URI SAN get a fresh CA, server and client
// certificate on the next bootstrap (the CA key is never persisted, so the client certificate
// cannot be reissued alone); the DB password and admin token are kept.
type localBootstrapper struct{ now func() time.Time }

func NewLocalBootstrapper() Bootstrapper { return localBootstrapper{now: time.Now} }

func (b localBootstrapper) Bootstrap(dirs DataDirs) (*Credentials, error) {
	s := dirs.Secrets
	// Every bootstrap file is written and read below a verified private directory, so new files
	// inherit its private DACL on Windows.
	if _, err := secrets.EnsurePrivateDir(s); err != nil {
		return nil, err
	}
	c := &Credentials{
		CAFile:         filepath.Join(s, "ca.pem"),
		ServerCertFile: filepath.Join(s, "server.pem"),
		ServerKeyFile:  filepath.Join(s, "server.key"),
		ClientCertFile: filepath.Join(s, "client.pem"),
		ClientKeyFile:  filepath.Join(s, "client.key"),
	}
	var err error
	if c.DBPassword, err = loadOrGenSecret(filepath.Join(s, "db-password")); err != nil {
		return nil, err
	}
	if c.AdminToken, err = loadOrGenSecret(filepath.Join(s, "admin-token")); err != nil {
		return nil, err
	}
	if exists(c.CAFile) && exists(c.ServerCertFile) && exists(c.ClientCertFile) &&
		exists(c.ServerKeyFile) && exists(c.ClientKeyFile) && hasURISAN(c.ClientCertFile, standaloneRuntimeURI()) {
		return c, nil
	}
	caKey, caCert, caDER, err := b.newCA()
	if err != nil {
		return nil, err
	}
	if err := writePEM(c.CAFile, "CERTIFICATE", caDER, 0o644); err != nil {
		return nil, err
	}
	if err := b.leaf(caKey, caCert, "nurtuo-control-plane", x509.ExtKeyUsageServerAuth, nil, c.ServerCertFile, c.ServerKeyFile); err != nil {
		return nil, err
	}
	if err := b.leaf(caKey, caCert, "nurtuo-runtime", x509.ExtKeyUsageClientAuth, []*url.URL{standaloneRuntimeURI()}, c.ClientCertFile, c.ClientKeyFile); err != nil {
		return nil, err
	}
	return c, nil
}

func (b localBootstrapper) newCA() (*ecdsa.PrivateKey, *x509.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Nurtuo local CA"},
		NotBefore: b.now().Add(-time.Hour), NotAfter: b.now().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return key, cert, der, err
}

func (b localBootstrapper) leaf(caKey *ecdsa.PrivateKey, ca *x509.Certificate, cn string, eku x509.ExtKeyUsage, uris []*url.URL, certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: b.now().Add(-time.Hour), NotAfter: b.now().AddDate(2, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		URIs: uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	return writePEM(keyFile, "EC PRIVATE KEY", kd, 0o600)
}

func loadOrGenSecret(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return string(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	v := hex.EncodeToString(raw)
	return v, os.WriteFile(path, []byte(v), 0o600)
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

// hasURISAN reports whether the PEM certificate at path carries want as a URI SAN.
func hasURISAN(path string, want *url.URL) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return false
	}
	for _, u := range cert.URIs {
		if u.String() == want.String() {
			return true
		}
	}
	return false
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
