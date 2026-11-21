// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSecretsAndConnectionTest(t *testing.T) {
	dir := t.TempDir()
	write := func(extra string) string {
		p := filepath.Join(dir, "rt.yaml")
		base := "runtime_id: r\ncontrol_plane_url: h:1\ncert_file: c\nkey_file: k\nca_file: a\n"
		if err := os.WriteFile(p, []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := LoadConfig(write("secrets:\n  provider: encrypted-file\n  path: sec\n  key_file: sec.key\nconnection_test: warn\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Secrets.Provider != "encrypted-file" || c.Secrets.Path != filepath.Join(dir, "sec") || c.Secrets.KeyFile != filepath.Join(dir, "sec.key") || c.ConnectionTest != "warn" {
		t.Fatalf("%+v", c)
	}
	if c, err := LoadConfig(write("")); err != nil || c.Secrets.Provider != "" {
		t.Fatalf("default must be env (empty): %+v %v", c, err)
	}
	for extra, want := range map[string]string{
		"secrets:\n  provider: vault\n": "unknown provider",
		"secrets:\n  provider: dir\n":   "requires path",
		"connection_test: sometimes\n":  "strict, warn or off",
	} {
		if _, err := LoadConfig(write(extra)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", extra, err)
		}
	}
	sc, err := LoadSecretsConfig(write("secrets:\n  provider: dir\n  path: d\n"))
	if err != nil || sc.Path != filepath.Join(dir, "d") {
		t.Fatalf("%+v %v", sc, err)
	}
}

func TestParseBundleConnections(t *testing.T) {
	const f = `{"schema":"nurtuo.flow/v1","id":"a","name":"a","revision":1,"steps":[{"id":"s","kind":"start","config":{"type":"manual"}},{"id":"r","kind":"returnDocuments","config":{}}],"edges":{"s:default":"r"}}`
	c, err := ParseBundle([]byte(`{"flows":[` + f + `],"connections":[{"schema":"nurtuo.connection/v1","id":"erp","connector":"http","settings":{"auth":{"token":"secret://t"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Flows) != 1 || len(c.Connections) != 1 || c.Connections[0].ID != "erp" {
		t.Fatalf("%+v", c)
	}
	if _, err := ParseBundle([]byte(`{"flows":[` + f + `],"connections":[{"schema":"nurtuo.connection/v1","id":"E R P","connector":"http"}]}`)); err == nil || !strings.Contains(err.Error(), "invalid id") {
		t.Fatalf("%v", err)
	}
	// A bare flow document still parses (no connections).
	if c, err := ParseBundle([]byte(f)); err != nil || len(c.Flows) != 1 || len(c.Connections) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
}
