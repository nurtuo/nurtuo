// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

var ctx = context.Background()

func TestSecretNeverFormats(t *testing.T) {
	s := New("hunter2-value")
	j, _ := json.Marshal(map[string]any{"s": s})
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "secret", s)
	for _, got := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q", s, s, s, s, s), string(j), buf.String(),
	} {
		if strings.Contains(got, "hunter2") {
			t.Fatalf("secret leaked: %s", got)
		}
	}
	if s.Reveal() != "hunter2-value" {
		t.Fatal("Reveal")
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"a", "erp-token", "A.b_c-1"} {
		if err := ValidName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", "../x", "a/b", `a\b`, ".hidden", "-x", "a b", strings.Repeat("a", 129), "a\x00b"} {
		if ValidName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestEnvProvider(t *testing.T) {
	env := map[string]string{"NURTUO_SECRET_ERP_TOKEN": "tok", "NURTUO_SECRET_EMPTY": "", "OTHER": "x"}
	e := &Env{
		Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Environ: func() []string {
			return []string{"NURTUO_SECRET_ERP_TOKEN=tok", "NURTUO_SECRET_EMPTY=", "OTHER=x", "NURTUO_SECRET_=z"}
		},
	}
	for _, n := range []string{"erp-token", "ERP_TOKEN", "erp.token"} {
		if v, err := e.Get(ctx, n); err != nil || v.Reveal() != "tok" {
			t.Errorf("Get(%q) = %v, %v", n, v, err)
		}
	}
	_, err := e.Get(ctx, "empty")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `"empty"`) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := e.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := e.Get(ctx, "../x"); err == nil {
		t.Fatal("bad name accepted")
	}
	if !errors.Is(e.Set(ctx, "a", New("b")), ErrReadOnly) || !errors.Is(e.Delete(ctx, "a"), ErrReadOnly) {
		t.Fatal("env must be read-only")
	}
	if l, _ := e.List(ctx); !slices.Equal(l, []string{"ERP_TOKEN"}) {
		t.Fatalf("list = %v", l)
	}
}

func TestDirProvider(t *testing.T) {
	root := filepath.Join(t.TempDir(), "s")
	d := NewDir(root)
	if _, err := d.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing dir should be not found: %v", err)
	}
	if err := d.Set(ctx, "api-key", New("v1")); err != nil {
		t.Fatal(err)
	}
	if v, err := d.Get(ctx, "api-key"); err != nil || v.Reveal() != "v1" {
		t.Fatalf("%v %v", v, err)
	}
	if err := d.Set(ctx, "api-key", New("v2")); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Get(ctx, "api-key"); v.Reveal() != "v2" {
		t.Fatal("overwrite")
	}
	// Kubernetes/Docker style: trailing newline is trimmed.
	if err := os.WriteFile(filepath.Join(root, "nl"), []byte("val\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Get(ctx, "nl"); v.Reveal() != "val" {
		t.Fatalf("trim: %q", v.Reveal())
	}
	if l, _ := d.List(ctx); !slices.Equal(l, []string{"api-key", "nl"}) {
		t.Fatalf("list %v", l)
	}
	if err := d.Delete(ctx, "nl"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "nl"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	if err := d.Set(ctx, "../escape", New("x")); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := d.Get(ctx, "a/b"); err == nil {
		t.Fatal("path traversal accepted on Get")
	}
	if err := d.Set(ctx, "empty", New("")); err == nil {
		t.Fatal("empty value accepted")
	}
}

func TestDirPermissionsAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not checked on Windows")
	}
	root := t.TempDir()
	d := NewDir(root)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "k")
	_ = os.WriteFile(p, []byte("v"), 0o644)
	_ = os.Chmod(p, 0o644)
	_, err := d.Get(ctx, "k")
	if err == nil || !strings.Contains(err.Error(), "permissions") || strings.Contains(err.Error(), `"v"`) {
		t.Fatalf("group-readable file accepted: %v", err)
	}
	_ = os.Chmod(p, 0o600)
	if _, err := d.Get(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)
	if _, err := d.Get(ctx, "k"); err == nil {
		t.Fatal("world-readable directory accepted")
	}
	_ = os.Chmod(root, 0o700)
	target := filepath.Join(t.TempDir(), "t")
	_ = os.WriteFile(target, []byte("v"), 0o600)
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := d.Get(ctx, "link"); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestEncryptedStore(t *testing.T) {
	dir := t.TempDir()
	e := NewEncrypted(dir, "")
	if _, err := e.Get(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh store: %v", err)
	}
	if err := e.Set(ctx, "db-pass", New("s3cr3t-value")); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "tok", New("tok-value")); err != nil {
		t.Fatal(err)
	}
	// A new instance (restart) reads the same data.
	e2 := NewEncrypted(dir, "")
	if v, err := e2.Get(ctx, "db-pass"); err != nil || v.Reveal() != "s3cr3t-value" {
		t.Fatalf("%v %v", v, err)
	}
	if l, _ := e2.List(ctx); !slices.Equal(l, []string{"db-pass", "tok"}) {
		t.Fatalf("list %v", l)
	}
	raw, err := os.ReadFile(filepath.Join(dir, EncryptedFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("s3cr3t")) || bytes.Contains(raw, []byte("db-pass")) {
		t.Fatal("plaintext in the encrypted file")
	}
	// Fresh nonce per write: re-setting the same value changes the ciphertext.
	_ = e2.Set(ctx, "tok", New("tok-value"))
	raw2, _ := os.ReadFile(filepath.Join(dir, EncryptedFileName))
	if bytes.Equal(raw, raw2) {
		t.Fatal("ciphertext unchanged across writes (nonce reuse?)")
	}
	if err := e2.Delete(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Get(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatal("not deleted")
	}
	if err := e2.Delete(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
}

func TestEncryptedTamperAndWrongKey(t *testing.T) {
	dir := t.TempDir()
	e := NewEncrypted(dir, "")
	_ = e.Set(ctx, "a", New("value-a"))
	p := filepath.Join(dir, EncryptedFileName)

	raw, _ := os.ReadFile(p)
	bad := bytes.Clone(raw)
	bad[len(bad)-3] ^= 1
	_ = os.WriteFile(p, bad, 0o600)
	if _, err := e.Get(ctx, "a"); err == nil || strings.Contains(err.Error(), "value-a") {
		t.Fatalf("tampered store accepted / leaked: %v", err)
	}
	_ = os.WriteFile(p, raw, 0o600)

	// A different master key cannot decrypt.
	other := &Encrypted{Path: p, Keys: FileKeySource{Path: filepath.Join(t.TempDir(), "k2")}}
	if _, err := other.Get(ctx, "a"); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("wrong key: %v", err)
	}
	_ = os.WriteFile(p, []byte("garbage"), 0o600)
	if _, err := e.Get(ctx, "a"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestKeySources(t *testing.T) {
	if _, err := (KeychainKeySource{}).Key(); !errors.Is(err, ErrKeychainUnavailable) {
		t.Fatal("stub must report unavailable")
	}
	kf := filepath.Join(t.TempDir(), "sub", "master.key")
	k1, err := FileKeySource{Path: kf}.Key()
	if err != nil || len(k1) != 32 {
		t.Fatalf("%d %v", len(k1), err)
	}
	k2, _ := FileKeySource{Path: kf}.Key()
	if !bytes.Equal(k1, k2) {
		t.Fatal("key not stable")
	}
	if _, err := (FileKeySource{Path: filepath.Join(t.TempDir(), "none"), ReadOnly: true}).Key(); err == nil {
		t.Fatal("read-only source must not create a key")
	}
	_ = os.WriteFile(kf, []byte("short"), 0o600)
	if _, err := (FileKeySource{Path: kf}).Key(); err == nil {
		t.Fatal("short key accepted")
	}
	// A real failure of an earlier source is not masked by a later one.
	_, err = ChainKeySource{FileKeySource{Path: kf}, FileKeySource{Path: filepath.Join(t.TempDir(), "ok")}}.Key()
	if err == nil {
		t.Fatal("chain masked a bad key")
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor()
	r.Add("abcdef", "abc", "", "ab")
	got := r.Redact("x abcdef y abc z ab")
	if got != "x *** y *** z ab" {
		t.Fatalf("got %q", got)
	}
	if r.RedactError(nil) != nil {
		t.Fatal("nil")
	}
	base := errors.New("boom")
	if r.RedactError(base) != base {
		t.Fatal("clean error must pass through")
	}
	e := r.RedactError(fmt.Errorf("dial abcdef: %w", base))
	if strings.Contains(e.Error(), "abcdef") || errors.Unwrap(e) != nil {
		t.Fatalf("not redacted: %v", e)
	}

	var buf bytes.Buffer
	log := slog.New(r.Handler(slog.NewTextHandler(&buf, nil))).With("pre", "has abcdef")
	log.Info("msg abcdef", "a", "abcdef", "err", errors.New("e abcdef"), slog.Group("g", "k", "abcdef"), "n", 5)
	if strings.Contains(buf.String(), "abcdef") {
		t.Fatalf("leak in log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "n=5") {
		t.Fatalf("non-secret attrs lost: %s", buf.String())
	}
	var nilR *Redactor
	if nilR.Redact("abcdef") != "abcdef" {
		t.Fatal("nil redactor")
	}
}

func TestOpenConfig(t *testing.T) {
	if _, err := Open(Config{Provider: "vault"}); err == nil {
		t.Fatal("unknown provider")
	}
	if _, err := Open(Config{Provider: ProviderDir}); err == nil {
		t.Fatal("dir needs path")
	}
	for _, c := range []Config{{}, {Provider: ProviderEnv}, {Provider: ProviderDir, Path: t.TempDir()}, {Provider: ProviderEncryptedFile, Path: t.TempDir()}} {
		if _, err := Open(c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestEnvVarFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/envvar.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture []struct {
		Name string `json:"name"`
		Env  string `json:"env"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture) < 4 {
		t.Fatalf("expected at least four fixture entries, got %d", len(fixture))
	}
	for _, entry := range fixture {
		if got := EnvVar(entry.Name); got != entry.Env {
			t.Errorf("EnvVar(%q) = %q, want %q", entry.Name, got, entry.Env)
		}
	}
}
