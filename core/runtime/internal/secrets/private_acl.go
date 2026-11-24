// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"fmt"
	"strings"
)

// Windows private-ACL policy (docs/specs/implemented/2026-11-24-windows-secrets-dacl.md). The decision logic is
// here, without a build tag, so its table tests run on every OS; the Win32 extraction and the
// helpers that set ACLs are in perm_windows.go.

// Well-known SIDs of the allowed principals besides the process token user (D4).
const (
	sidLocalSystem    = "S-1-5-18"     // NT AUTHORITY\SYSTEM
	sidAdministrators = "S-1-5-32-544" // BUILTIN\Administrators
	sidCreatorOwner   = "S-1-3-0"      // CREATOR OWNER (inherit-only placeholder; tolerated)
	sidOwnerRights    = "S-1-3-4"      // OWNER RIGHTS (tolerated)
)

// ACE types (winnt.h ACE_HEADER.AceType).
const (
	aceTypeAccessAllowed         = 0x0
	aceTypeAccessDenied          = 0x1
	aceTypeAccessDeniedObject    = 0x6
	aceTypeAccessDeniedCallback  = 0xA
	aceTypeAccessDeniedCallbackO = 0xC
)

// aceInfo is one ACE of a DACL. SID is empty for ACE types whose layout is not decoded.
type aceInfo struct {
	Type  byte
	Flags byte
	Mask  uint32
	SID   string
}

// daclInfo is the owner and DACL of a file or directory. Null is a NULL DACL (or no DACL at all),
// which grants everyone full access.
type daclInfo struct {
	Owner string
	Null  bool
	ACEs  []aceInfo
}

type aclProblem int

const (
	problemForeignOwner aclProblem = iota + 1
	problemReparsePoint
	problemNullDACL
	problemOpenACE
	problemUnknownACE
)

// aclError reports why a path is not private. It names the path and a SID, never file content.
type aclError struct {
	Path    string
	SID     string // offending owner or ACE SID, when known
	AceType byte
	Problem aclProblem
}

const allowedPrincipals = `this account, SYSTEM and BUILTIN\Administrators`

func (e *aclError) Error() string {
	switch e.Problem {
	case problemForeignOwner:
		return fmt.Sprintf("%s is owned by %s, not by this account, SYSTEM or Administrators", e.Path, e.SID)
	case problemReparsePoint:
		return fmt.Sprintf("%s is a symlink, junction or other reparse point; refusing to use it", e.Path)
	case problemNullDACL:
		return fmt.Sprintf("%s has no DACL (everyone has full access); only %s may have access", e.Path, allowedPrincipals)
	case problemUnknownACE:
		return fmt.Sprintf("%s has an access-allowed ACE of unsupported type %#x; only plain allow ACEs for %s are accepted", e.Path, e.AceType, allowedPrincipals)
	default:
		return fmt.Sprintf("%s grants access to %s; only %s may have access", e.Path, e.SID, allowedPrincipals)
	}
}

// repairable reports whether replacing the DACL with the private one fixes the problem. A foreign
// owner (a planted object) and a reparse point are never repaired (D3).
func (e *aclError) repairable() bool {
	return e.Problem == problemNullDACL || e.Problem == problemOpenACE || e.Problem == problemUnknownACE
}

// judgeDACL fails when the owner is not allowed, the DACL is NULL, or any granting ACE (including
// inherit-only ones) names a SID outside allowed. Deny ACEs are ignored; unknown granting ACE types
// fail closed. Strictness matches the Unix check (any group/other bit fails): a non-allowed allow
// ACE fails whatever its mask.
func judgeDACL(path string, d daclInfo, allowed map[string]bool) error {
	if !allowed[d.Owner] {
		return &aclError{Path: path, SID: d.Owner, Problem: problemForeignOwner}
	}
	if d.Null {
		return &aclError{Path: path, Problem: problemNullDACL}
	}
	for _, a := range d.ACEs {
		switch a.Type {
		case aceTypeAccessDenied, aceTypeAccessDeniedObject, aceTypeAccessDeniedCallback, aceTypeAccessDeniedCallbackO:
			continue
		case aceTypeAccessAllowed:
			if allowed[a.SID] || a.SID == sidCreatorOwner || a.SID == sidOwnerRights {
				continue
			}
			return &aclError{Path: path, SID: a.SID, Problem: problemOpenACE}
		default:
			return &aclError{Path: path, AceType: a.Type, Problem: problemUnknownACE}
		}
	}
	return nil
}

// operatorFixHint is the actionable fix for an operator-chosen dir / encrypted-file path (D9):
// Nurtuo never changes that path, so the error tells the operator how to. self is the running
// account in icacls form (DOMAIN\user, or *SID when the name does not resolve).
func operatorFixHint(e *aclError, isDir bool, self string) string {
	var b strings.Builder
	b.WriteString("Nurtuo does not change ACLs on a configured secrets path; fix it and restart. Allowed: ")
	b.WriteString(allowedPrincipals)
	b.WriteString(", with inheritance disabled. ")
	switch e.Problem {
	case problemForeignOwner:
		fmt.Fprintf(&b, `Check the contents, then take ownership: takeown /F "%s"`, e.Path)
		if isDir {
			b.WriteString(" /R /D Y")
		}
		fmt.Fprintf(&b, `, or icacls "%s" /setowner "%s"`, e.Path, self)
		if isDir {
			b.WriteString(" /T")
		}
	case problemReparsePoint:
		fmt.Fprintf(&b, "Replace %s with a real directory or file", e.Path)
	default:
		inh := ""
		if isDir {
			inh = "(OI)(CI)"
		}
		fmt.Fprintf(&b, `Run: icacls "%s" /inheritance:r /grant:r "%s:%sF" "*%s:%sF" "*%s:%sF"`,
			e.Path, self, inh, sidLocalSystem, inh, sidAdministrators, inh)
		if e.SID != "" {
			fmt.Fprintf(&b, `, then: icacls "%s" /remove "*%s"`, e.Path, e.SID)
		}
	}
	return b.String()
}
