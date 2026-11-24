// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Not wrapped by golang.org/x/sys/windows.
var (
	modAdvapi32                 = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW              = modAdvapi32.NewProc("LogonUserW")
	procImpersonateLoggedOnUser = modAdvapi32.NewProc("ImpersonateLoggedOnUser")
)

const (
	logon32LogonInteractive = 2
	logon32LogonNetwork     = 3
	logon32ProviderDefault  = 0
)

func logonUser(user, pass string) (windows.Token, error) {
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return 0, err
	}
	dom, _ := windows.UTF16PtrFromString(".")
	p, err := windows.UTF16PtrFromString(pass)
	if err != nil {
		return 0, err
	}
	var lastErr error
	for _, typ := range []uintptr{logon32LogonInteractive, logon32LogonNetwork} {
		var tok windows.Token
		r, _, e := procLogonUserW.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(dom)), uintptr(unsafe.Pointer(p)),
			typ, logon32ProviderDefault, uintptr(unsafe.Pointer(&tok)))
		if r != 0 {
			return tok, nil
		}
		lastErr = e
	}
	return 0, lastErr
}

// asUser runs fn on a locked OS thread that impersonates tok. If reverting fails, the thread
// stays locked and is discarded when the goroutine exits.
func asUser(t *testing.T, tok windows.Token, fn func()) {
	t.Helper()
	done := make(chan struct{})
	var reverted bool
	go func() {
		defer close(done)
		runtime.LockOSThread()
		if r, _, e := procImpersonateLoggedOnUser.Call(uintptr(tok)); r == 0 {
			t.Errorf("ImpersonateLoggedOnUser: %v", e)
			return
		}
		func() {
			defer func() { reverted = windows.RevertToSelf() == nil }()
			fn()
		}()
		if reverted {
			runtime.UnlockOSThread()
		}
	}()
	<-done
	if !reverted {
		t.Fatal("impersonation failed or could not be reverted")
	}
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// TestOtherLocalUserCannotReadSecrets proves cross-user denial with a real second, non-administrator
// local account. It creates a temporary user (deleted in Cleanup), so it runs only with
// NURTUO_TEST_WINDOWS_OTHER_USER=1 in an elevated process. With the variable set it never skips: a
// missing precondition fails the test, so the go-windows job cannot pass without running it.
// The random password is never logged.
func TestOtherLocalUserCannotReadSecrets(t *testing.T) {
	if os.Getenv("NURTUO_TEST_WINDOWS_OTHER_USER") != "1" {
		t.Skip("set NURTUO_TEST_WINDOWS_OTHER_USER=1 in an elevated shell to run (creates a temporary local user)")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("NURTUO_TEST_WINDOWS_OTHER_USER=1 needs an elevated (administrator) process")
	}
	name := "nurtuo" + randHex(t, 4)
	pass := "Nt1!" + randHex(t, 12)
	redact := func(b []byte) string { return strings.ReplaceAll(string(b), pass, "***") }
	if out, err := exec.Command("net", "user", name, pass, "/add").CombinedOutput(); err != nil {
		t.Fatalf("net user %s /add: %v: %s", name, err, redact(out))
	}
	t.Cleanup(func() {
		if out, err := exec.Command("net", "user", name, "/delete").CombinedOutput(); err != nil {
			t.Errorf("net user %s /delete: %v: %s", name, err, out)
		}
	})
	tok, err := logonUser(name, pass)
	if err != nil {
		t.Fatalf("LogonUser %s: %v", name, err)
	}
	defer func() { _ = tok.Close() }()
	tu, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	other := tu.User.Sid.String()
	t.Logf("temporary user %s (%s)", name, other)

	pd := os.Getenv("ProgramData")
	if pd == "" {
		t.Fatal("ProgramData is not set")
	}
	base := filepath.Join(pd, "nurtuo-test-"+randHex(t, 4))
	if err := os.Mkdir(base, 0o755); err != nil { // inherits the default ProgramData ACL
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if out, err := exec.Command("icacls", pd).CombinedOutput(); err == nil {
		t.Logf("icacls %s:\n%s", pd, out)
	}

	// Control: the other user can read an ordinary file here, so a denial below comes from the
	// private DACL and not from an untraversable parent.
	control := filepath.Join(base, "control.txt")
	if err := os.WriteFile(control, []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	asUser(t, tok, func() {
		if _, err := os.ReadFile(control); err != nil {
			t.Errorf("control: other user cannot read %s (%v); the test would prove nothing", control, err)
		}
	})
	if t.Failed() {
		t.FailNow()
	}

	// The layout nurtuo standalone creates (DataDirs.EnsureReport + Bootstrap + the encrypted store).
	sec := filepath.Join(base, "secrets")
	if _, err := EnsurePrivateDir(sec); err != nil {
		t.Fatal(err)
	}
	files := []string{}
	for _, n := range []string{"admin-token", "db-password", "server.key", "client.key"} {
		p := filepath.Join(sec, n)
		if err := os.WriteFile(p, []byte(randHex(t, 8)), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	storeDir := filepath.Join(sec, "store")
	if err := NewEncrypted(storeDir, "").Set(ctx, "x", New(randHex(t, 8))); err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join(storeDir, KeyFileName), filepath.Join(storeDir, EncryptedFileName))
	if out, err := exec.Command("icacls", sec, "/T").CombinedOutput(); err == nil {
		t.Logf("icacls %s /T:\n%s", sec, out)
	}

	asUser(t, tok, func() {
		for _, p := range files {
			f, err := os.Open(p)
			if err == nil {
				_ = f.Close()
				t.Errorf("other user opened %s", p)
			} else if !errors.Is(err, fs.ErrPermission) {
				t.Errorf("open %s as other user: %v, want access denied", p, err)
			}
		}
		for _, d := range []string{sec, storeDir} {
			if _, err := os.ReadDir(d); !errors.Is(err, fs.ErrPermission) {
				t.Errorf("list %s as other user: %v, want access denied", d, err)
			}
			if err := os.WriteFile(filepath.Join(d, "planted"), []byte("p"), 0o644); !errors.Is(err, fs.ErrPermission) {
				t.Errorf("create in %s as other user: %v, want access denied", d, err)
			}
		}
	})

	// Planting: before the first run, the other user pre-creates secrets\store\master.key in a fresh
	// data root (ProgramData lets Users create folders and files). Nurtuo must refuse, not use it.
	sec2 := filepath.Join(base, "planted", "secrets")
	store2 := filepath.Join(sec2, "store")
	key2 := filepath.Join(store2, KeyFileName)
	if err := os.MkdirAll(filepath.Dir(sec2), 0o755); err != nil {
		t.Fatal(err)
	}
	asUser(t, tok, func() {
		if err := os.MkdirAll(store2, 0o755); err != nil {
			t.Errorf("other user cannot create %s (%v): default ProgramData ACL differs from the spec's assumption", store2, err)
			return
		}
		if err := os.WriteFile(key2, make([]byte, 32), 0o644); err != nil {
			t.Errorf("other user cannot plant %s: %v", key2, err)
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	before := sddl(t, sec2)
	_, err = EnsurePrivateDir(sec2)
	wantProblem(t, err, problemForeignOwner)
	if !strings.Contains(err.Error(), other) {
		t.Fatalf("error does not name the foreign owner %s: %v", other, err)
	}
	if after := sddl(t, sec2); after != before {
		t.Fatal("EnsurePrivateDir changed a foreign-owned directory")
	}
	_, err = FileKeySource{Path: key2}.Key()
	wantProblem(t, err, problemForeignOwner)
	if _, err := Open(Config{Provider: ProviderEncryptedFile, Path: store2}); err == nil {
		t.Fatal("Open accepted a foreign-owned operator store")
	}
}
