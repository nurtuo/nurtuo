// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const sidUsers = "S-1-5-32-545" // BUILTIN\Users

// allowedSIDs is what the secrets package allows (D4): this process's user, SYSTEM, Administrators.
func allowedSIDs(t *testing.T) map[string]bool {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]bool{tu.User.Sid.String(): true, "S-1-5-18": true, "S-1-5-32-544": true}
}

// allowACESIDs returns the SIDs of the plain allow ACEs of path and fails on other granting types.
func allowACESIDs(t *testing.T, path string) []string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("%s: no DACL (%v)", path, err)
	}
	var out []string
	for i := 0; i < int(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatal(err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			out = append(out, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String())
		case windows.ACCESS_DENIED_ACE_TYPE:
		default:
			t.Fatalf("%s: unexpected ACE type %#x", path, ace.Header.AceType)
		}
	}
	return out
}

// wantSecretsPrivate asserts that every object under dir allows only allowedSIDs.
func wantSecretsPrivate(t *testing.T, dir string) int {
	t.Helper()
	allowed := allowedSIDs(t)
	n := 0
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n++
		for _, s := range allowACESIDs(t, p) {
			if !allowed[s] {
				t.Errorf("%s: allow ACE for %s", p, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// grantUsersInheritable gives BUILTIN\Users inheritable read on dir, like %ProgramData%.
func grantUsersInheritable(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("icacls", dir, "/grant", "*"+sidUsers+":(OI)(CI)R").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
}

func TestDataDirsEnsureSecretsPrivate(t *testing.T) {
	root := t.TempDir()
	grantUsersInheritable(t, root)
	d := NewDataDirs(root)
	var log bytes.Buffer
	if err := ensureDataDirs(d, &log); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("fresh install reported repairs: %s", log.String())
	}
	if _, err := NewLocalBootstrapper().Bootstrap(d); err != nil {
		t.Fatal(err)
	}
	if n := wantSecretsPrivate(t, d.Secrets); n != 9 { // dir + db-password, admin-token, 3 pem, 2 TLS keys, signing key
		t.Fatalf("only %d objects under %s", n, d.Secrets)
	}
	repaired, err := d.EnsureReport()
	if err != nil || len(repaired) != 0 {
		t.Fatalf("second run: repaired=%v err=%v", repaired, err)
	}
}

func TestDataDirsEnsureBundlesPrivate(t *testing.T) {
	root := t.TempDir()
	grantUsersInheritable(t, root)
	d := NewDataDirs(root)
	if err := os.Mkdir(d.Bundles, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(d.Bundles, "cached.bundle")
	if err := os.WriteFile(cache, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	repaired, err := d.EnsureReport()
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) == 0 {
		t.Fatal("expected a repair for the inherited Users ACE")
	}
	wantSecretsPrivate(t, d.Bundles)
}

func TestStandaloneRepairsLegacySecretsACL(t *testing.T) {
	root := t.TempDir()
	grantUsersInheritable(t, root)
	d := NewDataDirs(root)
	// A legacy install: secrets\ created with os.MkdirAll, inheriting the Users ACE.
	if err := os.MkdirAll(d.Secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	const tok = "legacy-admin-token-value"
	if err := os.WriteFile(filepath.Join(d.Secrets, "admin-token"), []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := ensureDataDirs(d, &log); err != nil {
		t.Fatal(err)
	}
	if want := "standalone: tightened permissions on " + d.Secrets; !strings.Contains(log.String(), want) {
		t.Fatalf("stderr %q lacks %q", log.String(), want)
	}
	if strings.Contains(log.String(), tok) {
		t.Fatal("token value in output")
	}
	c, err := NewLocalBootstrapper().Bootstrap(d)
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminToken != tok {
		t.Fatal("admin token changed by the repair")
	}
	wantSecretsPrivate(t, d.Secrets)
	if repaired, err := d.EnsureReport(); err != nil || len(repaired) != 0 {
		t.Fatalf("second run: repaired=%v err=%v", repaired, err)
	}
}
