// SPDX-License-Identifier: Apache-2.0

package nats

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Config is the decoded connection configuration. Secret values (token, password, creds, nkey
// seed, TLS key) are resolved by the runtime before they reach the connector.
type Config struct {
	URL      string
	Token    string
	Username string
	Password string
	// Creds is the content of a .creds file (JWT + NKey seed).
	Creds string
	// NKeySeed is a user NKey seed (alternative to Creds).
	NKeySeed string

	TLSEnabled bool
	CAPEM      string
	CertPEM    string
	KeyPEM     string
	ServerName string
	// InsecureSkipVerify disables server certificate verification. Intended for development only.
	InsecureSkipVerify bool

	Name           string
	ConnectTimeout time.Duration
}

// parseConfig decodes ConnectionConfig.settings.
func parseConfig(cc *connectorv1.ConnectionConfig) (*Config, error) {
	if cc == nil || cc.GetSettings() == nil {
		return nil, errors.New("connection settings are required")
	}
	m := cc.GetSettings().AsMap()
	str := func(k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
	c := &Config{
		URL:            str("url"),
		Token:          str("token"),
		Username:       str("username"),
		Password:       str("password"),
		Creds:          str("creds"),
		NKeySeed:       str("nkey_seed"),
		Name:           str("name"),
		ConnectTimeout: 5 * time.Second,
	}
	if c.Name == "" {
		c.Name = "nurtuo"
	}
	if c.URL == "" {
		return nil, errors.New("settings.url is required")
	}
	if v, ok := m["connect_timeout_ms"].(float64); ok && v > 0 {
		c.ConnectTimeout = time.Duration(v) * time.Millisecond
	}
	if t, ok := m["tls"].(map[string]any); ok {
		c.TLSEnabled = true
		if v, ok := t["enabled"].(bool); ok {
			c.TLSEnabled = v
		}
		c.CAPEM, _ = t["ca_pem"].(string)
		c.CertPEM, _ = t["cert_pem"].(string)
		c.KeyPEM, _ = t["key_pem"].(string)
		c.ServerName, _ = t["server_name"].(string)
		c.InsecureSkipVerify, _ = t["insecure_skip_verify"].(bool)
	}
	n := 0
	for _, s := range []string{c.Token, c.Creds, c.NKeySeed} {
		if s != "" {
			n++
		}
	}
	if c.Username != "" {
		n++
	}
	if n > 1 {
		return nil, errors.New("settings: use only one of token, username/password, creds, nkey_seed")
	}
	if (c.Username == "") != (c.Password == "") && c.Token == "" && c.Creds == "" && c.NKeySeed == "" {
		return nil, errors.New("settings: username and password must be set together")
	}
	if (c.CertPEM == "") != (c.KeyPEM == "") {
		return nil, errors.New("settings.tls: cert_pem and key_pem must be set together")
	}
	return c, nil
}

func (c *Config) tlsConfig() (*tls.Config, error) {
	if !c.TLSEnabled {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName, InsecureSkipVerify: c.InsecureSkipVerify} //nolint:gosec // explicit opt-in
	if c.CAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.CAPEM)) {
			return nil, errors.New("settings.tls.ca_pem: no valid certificate found")
		}
		cfg.RootCAs = pool
	}
	if c.CertPEM != "" {
		cert, err := tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM))
		if err != nil {
			return nil, errors.New("settings.tls: invalid client certificate or key")
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// options builds nats.go options. Errors never contain secret values.
func (c *Config) options() ([]nats.Option, error) {
	opts := []nats.Option{
		nats.Name(c.Name),
		nats.Timeout(c.ConnectTimeout),
		nats.NoReconnect(), // short-lived connection per call; the runtime owns retry policy
	}
	switch {
	case c.Token != "":
		opts = append(opts, nats.Token(c.Token))
	case c.Username != "":
		opts = append(opts, nats.UserInfo(c.Username, c.Password))
	case c.Creds != "":
		creds := c.Creds
		opts = append(opts, nats.UserJWTAndSeed(extractBlock(creds, "NATS USER JWT"), extractBlock(creds, "USER NKEY SEED")))
	case c.NKeySeed != "":
		kp, err := nkeys.FromSeed([]byte(c.NKeySeed))
		if err != nil {
			return nil, errors.New("settings.nkey_seed is not a valid NKey seed")
		}
		pub, err := kp.PublicKey()
		if err != nil {
			return nil, errors.New("settings.nkey_seed is not a valid NKey seed")
		}
		opts = append(opts, nats.Nkey(pub, kp.Sign))
	}
	tc, err := c.tlsConfig()
	if err != nil {
		return nil, err
	}
	if tc != nil {
		opts = append(opts, nats.Secure(tc))
	}
	return opts, nil
}

// extractBlock returns the body between "-----BEGIN <name>-----" and its END marker of a .creds file.
func extractBlock(creds, name string) string {
	begin := "-----BEGIN " + name + "-----"
	i := strings.Index(creds, begin)
	if i < 0 {
		return ""
	}
	rest := creds[i+len(begin):]
	j := strings.Index(rest, "------END")
	if j < 0 {
		j = strings.Index(rest, "-----END")
	}
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// connect opens a connection.
func (c *Config) connect() (*nats.Conn, error) {
	opts, err := c.options()
	if err != nil {
		return nil, err
	}
	nc, err := nats.Connect(c.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %s", redact(err.Error(), c))
	}
	return nc, nil
}

// connectCtx is connect that gives up when ctx is done. nats.Connect has no context support, so
// the dial runs in a goroutine; a connection that completes after cancellation is closed.
func (c *Config) connectCtx(ctx context.Context) (*nats.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		nc  *nats.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		nc, err := c.connect()
		ch <- result{nc, err}
	}()
	select {
	case r := <-ch:
		return r.nc, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.nc != nil {
				r.nc.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// redact removes secret values from s.
func redact(s string, c *Config) string {
	for _, sec := range []string{c.Token, c.Password, c.Creds, c.NKeySeed, c.KeyPEM} {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}
