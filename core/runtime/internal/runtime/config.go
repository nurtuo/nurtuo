// SPDX-License-Identifier: AGPL-3.0-only

// Package runtime wires the gateway client and the supervisor into one runnable unit.
package runtime

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"github.com/nurtuo/nurtuo/core/runtime/internal/connhost"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
)

// Config is the on-disk runtime configuration (YAML or JSON, chosen by file extension).
type Config struct {
	RuntimeID       string `yaml:"runtime_id" json:"runtime_id"`
	ControlPlaneURL string `yaml:"control_plane_url" json:"control_plane_url"` // gateway host:port
	CertFile        string `yaml:"cert_file" json:"cert_file"`
	KeyFile         string `yaml:"key_file" json:"key_file"`
	CAFile          string `yaml:"ca_file" json:"ca_file"`
	ServerName      string `yaml:"server_name,omitempty" json:"server_name,omitempty"` // TLS SNI override
	LogLevel        string `yaml:"log_level,omitempty" json:"log_level,omitempty"`
	BundleDir       string `yaml:"bundle_dir,omitempty" json:"bundle_dir,omitempty"` // <digest>.json bundles; empty = placeholder deployer
	// StateDir is the runtime's private state directory: embedded JetStream data in jetstream/ and
	// the applied revision in revision. Optional: when unset, queues are in-memory and the revision
	// is lost on restart (startup warning). Standalone and `service install` always set it.
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty"`
	// HTTPAddr is the listen address of HTTP-trigger listeners; default "127.0.0.1:0" (random port).
	HTTPAddr string `yaml:"http_addr,omitempty" json:"http_addr,omitempty"`
	// TrustedKeys are hex Ed25519 public keys. When set, bundle_dir holds signed <sha256>.bundle
	// archives, which are digest-checked and signature-verified before use; plain .json bundles
	// are then NOT accepted.
	TrustedKeys []string `yaml:"trusted_keys,omitempty" json:"trusted_keys,omitempty"`
	// Secrets selects where `secret://name` references in bundle connections are resolved:
	// {provider: env|dir|encrypted-file, path: ..., key_file: ...}. Default: env (NURTUO_SECRET_<NAME>).
	Secrets secrets.Config `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// ConnectionTest controls the Test() of every referenced connection at deploy time:
	// strict (default, a failing test fails the deployment), warn, or off.
	ConnectionTest string `yaml:"connection_test,omitempty" json:"connection_test,omitempty"`
}

// LoadConfig reads and validates a config file. Relative cert paths resolve against the file's directory.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		err = json.Unmarshal(b, &c)
	default:
		err = yaml.Unmarshal(b, &c)
	}
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for _, p := range []*string{&c.CertFile, &c.KeyFile, &c.CAFile, &c.StateDir, &c.Secrets.Path, &c.Secrets.KeyFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadSecretsConfig reads only the `secrets` section of a runtime config file (the CLI needs the
// secret store location, not the gateway credentials). Relative paths resolve against the file.
func LoadSecretsConfig(path string) (secrets.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return secrets.Config{}, fmt.Errorf("read config: %w", err)
	}
	var c struct {
		Secrets secrets.Config `yaml:"secrets" json:"secrets"`
	}
	if strings.ToLower(filepath.Ext(path)) == ".json" {
		err = json.Unmarshal(b, &c)
	} else {
		err = yaml.Unmarshal(b, &c)
	}
	if err != nil {
		return secrets.Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for _, p := range []*string{&c.Secrets.Path, &c.Secrets.KeyFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	return c.Secrets, c.Secrets.Validate()
}

// Validate reports missing required fields.
func (c *Config) Validate() error {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"runtime_id", c.RuntimeID}, {"control_plane_url", c.ControlPlaneURL},
		{"cert_file", c.CertFile}, {"key_file", c.KeyFile}, {"ca_file", c.CAFile},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return errors.New("config: missing required fields: " + strings.Join(missing, ", "))
	}
	if _, err := c.trustedKeys(); err != nil {
		return err
	}
	if err := c.Secrets.Validate(); err != nil {
		return err
	}
	switch c.ConnectionTest {
	case "", connhost.TestStrict, connhost.TestWarn, connhost.TestOff:
	default:
		return fmt.Errorf("config: connection_test %q must be strict, warn or off", c.ConnectionTest)
	}
	if len(c.TrustedKeys) > 0 && c.BundleDir == "" {
		return errors.New("config: trusted_keys requires bundle_dir")
	}
	return nil
}

func (c *Config) trustedKeys() ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for i, h := range c.TrustedKeys {
		k, err := bundle.DecodePublicKey(strings.TrimSpace(h))
		if err != nil {
			return nil, fmt.Errorf("config: trusted_keys[%d]: %w", i, err)
		}
		out = append(out, k)
	}
	return out, nil
}
