// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretValue = "p4ss-w0rd-VALUE-xyz"

// runSecretCmd runs `nurtuo secret ...` with stdin as a pipe.
func runSecretCmd(t *testing.T, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	oldIn, oldTTY := secretStdin, stdinIsTTY
	secretStdin, stdinIsTTY = strings.NewReader(stdin), func() bool { return false }
	defer func() { secretStdin, stdinIsTTY = oldIn, oldTTY }()
	var o, e bytes.Buffer
	code = dispatch(append([]string{"secret"}, args...), &o, &e)
	return code, o.String(), e.String()
}

func TestSecretCLILifecycleDirProvider(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pf := []string{"--provider", "dir", "--path", dir}
	with := func(a ...string) []string { return append(append([]string{a[0]}, pf...), a[1:]...) }

	// stdin value (trailing newline trimmed).
	if code, out, errOut := runSecretCmd(t, secretValue+"\n", with("set", "erp-token")...); code != 0 {
		t.Fatalf("set: %d %s %s", code, out, errOut)
	} else if strings.Contains(out+errOut, secretValue) {
		t.Fatal("set echoed the value")
	}
	// get without --reveal never prints the value.
	code, out, _ := runSecretCmd(t, "", with("get", "erp-token")...)
	if code != 0 || strings.Contains(out, secretValue) || !strings.Contains(out, "set (") {
		t.Fatalf("get: %d %q", code, out)
	}
	code, out, _ = runSecretCmd(t, "", with("get", "--reveal", "erp-token")...)
	if code != 0 || strings.TrimSpace(out) != secretValue {
		t.Fatalf("get --reveal: %d %q", code, out)
	}
	code, out, _ = runSecretCmd(t, "", with("list")...)
	if code != 0 || strings.TrimSpace(out) != "erp-token" || strings.Contains(out, secretValue) {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, _, _ := runSecretCmd(t, "", with("delete", "erp-token")...); code != 0 {
		t.Fatal("delete")
	}
	if code, _, errOut := runSecretCmd(t, "", with("get", "erp-token")...); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("get after delete: %d %q", code, errOut)
	}
}

func TestSecretCLIRefusesArgvValues(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"set", "--provider", "dir", "--path", dir, "name", secretValue},
		{"set", "--provider", "dir", "--path", dir, "--value", secretValue, "name"},
		{"set", "--provider", "dir", "--path", dir, "--value=" + secretValue, "name"},
	} {
		code, out, errOut := runSecretCmd(t, secretValue, args...)
		if code != 2 {
			t.Fatalf("%v: exit %d, want 2 (usage error)", args, code)
		}
		if strings.Contains(out+errOut, secretValue) {
			t.Fatalf("%v: value echoed: %s%s", args, out, errOut)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("something was stored: %v", ents)
	}
}

func TestSecretCLIEnvProviderIsReadOnly(t *testing.T) {
	code, _, errOut := runSecretCmd(t, secretValue, "set", "--provider", "env", "erp-token")
	if code != 1 || !strings.Contains(errOut, "NURTUO_SECRET_ERP_TOKEN") || strings.Contains(errOut, secretValue) {
		t.Fatalf("%d %q", code, errOut)
	}
	if code, _, errOut := runSecretCmd(t, "", "delete", "--provider", "env", "x"); code != 1 || !strings.Contains(errOut, "read-only") {
		t.Fatalf("delete: %d %q", code, errOut)
	}
	t.Setenv("NURTUO_SECRET_FROM_ENV", secretValue)
	code, out, _ := runSecretCmd(t, "", "list", "--provider", "env")
	if code != 0 || !strings.Contains(out, "FROM_ENV") || strings.Contains(out, secretValue) {
		t.Fatalf("list: %d %q", code, out)
	}
}

func TestSecretCLIEncryptedFileViaConfig(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "runtime.yaml")
	if err := os.WriteFile(cfg, []byte("secrets:\n  provider: encrypted-file\n  path: store\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runSecretCmd(t, secretValue, "set", "--config", cfg, "db-pass"); code != 0 {
		t.Fatalf("set: %s", errOut)
	}
	raw, err := os.ReadFile(filepath.Join(root, "store", "secrets.enc"))
	if err != nil {
		t.Fatalf("relative path must resolve against the config file: %v", err)
	}
	if bytes.Contains(raw, []byte(secretValue)) {
		t.Fatal("plaintext in the encrypted store")
	}
	if code, out, _ := runSecretCmd(t, "", "get", "--reveal", "--config", cfg, "db-pass"); code != 0 || strings.TrimSpace(out) != secretValue {
		t.Fatalf("round trip: %d %q", code, out)
	}
}

func TestSecretCLIInputValidation(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := runSecretCmd(t, "", "set", "--provider", "dir", "--path", dir, "x"); code != 1 || !strings.Contains(errOut, "empty") {
		t.Fatalf("empty stdin: %d %q", code, errOut)
	}
	if code, _, _ := runSecretCmd(t, "v", "set", "--provider", "dir", "--path", dir, "../evil"); code != 1 {
		t.Fatalf("bad name accepted: %d", code)
	}
	if code, _, _ := runSecretCmd(t, "", "frobnicate"); code != 2 {
		t.Fatal("unknown action")
	}
	if code, _, _ := runSecretCmd(t, ""); code != 2 {
		t.Fatal("no action")
	}
	if code, _, errOut := runSecretCmd(t, "", "list", "--provider", "vault"); code != 1 || !strings.Contains(errOut, "unknown provider") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestSecretCLIPromptIsHidden(t *testing.T) {
	// t.TempDir is 0755 on Linux; the dir provider rejects group/other access.
	dir := filepath.Join(t.TempDir(), "s")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldTTY, oldRead := stdinIsTTY, readHiddenLine
	defer func() { stdinIsTTY, readHiddenLine = oldTTY, oldRead }()
	stdinIsTTY = func() bool { return true }
	answers := []string{secretValue, secretValue}
	readHiddenLine = func() (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	var o, e bytes.Buffer
	if code := dispatch([]string{"secret", "set", "--provider", "dir", "--path", dir, "k"}, &o, &e); code != 0 {
		t.Fatalf("%d %s", code, e.String())
	}
	if strings.Contains(o.String()+e.String(), secretValue) || !strings.Contains(e.String(), "hidden") {
		t.Fatalf("prompt output: %q %q", o.String(), e.String())
	}
	// Mismatching confirmation stores nothing.
	answers = []string{"one", "two"}
	o.Reset()
	e.Reset()
	if code := dispatch([]string{"secret", "set", "--provider", "dir", "--path", dir, "k2"}, &o, &e); code != 1 || !strings.Contains(e.String(), "do not match") {
		t.Fatalf("%d %s", code, e.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "k2")); err == nil {
		t.Fatal("mismatch stored a value")
	}
}
