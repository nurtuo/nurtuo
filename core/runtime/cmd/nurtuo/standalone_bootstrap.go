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
	"os"
	"path/filepath"
	"time"
)

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
// admin token inside dirs.Secrets. It is idempotent: existing material is reused.
type localBootstrapper struct{ now func() time.Time }

func NewLocalBootstrapper() Bootstrapper { return localBootstrapper{now: time.Now} }

func (b localBootstrapper) Bootstrap(dirs DataDirs) (*Credentials, error) {
	s := dirs.Secrets
	if err := os.MkdirAll(s, 0o700); err != nil {
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
		exists(c.ServerKeyFile) && exists(c.ClientKeyFile) {
		return c, nil
	}
	caKey, caCert, caDER, err := b.newCA()
	if err != nil {
		return nil, err
	}
	if err := writePEM(c.CAFile, "CERTIFICATE", caDER, 0o644); err != nil {
		return nil, err
	}
	if err := b.leaf(caKey, caCert, "nurtuo-control-plane", x509.ExtKeyUsageServerAuth, c.ServerCertFile, c.ServerKeyFile); err != nil {
		return nil, err
	}
	if err := b.leaf(caKey, caCert, "nurtuo-runtime", x509.ExtKeyUsageClientAuth, c.ClientCertFile, c.ClientKeyFile); err != nil {
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

func (b localBootstrapper) leaf(caKey *ecdsa.PrivateKey, ca *x509.Certificate, cn string, eku x509.ExtKeyUsage, certFile, keyFile string) error {
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

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
