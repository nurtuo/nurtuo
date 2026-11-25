// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"errors"
	"strings"
	"testing"
)

const (
	tUser  = "S-1-5-21-1-2-3-1001"
	tOther = "S-1-5-21-1-2-3-1002"
	tUsers = "S-1-5-32-545"
	tWorld = "S-1-1-0"
	tAuth  = "S-1-5-11"
)

const (
	flagOI = 0x1
	flagCI = 0x2
	flagIO = 0x8 // INHERIT_ONLY_ACE
)

func allow(sid string, flags byte, mask uint32) aceInfo {
	return aceInfo{Type: aceTypeAccessAllowed, Flags: flags, Mask: mask, SID: sid}
}

func TestJudgeDACL(t *testing.T) {
	allowed := map[string]bool{tUser: true, sidLocalSystem: true, sidAdministrators: true}
	private := []aceInfo{
		allow(tUser, flagOI|flagCI, 0x1F01FF), allow(sidLocalSystem, flagOI|flagCI, 0x1F01FF), allow(sidAdministrators, flagOI|flagCI, 0x1F01FF),
	}
	with := func(extra ...aceInfo) []aceInfo { return append(append([]aceInfo{}, private...), extra...) }
	const path = `C:\ProgramData\Nurtuo\secrets\admin-token`
	cases := []struct {
		name    string
		d       daclInfo
		problem aclProblem // 0 = ok
		sid     string
	}{
		{"allowed only", daclInfo{Owner: tUser, ACEs: private}, 0, ""},
		{"owner Administrators (elevated creator)", daclInfo{Owner: sidAdministrators, ACEs: private}, 0, ""},
		{"owner SYSTEM", daclInfo{Owner: sidLocalSystem, ACEs: private}, 0, ""},
		{"empty DACL (nobody has access)", daclInfo{Owner: tUser, ACEs: []aceInfo{}}, 0, ""},
		{"creator owner and owner rights tolerated", daclInfo{Owner: tUser, ACEs: with(allow(sidCreatorOwner, flagOI|flagCI|flagIO, 0x1F01FF), allow(sidOwnerRights, 0, 0x1F01FF))}, 0, ""},
		{"deny ACEs ignored", daclInfo{Owner: tUser, ACEs: with(
			aceInfo{Type: aceTypeAccessDenied, Mask: 0x1F01FF, SID: tUsers},
			aceInfo{Type: aceTypeAccessDeniedObject}, aceInfo{Type: aceTypeAccessDeniedCallback}, aceInfo{Type: aceTypeAccessDeniedCallbackO},
		)}, 0, ""},
		{"Users read", daclInfo{Owner: tUser, ACEs: with(allow(tUsers, 0, 0x1200A9))}, problemOpenACE, tUsers},
		{"Users read attributes only", daclInfo{Owner: tUser, ACEs: with(allow(tUsers, 0, 0x80))}, problemOpenACE, tUsers},
		{"Users inherit-only create", daclInfo{Owner: tUser, ACEs: with(allow(tUsers, flagCI|flagIO, 0x6))}, problemOpenACE, tUsers},
		{"Everyone", daclInfo{Owner: tUser, ACEs: with(allow(tWorld, 0, 0x1F01FF))}, problemOpenACE, tWorld},
		{"Authenticated Users", daclInfo{Owner: tUser, ACEs: with(allow(tAuth, 0, 0x1200A9))}, problemOpenACE, tAuth},
		{"other user first", daclInfo{Owner: tUser, ACEs: append([]aceInfo{allow(tOther, 0, 0x1)}, private...)}, problemOpenACE, tOther},
		{"foreign owner", daclInfo{Owner: tOther, ACEs: private}, problemForeignOwner, tOther},
		{"foreign owner wins over open ACE", daclInfo{Owner: tOther, ACEs: with(allow(tUsers, 0, 0x1))}, problemForeignOwner, tOther},
		{"NULL DACL", daclInfo{Owner: tUser, Null: true}, problemNullDACL, ""},
		{"allowed-object ACE", daclInfo{Owner: tUser, ACEs: with(aceInfo{Type: 0x5, Mask: 0x1})}, problemUnknownACE, ""},
		{"allowed-callback ACE", daclInfo{Owner: tUser, ACEs: with(aceInfo{Type: 0x9, Mask: 0x1})}, problemUnknownACE, ""},
		{"compound ACE", daclInfo{Owner: tUser, ACEs: with(aceInfo{Type: 0x4})}, problemUnknownACE, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := judgeDACL(path, c.d, allowed)
			if c.problem == 0 {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			var ae *aclError
			if !errors.As(err, &ae) || ae.Problem != c.problem {
				t.Fatalf("want problem %d, got %v", c.problem, err)
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Fatalf("message lacks path: %q", msg)
			}
			if c.sid != "" && !strings.Contains(msg, c.sid) {
				t.Fatalf("message lacks SID %s: %q", c.sid, msg)
			}
			wantRepair := c.problem != problemForeignOwner
			if ae.repairable() != wantRepair {
				t.Fatalf("repairable=%v, want %v", ae.repairable(), wantRepair)
			}
		})
	}
	if (&aclError{Problem: problemReparsePoint}).repairable() {
		t.Fatal("reparse point must not be repairable")
	}
}

func TestOperatorFixHint(t *testing.T) {
	const self = `HOST\nurtuo`
	open := &aclError{Path: `D:\store`, SID: tUsers, Problem: problemOpenACE}
	h := operatorFixHint(open, true, self)
	for _, want := range []string{
		`icacls "D:\store" /inheritance:r /grant:r "HOST\nurtuo:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"`,
		`icacls "D:\store" /remove "*S-1-5-32-545"`,
		`SYSTEM`, `BUILTIN\Administrators`, "does not change",
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("dir hint lacks %q:\n%s", want, h)
		}
	}
	if h := operatorFixHint(&aclError{Path: `D:\k`, SID: tUsers, Problem: problemOpenACE}, false, self); strings.Contains(h, "(OI)") {
		t.Fatalf("file hint must not use inheritance flags: %s", h)
	}
	if h := operatorFixHint(&aclError{Path: `D:\k`, Problem: problemNullDACL}, false, self); strings.Contains(h, "/remove") {
		t.Fatalf("no /remove without an offending SID: %s", h)
	}
	h = operatorFixHint(&aclError{Path: `D:\store`, SID: tOther, Problem: problemForeignOwner}, true, self)
	if !strings.Contains(h, `takeown /F "D:\store" /R /D Y`) || !strings.Contains(h, `/setowner "HOST\nurtuo" /T`) {
		t.Fatalf("owner hint: %s", h)
	}
	if h := operatorFixHint(&aclError{Path: `D:\store`, Problem: problemReparsePoint}, true, self); !strings.Contains(h, "real directory") {
		t.Fatalf("reparse hint: %s", h)
	}
}
