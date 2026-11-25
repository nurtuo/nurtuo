// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
)

// Config is the connection configuration. Password is injected in memory by the runtime from a
// secret reference; it is never logged or echoed in diagnostics.
type Config struct {
	Host            string
	Port            int
	Database        string
	User            string
	Password        string
	SSLMode         string // disable | require | verify-ca | verify-full
	SSLRootCert     string // path to CA bundle (verify-ca / verify-full)
	PoolMaxConns    int
	ConnectTimeoutS int
}

var validSSLModes = map[string]bool{"disable": true, "require": true, "verify-ca": true, "verify-full": true}

// ParseConfig reads the connection settings of a ConnectionConfig.
func ParseConfig(c *connectorv1.ConnectionConfig) (*Config, error) {
	s := c.GetSettings().AsMap()
	str := func(k string) string { v, _ := s[k].(string); return v }
	num := func(k string, def int) int {
		if v, ok := s[k].(float64); ok {
			return int(v)
		}
		return def
	}
	cfg := &Config{
		Host: str("host"), Port: num("port", 5432), Database: str("database"),
		User: str("user"), Password: str("password"),
		SSLMode: str("sslMode"), SSLRootCert: str("sslRootCert"),
		PoolMaxConns: num("poolMaxConns", 4), ConnectTimeoutS: num("connectTimeoutSeconds", 10),
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "require"
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("connection: host is required")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("connection: database is required")
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("connection: user is required")
	}
	if !validSSLModes[cfg.SSLMode] {
		return nil, fmt.Errorf("connection: sslMode %q is not one of disable, require, verify-ca, verify-full", cfg.SSLMode)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("connection: port %d out of range", cfg.Port)
	}
	if cfg.PoolMaxConns < 1 || cfg.PoolMaxConns > 100 {
		return nil, fmt.Errorf("connection: poolMaxConns must be between 1 and 100")
	}
	return cfg, nil
}

// URL builds a connection URL with all parts properly escaped. It contains the password: use
// only to configure the pool, never log it.
func (c *Config) URL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:   "/" + c.Database,
	}
	q := url.Values{}
	q.Set("sslmode", c.SSLMode)
	if c.SSLRootCert != "" {
		q.Set("sslrootcert", c.SSLRootCert)
	}
	q.Set("connect_timeout", strconv.Itoa(c.ConnectTimeoutS))
	q.Set("pool_max_conns", strconv.Itoa(c.PoolMaxConns))
	u.RawQuery = q.Encode()
	return u.String()
}

// key identifies a pool: a SHA-256 of the connection URL, so the plaintext password is never
// kept as a map key.
func (c *Config) key() string {
	sum := sha256.Sum256([]byte(c.URL()))
	return hex.EncodeToString(sum[:])
}

func (c *Config) poolConfig() (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(c.URL())
	if err != nil {
		return nil, fmt.Errorf("connection: %s", redact(err.Error(), c.Password))
	}
	return pc, nil
}

// redact removes the secret from a message.
func redact(msg string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
			msg = strings.ReplaceAll(msg, url.QueryEscape(s), "***")
			msg = strings.ReplaceAll(msg, url.PathEscape(s), "***")
		}
	}
	return msg
}
