// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package secrets

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const sidUsers = "S-1-5-32-545" // BUILTIN\Users

// inspect returns the owner and DACL of path and whether the DACL is protected.
func inspect(t *testing.T, path string) (daclInfo, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	d, err := readDACL(path)
	if err != nil {
		t.Fatal(err)
	}
	return d, ctl&windows.SE_DACL_PROTECTED != 0
}

// sddl is the owner and DACL of path as SDDL, to compare before and after.
func sddl(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// allowSIDs is the set of SIDs with an allow ACE.
func allowSIDs(d daclInfo) map[string]bool {
	m := map[string]bool{}
	for _, a := range d.ACEs {
		if a.Type == aceTypeAccessAllowed {
			m[a.SID] = true
		}
	}
	return m
}

func mustPrivateSIDs(t *testing.T) map[string]bool {
	t.Helper()
	p, err := privateSIDs()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// wantPrivate asserts that the allow ACEs of path name exactly privateSIDs() and, if protected is
// set, that the DACL is protected.
func wantPrivate(t *testing.T, path string, protected bool) daclInfo {
	t.Helper()
	d, prot := inspect(t, path)
	if protected && !prot {
		t.Errorf("%s: DACL is not protected", path)
	}
	got, want := allowSIDs(d), mustPrivateSIDs(t)
	if len(got) != len(want) {
		t.Errorf("%s: allow SIDs %v, want %v", path, got, want)
	}
	for s := range got {
		if !want[s] {
			t.Errorf("%s: allow ACE for %s outside privateSIDs()", path, s)
		}
	}
	return d
}

// grantUsers adds an explicit BUILTIN\Users read ACE (inheritable on a directory) with icacls.
func grantUsers(t *testing.T, path string, dir bool) {
	t.Helper()
	perm := "*" + sidUsers + ":R"
	if dir {
		perm = "*" + sidUsers + ":(OI)(CI)R"
	}
	if out, err := exec.Command("icacls", path, "/grant", perm).CombinedOutput(); err != nil {
		t.Fatalf("icacls %s /grant %s: %v: %s", path, perm, err, out)
	}
}

func wantProblem(t *testing.T, err error, p aclProblem) {
	t.Helper()
	var ae *aclError
	if !errors.As(err, &ae) || ae.Problem != p {
		t.Fatalf("want ACL problem %d, got %v", p, err)
	}
}

func TestEnsurePrivateDirCreatesProtectedDACL(t *testing.T) {
	root := t.TempDir()
	grantUsers(t, root, true) // a parent like ProgramData: inheritable Users read
	dir := filepath.Join(root, "a", "secrets")
	repaired, err := EnsurePrivateDir(dir)
	if err != nil || len(repaired) != 0 {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	d := wantPrivate(t, dir, true)
	for _, a := range d.ACEs {
		if a.Flags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
			t.Errorf("ACE for %s is not inheritable (flags %#x)", a.SID, a.Flags)
		}
	}
	f := filepath.Join(dir, "admin-token")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantPrivate(t, f, false) // inherited from the private directory
	if repaired, err := EnsurePrivateDir(dir); err != nil || len(repaired) != 0 {
		t.Fatalf("second run: repaired=%v err=%v", repaired, err)
	}
}

func TestEnsurePrivateDirRepairsInheritedUsersACE(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "admin-token")
	if err := os.WriteFile(f, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	grantUsers(t, dir, true)
	grantUsers(t, f, false)
	if err := checkPerm(dir, lstat(t, dir)); err == nil || !strings.Contains(err.Error(), sidUsers) {
		t.Fatalf("precondition: %v", err)
	}
	repaired, err := EnsurePrivateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(repaired, dir) || !slices.Contains(repaired, f) {
		t.Fatalf("repaired %v, want %s and %s", repaired, dir, f)
	}
	for _, p := range []string{dir, f} {
		wantPrivate(t, p, true) // never adds a principal outside privateSIDs()
		if err := checkPerm(p, lstat(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(f); string(b) != "tok" {
		t.Fatal("content changed")
	}
}

func TestEnsurePrivateDirNeverAddsOrRemovesAllowed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Stricter than the private DACL (token user only): acceptable, so left exactly as is.
	u, _ := processUserSID()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before := sddl(t, dir)
	if repaired, err := EnsurePrivateDir(dir); err != nil || len(repaired) != 0 {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	if after := sddl(t, dir); after != before {
		t.Fatalf("acceptable DACL changed:\n%s\n%s", before, after)
	}
}

func TestCheckPermWindowsRejectsOpenDACL(t *testing.T) {
	const value = "v-secret-value-xyz"
	// dir provider
	dd := filepath.Join(t.TempDir(), "dir")
	d := NewDir(dd)
	if err := d.Set(ctx, "k", New(value)); err != nil {
		t.Fatal(err)
	}
	kp := filepath.Join(dd, "k")
	grantUsers(t, kp, false)
	_, err := d.Get(ctx, "k")
	wantOpenErr(t, err, kp, value)

	// FileKeySource
	kf := filepath.Join(t.TempDir(), "keys", "master.key")
	if _, err := (FileKeySource{Path: kf}).Key(); err != nil {
		t.Fatal(err)
	}
	grantUsers(t, kf, false)
	_, err = FileKeySource{Path: kf}.Key()
	wantOpenErr(t, err, kf, value)

	// Encrypted.load
	ed := filepath.Join(t.TempDir(), "store")
	e := NewEncrypted(ed, "")
	if err := e.Set(ctx, "k", New(value)); err != nil {
		t.Fatal(err)
	}
	enc := filepath.Join(ed, EncryptedFileName)
	grantUsers(t, enc, false)
	_, err = e.Get(ctx, "k")
	wantOpenErr(t, err, enc, value)
}

func wantOpenErr(t *testing.T, err error, path, value string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s with a Users ACE accepted", path)
	}
	msg := err.Error()
	if !strings.Contains(msg, path) || !strings.Contains(msg, sidUsers) || strings.Contains(msg, value) {
		t.Fatalf("error must name %s and %s and not the value: %q", path, sidUsers, msg)
	}
}

func TestCheckPermWindowsRejectsNullDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir")
	d := NewDir(dir)
	if err := d.Set(ctx, "k", New("v")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "k")
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, checkPerm(p, lstat(t, p)), problemNullDACL)
	if _, err := d.Get(ctx, "k"); err == nil {
		t.Fatal("NULL DACL accepted")
	}
}

func TestWriteFileAtomicPrivateFromCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if _, err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	var seen int
	afterPrivateCreate = func(tmp string) {
		seen++
		if fi := lstat(t, tmp); fi.Size() != 0 {
			t.Errorf("temp file has %d bytes before the check", fi.Size())
		}
		wantPrivate(t, tmp, true) // explicit protected DACL from CreateFile, before any write
	}
	t.Cleanup(func() { afterPrivateCreate = nil })
	enc := filepath.Join(dir, EncryptedFileName)
	for range 2 { // create, then replace
		if err := writeFileAtomic(enc, []byte("data")); err != nil {
			t.Fatal(err)
		}
		wantPrivate(t, enc, true)
	}
	if seen != 2 {
		t.Fatalf("hook ran %d times", seen)
	}
}

func TestFileKeySourcePrivateKeyFile(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	grantUsers(t, parent, true) // a non-private operator directory
	before := sddl(t, parent)
	kf := filepath.Join(parent, "master.key")
	if _, err := (FileKeySource{Path: kf}).Key(); err != nil {
		t.Fatal(err)
	}
	wantPrivate(t, kf, true)
	if after := sddl(t, parent); after != before {
		t.Fatalf("parent DACL changed:\n%s\n%s", before, after)
	}
	if _, err := (FileKeySource{Path: kf}).Key(); err != nil {
		t.Fatalf("re-read: %v", err)
	}
}

func TestEnsurePrivateDirRefusesReparsePoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if _, err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(dir, "j")
	if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); jerr != nil {
		if serr := os.Symlink(target, link); serr != nil {
			t.Skipf("neither a junction (%v: %s) nor a symlink (%v) can be created", jerr, out, serr)
		}
	}
	_, err := EnsurePrivateDir(dir)
	wantProblem(t, err, problemReparsePoint)
	wantProblem(t, ensurePrivateDirNoRepair(dir), problemReparsePoint)
	// The directory itself being a reparse point is refused too.
	_, err = EnsurePrivateDir(link)
	wantProblem(t, err, problemReparsePoint)
	wantProblem(t, checkPerm(link, lstat(t, link)), problemReparsePoint)
}

func TestOpenFailsClosedOnOpenOperatorStore(t *testing.T) {
	for _, prov := range []string{ProviderDir, ProviderEncryptedFile} {
		t.Run(prov, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "store")
			if err := os.Mkdir(store, 0o755); err != nil {
				t.Fatal(err)
			}
			grantUsers(t, store, true)
			before := sddl(t, store)
			_, err := Open(Config{Provider: prov, Path: store})
			wantOperatorErr(t, err, store)
			if after := sddl(t, store); after != before {
				t.Fatalf("store DACL changed (no repair allowed):\n%s\n%s", before, after)
			}
		})
	}
	t.Run("key_file", func(t *testing.T) {
		store := filepath.Join(t.TempDir(), "store")
		if _, err := EnsurePrivateDir(store); err != nil {
			t.Fatal(err)
		}
		kf := filepath.Join(t.TempDir(), "master.key")
		if err := os.WriteFile(kf, make([]byte, 32), 0o600); err != nil {
			t.Fatal(err)
		}
		grantUsers(t, kf, false)
		before := sddl(t, kf)
		_, err := Open(Config{Provider: ProviderEncryptedFile, Path: store, KeyFile: kf})
		wantOperatorErr(t, err, kf)
		if after := sddl(t, kf); after != before {
			t.Fatalf("key_file DACL changed (no repair allowed):\n%s\n%s", before, after)
		}
	})
	// A missing operator path is fine at Open; the first write creates it private.
	if _, err := Open(Config{Provider: ProviderDir, Path: filepath.Join(t.TempDir(), "none")}); err != nil {
		t.Fatal(err)
	}
}

func wantOperatorErr(t *testing.T, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Open accepted too-open %s", path)
	}
	msg := err.Error()
	for _, want := range []string{path, sidUsers, "SYSTEM", `BUILTIN\Administrators`, "icacls", "/inheritance:r"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error lacks %q: %s", want, msg)
		}
	}
}

func TestDirSetCreatesMissingOperatorDirPrivate(t *testing.T) {
	dp := filepath.Join(t.TempDir(), "new", "dir")
	if err := NewDir(dp).Set(ctx, "k", New("v")); err != nil {
		t.Fatal(err)
	}
	wantPrivate(t, dp, true)
	ep := filepath.Join(t.TempDir(), "new", "store")
	if err := NewEncrypted(ep, "").Set(ctx, "k", New("v")); err != nil {
		t.Fatal(err)
	}
	wantPrivate(t, ep, true)
	wantPrivate(t, filepath.Join(ep, KeyFileName), true)
	wantPrivate(t, filepath.Join(ep, EncryptedFileName), true)

	// Existing too-open paths: fail closed, unchanged.
	for _, mk := range []func(string) Store{
		func(p string) Store { return NewDir(p) },
		func(p string) Store { return NewEncrypted(p, "") },
	} {
		p := filepath.Join(t.TempDir(), "open")
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		grantUsers(t, p, true)
		before := sddl(t, p)
		if err := mk(p).Set(ctx, "k", New("v")); err == nil || !strings.Contains(err.Error(), sidUsers) {
			t.Fatalf("too-open %s accepted: %v", p, err)
		}
		if after := sddl(t, p); after != before {
			t.Fatalf("%s DACL changed:\n%s\n%s", p, before, after)
		}
	}
}

func lstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
