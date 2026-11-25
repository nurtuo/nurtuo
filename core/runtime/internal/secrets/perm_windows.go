// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows private ACLs (docs/specs/implemented/2026-11-24-windows-secrets-dacl.md). POSIX mode bits mean nothing
// on Windows, and the standalone data root %ProgramData%\Nurtuo inherits read and create access for
// the local Users group. Private directories and files therefore get a protected (non-inherited) DACL
// that allows only the process token user, SYSTEM and BUILTIN\Administrators (D4), and every read
// path verifies it.
//
// Accepted assumption (verify-then-open): checkPerm inspects a path by name and the caller opens it
// afterwards. Swapping the object in between needs write access to its parent directory, which the
// private DACL of that directory denies to every principal outside privateSIDs().

// processUserSID is the SID of the process token user. It is the process token, not the thread
// token, so it does not change while a thread impersonates another user.
var processUserSID = sync.OnceValues(func() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read process token user: %w", err)
	}
	return tu.User.Sid.String(), nil
})

// privateSIDs is the set of principals used both to set and to verify private DACLs (D4).
func privateSIDs() (map[string]bool, error) {
	u, err := processUserSID()
	if err != nil {
		return nil, err
	}
	return map[string]bool{u: true, sidLocalSystem: true, sidAdministrators: true}, nil
}

// privateSD returns a self-relative security descriptor with the protected private DACL: one
// FILE_ALL_ACCESS allow ACE per privateSIDs() entry, inheritable to files and sub-directories when
// dir is set. Owner and group are left unset (the creator's defaults apply).
func privateSD(dir bool) (*windows.SECURITY_DESCRIPTOR, error) {
	u, err := processUserSID()
	if err != nil {
		return nil, err
	}
	inh := ""
	if dir {
		inh = "OICI"
	}
	sids := []string{u}
	for _, s := range []string{sidLocalSystem, sidAdministrators} {
		if s != u { // a service running as SYSTEM gets one ACE, not two
			sids = append(sids, s)
		}
	}
	sddl := "D:P" // P: protected, nothing inherits from the parent (ProgramData)
	for _, s := range sids {
		sddl += fmt.Sprintf("(A;%s;FA;;;%s)", inh, s)
	}
	return windows.SecurityDescriptorFromString(sddl)
}

func privateSA(dir bool) (*windows.SecurityAttributes, error) {
	sd, err := privateSD(dir)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

// setPrivateDACL replaces the DACL of path with the protected private DACL. The owner is never
// changed. On a directory, Windows also re-propagates the inheritable ACEs to children whose DACL
// is not protected.
func setPrivateDACL(path string, dir bool) error {
	sd, err := privateSD(dir)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	runtime.KeepAlive(sd)
	return err
}

// readDACL returns the owner SID and the DACL of path.
func readDACL(path string) (daclInfo, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return daclInfo{}, err
	}
	defer runtime.KeepAlive(sd)
	var d daclInfo
	owner, _, err := sd.Owner()
	if err != nil {
		return daclInfo{}, err
	}
	if owner != nil {
		d.Owner = owner.String()
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		d.Null = true
		return d, nil
	}
	if err != nil {
		return daclInfo{}, err
	}
	for i := 0; i < int(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return daclInfo{}, err
		}
		a := aceInfo{Type: ace.Header.AceType, Flags: ace.Header.AceFlags, Mask: uint32(ace.Mask)}
		// Only the plain allow / deny layouts start the SID at SidStart.
		if a.Type == aceTypeAccessAllowed || a.Type == aceTypeAccessDenied {
			a.SID = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		}
		d.ACEs = append(d.ACEs, a)
	}
	return d, nil
}

// isReparse reports symlinks, junctions and other reparse points.
func isReparse(fi fs.FileInfo) bool {
	if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return true
	}
	if a, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return a.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return false
}

// checkPerm refuses a path whose owner or DACL is not private (see judgeDACL), and reparse points.
func checkPerm(path string, fi fs.FileInfo) error {
	if isReparse(fi) {
		return &aclError{Path: path, Problem: problemReparsePoint}
	}
	allowed, err := privateSIDs()
	if err != nil {
		return err
	}
	d, err := readDACL(path)
	if err != nil {
		return fmt.Errorf("read ACL of %s: %w", path, err)
	}
	return judgeDACL(path, d, allowed)
}

// chmodPrivate sets an explicit protected private DACL (no inheritance flags) on a file.
func chmodPrivate(path string) error { return setPrivateDACL(path, false) }

// createPrivateExclusive creates a new file (fails if it exists) whose protected private DACL is
// part of the CreateFile call, so no other principal can open it in between.
func createPrivateExclusive(path string) (*os.File, error) {
	sa, err := privateSA(false)
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	runtime.KeepAlive(sa)
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// mkdirPrivate creates a missing leaf directory with the protected private DACL from its first
// instant (parents keep their normal ACL). An existing directory is left as it is.
func mkdirPrivate(path string) error {
	fi, err := os.Lstat(path)
	if err == nil {
		if isReparse(fi) {
			return &aclError{Path: path, Problem: problemReparsePoint}
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sa, err := privateSA(true)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(p, sa)
	runtime.KeepAlive(sa)
	if err != nil && !errors.Is(err, fs.ErrExist) { // ErrExist: created concurrently; callers verify it
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

// visitTree calls fn for root and everything below it, without following reparse points (they fail
// in checkPerm before the walk could descend into them).
func visitTree(root string, fn func(path string, fi fs.FileInfo) error) error {
	return filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		return fn(p, fi)
	})
}

// EnsurePrivateDir creates path (0700 on Unix, protected private DACL on Windows) and, on
// Windows, verifies and repairs the directory and everything below it. It returns the paths whose
// DACL it tightened. It fails closed if any object has a foreign owner or cannot be fixed.
// Only for Nurtuo-managed paths (standalone secrets\, #64 bundles\); never for operator-chosen paths (D9).
//
// Repair never changes an owner and never adds a principal outside privateSIDs(): it only replaces
// a too-open DACL with the private one, so it cannot weaken protection.
//
// TODO(#78): this helper lives in internal/secrets only for now (D1, interim). Target direction: a
// shared, dedicated private-path ACL layer that the D7 follow-up (data root, postgres\data,
// postgres\runtime, logs\; https://github.com/nurtuo/nurtuo/issues/78) can reuse, not a permanent
// home inside the secrets package.
func EnsurePrivateDir(path string) ([]string, error) {
	if err := mkdirPrivate(path); err != nil {
		return nil, standaloneHint(err)
	}
	// Pass 1: find what is too open; refuse before changing anything if an object is foreign-owned,
	// a reparse point or unreadable.
	type item struct {
		path string
		dir  bool
	}
	var todo []item
	err := visitTree(path, func(p string, fi fs.FileInfo) error {
		err := checkPerm(p, fi)
		if err == nil {
			return nil
		}
		var ae *aclError
		if errors.As(err, &ae) && ae.repairable() {
			todo = append(todo, item{p, fi.IsDir()})
			return nil
		}
		return err
	})
	if err != nil {
		return nil, standaloneHint(err)
	}
	// Pass 2: repair top-down. A parent's repair re-propagates inherited ACEs, so re-check first.
	var repaired []string
	for _, it := range todo {
		fi, err := os.Lstat(it.path)
		if err != nil {
			return repaired, err
		}
		if checkPerm(it.path, fi) == nil {
			continue
		}
		if err := setPrivateDACL(it.path, it.dir); err != nil {
			return repaired, fmt.Errorf("tighten ACL of %s: %w; run Nurtuo as the account that created it or as an administrator", it.path, err)
		}
		if fi, err = os.Lstat(it.path); err == nil {
			err = checkPerm(it.path, fi)
		}
		if err != nil {
			return repaired, fmt.Errorf("%s is still not private after repair: %w", it.path, err)
		}
		repaired = append(repaired, it.path)
	}
	return repaired, nil
}

func standaloneHint(err error) error {
	var ae *aclError
	if errors.As(err, &ae) && ae.Problem == problemForeignOwner {
		return fmt.Errorf("%w; remove it or take ownership after checking its contents", err)
	}
	return err
}

// ensurePrivateDirNoRepair is the check-only counterpart of EnsurePrivateDir for operator-chosen
// paths (D9): a missing leaf is created private; an existing tree is verified and a problem fails
// closed with fix guidance. It never changes the ACL of an existing object.
func ensurePrivateDirNoRepair(path string) error {
	if err := mkdirPrivate(path); err != nil {
		return operatorError(err, true)
	}
	return checkPrivateTree(path)
}

// checkPrivateTree verifies path and everything below it, without changing anything.
func checkPrivateTree(path string) error {
	return visitTree(path, func(p string, fi fs.FileInfo) error {
		return operatorError(checkPerm(p, fi), fi.IsDir())
	})
}

// operatorError adds the D9 fix guidance (principals, icacls / takeown) to an aclError.
func operatorError(err error, isDir bool) error {
	var ae *aclError
	if !errors.As(err, &ae) {
		return err
	}
	return fmt.Errorf("%w. %s", err, operatorFixHint(ae, isDir, selfAccount()))
}

// selfAccount is the running account in icacls form: DOMAIN\user, or *SID if it does not resolve.
func selfAccount() string {
	u, err := processUserSID()
	if err != nil {
		return "<this account>"
	}
	if sid, err := windows.StringToSid(u); err == nil {
		if acc, dom, _, err := sid.LookupAccount(""); err == nil && acc != "" {
			if dom != "" {
				return dom + `\` + acc
			}
			return acc
		}
	}
	return "*" + u
}

// checkOperatorPaths verifies an existing operator-chosen dir / encrypted-file store and key_file
// when the store is opened (D9). Nothing is created or changed; missing paths are fine (the first
// write creates them private).
func checkOperatorPaths(c Config) error {
	if c.Provider != ProviderDir && c.Provider != ProviderEncryptedFile {
		return nil
	}
	if _, err := os.Lstat(c.Path); err == nil {
		if err := checkPrivateTree(c.Path); err != nil {
			return fmt.Errorf("secrets: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("secrets: %w", err)
	}
	if c.Provider == ProviderEncryptedFile && c.KeyFile != "" {
		fi, err := os.Lstat(c.KeyFile)
		if err == nil {
			if err := operatorError(checkPerm(c.KeyFile, fi), false); err != nil {
				return fmt.Errorf("secrets: %w", err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("secrets: %w", err)
		}
	}
	return nil
}

// checkStoreDir verifies the encrypted store's directory on read (Windows only).
func checkStoreDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return checkPerm(dir, fi)
}
