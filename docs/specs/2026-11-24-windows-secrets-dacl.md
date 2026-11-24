<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Owner-only Windows DACL for standalone secrets and private files

Status: draft. Date: 2026-11-24.

Extends: [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md) (gap 1, critical), [runtime-cli-service-standalone](implemented/2026-11-21-runtime-cli-service-standalone.md) (standalone data directory). Related: [runtime bundle_dir](2026-11-23-runtime-bundle-dir.md) (phase 2, issue #64: `bundles/` `0700`, `secrets/bundle-signing.key` `0600`), [workspace isolation](2026-11-22-workspace-isolation.md) (issue #54, different layer, see "Overlaps / related work").

## TLDR

On Windows, `checkPerm` and `chmodPrivate` in `core/runtime/internal/secrets/perm_windows.go` are no-ops, and `nurtuo standalone` keeps its data root in `%ProgramData%\Nurtuo`, which inherits read access (and create-file access) for the local `Users` group. Any other local user can read `secrets\store\master.key` plus `secrets.enc` (every connection secret), `admin-token`, `db-password` and the TLS private keys. This spec replaces the Windows no-ops with real DACL helpers: private directories and files get an explicit, protected (non-inherited) DACL at creation, existing installs are repaired at startup, and every read path verifies the DACL and fails closed when it is too open or the owner is foreign. The helpers are generic so `<data-dir>/bundles` and `secrets/bundle-signing.key` from #64 reuse them. **Draft: four owner decisions are open (D4, D7, D9, D13).**

## Status and links

| | |
|---|---|
| Pull requests | this spec (docs only); implementation will use `Closes #57` on a later `feat/` or `fix/` PR after approval |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md) (lists "Windows ACL check" as future work) |
| Issues | https://github.com/nurtuo/nurtuo/issues/57 (from the gap list in https://github.com/nurtuo/nurtuo/pull/47); related #64 (bundle-dir phase 2), #54 (workspace isolation, parallel work, not touched here) |
| Code (existing) | `core/runtime/internal/secrets/{perm_unix.go,perm_windows.go,dir.go,encrypted.go,open.go}`, `core/runtime/cmd/nurtuo/{standalone_layout.go,standalone_bootstrap.go,standalone.go,standalone_register.go,secret.go}`, `core/runtime/internal/runtime/run.go` (`secrets.Open` at runtime start), `.github/workflows/ci.yml` (`go` job) |
| User docs | [security.md](../security.md), [operations.md](../operations.md) (standalone data directory) |

## Goal

Operators who run `nurtuo standalone` (and, by the shared helpers, the runtime with the `dir` or `encrypted-file` provider) on Windows need the secret material to be readable only by the account that runs Nurtuo, as `0700`/`0600` already guarantee on Linux and macOS. Today nothing restricts it on Windows. The proposal: real Windows implementations of the existing permission helpers, an exported `EnsurePrivateDir` that creates or repairs a private directory, fail-closed verification on every read, and a Windows CI job with a real DACL test.

## Open Questions

Blocking (owner, see Decisions):

- **Q1 (D4):** DACL shape: strictly owner-only (as the issue says), or owner + `SYSTEM` + `BUILTIN\Administrators` (recommended, the Windows equivalent of `0700` with root)?
- **Q2 (D7):** Scope: only `secrets\` (recommended for this spec), or also the data root, `postgres\data` and `logs\` (recommended as a separate follow-up)?
- **Q3 (D9):** Should the non-standalone runtime (`nurtuo run` / `nurtuo service` with `secrets.provider: dir | encrypted-file`) also repair and fail closed on Windows (recommended), or only standalone?
- **Q4 (D13):** Make the new Windows CI job a required check (recommended: not now)?

Non-blocking: all other rows in Decisions are implementer defaults.

## Scope

In scope:

- Windows implementations of `checkPerm` and `chmodPrivate` in `core/runtime/internal/secrets` (today no-ops).
- New exported helpers in the same package (D1): `EnsurePrivateDir` (create or repair a private directory tree) and an atomic private-file create used by `FileKeySource` and `writeFileAtomic`.
- Standalone wiring: `DataDirs.Ensure` and `localBootstrapper.Bootstrap` protect `<data-dir>\secrets` and everything below it (`store\master.key`, `store\secrets.enc`, `admin-token`, `db-password`, `server.key`, `client.key`, the certificates) before any secret is written or read.
- Startup repair of too-open DACLs on existing installs (D2), fail closed when repair is impossible (D3).
- The `secrets.Open` path used by the runtime and `nurtuo secret` for `dir` / `encrypted-file` (D9, pending owner).
- Windows-only tests and a non-required Windows CI job (D6).
- User docs: interim warning (D5) and the final description of the Windows protection.

Non-goals:

- Bundle delivery, `<data-dir>\bundles`, `bundle-signing.key` generation (issue #64; this spec only makes the helper reusable, see "Overlaps").
- Protecting the data root, `postgres\data`, `postgres\runtime` or `logs\` (D7; recorded as gap 3).
- DPAPI / OS keychain (`KeychainKeySource` stays a stub; connections-and-secrets gap 2).
- Secret, master key, admin token or DB password rotation tooling (connections-and-secrets gaps 3 and 5).
- Any Linux/macOS behaviour change (D8).
- Changing `DefaultDataRoot` or the documented data directory layout (D14).
- Control plane, workspace isolation, RBAC (#54).

## Design

### Current behavior (as the code works on `main`, `7953d4f`)

- `DefaultDataRoot` (`cmd/nurtuo/standalone_layout.go:24`): `NURTUO_DATA_DIR`, else on Windows `%ProgramData%\Nurtuo`, else `%LOCALAPPDATA%\Nurtuo`.
- `DataDirs.Ensure` (`standalone_layout.go:65`): `os.MkdirAll` `0755` for `postgres\data`, `postgres\runtime`, `postgres\cache`, `logs`; `os.MkdirAll(d.Secrets, 0o700)`. On Windows Go ignores the mode, so every directory inherits the parent ACL. `os.MkdirAll` also never changes an existing directory (on any OS).
- `localBootstrapper.Bootstrap` (`standalone_bootstrap.go:54`): `MkdirAll(secrets, 0o700)`, then `loadOrGenSecret` for `db-password` and `admin-token` (`os.ReadFile`, else `os.WriteFile(…, 0o600)`), then `writePEM` for `ca.pem`/`server.pem`/`client.pem` (`0644`) and `server.key`/`client.key` (`0600`). No permission check on read on any OS.
- `runStandalone` (`standalone.go`) calls `dirs.Ensure()` and `Bootstrap` once, then `Stack.Run` calls both again. `runRealRuntime` (`standalone_register.go:72`) configures `secrets.Config{Provider: encrypted-file, Path: <secrets>\store}`; `nurtuo secret` without flags uses the same store (`standaloneSecretStore`, `secret.go:125`).
- `internal/secrets`:
  - `checkPerm(path, fi)` is called by `Dir.checkDir` (`dir.go:30`), `Dir.Get` per file (`dir.go:61`), `FileKeySource.Key` on the key file (`encrypted.go:52`) and `Encrypted.load` on `secrets.enc` (`encrypted.go:147`). On Unix it rejects `mode & 0o077 != 0`; on Windows it returns nil.
  - `chmodPrivate` is called only on the temp file in `writeFileAtomic` (`dir.go:131`): Unix `chmod 0600`, Windows no-op.
  - Directory creation: `Dir.Set` (`dir.go:83`), `FileKeySource.Key` (`encrypted.go:68`), `Encrypted.save` (`encrypted.go:185`), all `os.MkdirAll(…, 0o700)`. The key file is created with `os.OpenFile(O_CREATE|O_EXCL, 0o600)` (`encrypted.go:72`); temp files with `os.CreateTemp` (`dir.go:125`).
  - `Encrypted` checks the key file and `secrets.enc` but not its directory (any OS).
- `secrets.Open` (`open.go`) is called by `internal/runtime/run.go:45` at runtime start and by `nurtuo secret`; it does not touch the disk.
- `nurtuo service` runs `nurtuo service run --config …` (the distributed runtime, not standalone) through `kardianos/service`; on Windows that is the Service Control Manager, by default as `LocalSystem`. There is no standalone service mode.
- Default ACL of `C:\ProgramData` (Windows default, to be confirmed on a real host in Phase 2): `SYSTEM` and `Administrators` full (inherited to children), `CREATOR OWNER` full (inherit-only), `Users` read & execute (inherited), and `Users` "create files / create folders" inherited to sub-folders. Consequences for `%ProgramData%\Nurtuo\secrets` today: (a) any local user can read every file; (b) any local user can **create** files and folders in `secrets\` and `secrets\store\` and becomes their owner, so a user who plants `secrets\store\master.key` before the first `nurtuo secret set` chooses the master key (`FileKeySource.Key` uses an existing file as-is).

### Prior-spec / docs drift (design against the code)

| Where | What it says | What the code does |
|---|---|---|
| `core/runtime/internal/secrets/perm_windows.go:9` (comment), [ADR 0010](../adr/0010-connections-and-secrets.md) `dir` row, [security.md](../security.md) "Secrets handling" | secret directories / the data dir inherit the owner's **profile** ACL | standalone's default root is `%ProgramData%\Nurtuo` (`standalone_layout.go:31`); the profile (`%LOCALAPPDATA%`) is only the fallback. The claim is stale; treated as unintentional (the issue confirms the ProgramData risk). The implementation PR fixes the comment and the user docs; ADR 0010 is not edited (its "future work" line is resolved by this spec). |
| [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md) gap 1 | "set an owner-only DACL on the **secrets directory**" | the same exposure also applies to the `dir` provider and an `encrypted-file` `key_file` at any operator path, and to the bootstrap files that are never checked on read. The design covers them through the shared helpers (D1, D9). |
| [security.md](../security.md) "Transport" bullet | standalone private keys `0600`, directory `0700` | true on POSIX only at creation; existing directories are never re-moded (`os.MkdirAll`), and on Windows modes are ignored. Unix part out of scope (D8, gap 6). |

### Proposed approach

All new code is in `core/runtime/internal/secrets` (AGPL-3.0-only, same module as all callers; `cmd/nurtuo` already imports it). No new package.

**1. Allowed principals (D4, pending owner).** One function `privateSIDs()` returns the set used for both setting and verifying:

- the process token user (`windows.GetCurrentProcessToken().GetTokenUser()`), not the file owner: an elevated administrator's new objects are owned by `BUILTIN\Administrators` by default, so "the owner" is not a reliable principal;
- `NT AUTHORITY\SYSTEM` (`windows.CreateWellKnownSid(windows.WinLocalSystemSid)`): backup, the Windows service account of `nurtuo service`, no lock-out when an install later runs as a service;
- `BUILTIN\Administrators` (`WinBuiltinAdministratorsSid`): an elevated operator can run `nurtuo secret set` against a store created by another account or by `LocalSystem`. Administrators can take ownership of any file anyway (`SeTakeOwnershipPrivilege`); on Unix `root` likewise bypasses `0700`.

The strict "owner-only" alternative (token user only) is recorded in D4 with its lock-out risks.

**2. DACL shape (maps `0700` / `0600`).**

| Unix | Windows (proposed) |
|---|---|
| directory `0700` | protected DACL (`PROTECTED_DACL_SECURITY_INFORMATION`, so nothing inherits from `ProgramData`), one `ACCESS_ALLOWED` ACE `FILE_ALL_ACCESS` per allowed SID with `OBJECT_INHERIT_ACE \| CONTAINER_INHERIT_ACE` (built with `windows.ACLFromEntries`, `SUB_CONTAINERS_AND_OBJECTS_INHERIT`) |
| file `0600` | normally inherited from the private directory (atomic at create time); `chmodPrivate` additionally sets an explicit protected DACL with the same SIDs and no inheritance flags, for files whose parent is not a private directory (`key_file` at a custom path) |

Applied with `windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, DACL_SECURITY_INFORMATION|PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)`; the owner is not changed (taking ownership needs privileges and would hide a planted object, see 4).

**3. Creation without a race.** A DACL set after `CreateFile` leaves a window in which another user can open a handle and keep it; Windows checks access only at open time. Therefore:

- `EnsurePrivateDir` creates a missing leaf directory with `windows.CreateDirectory(path, &windows.SecurityAttributes{SecurityDescriptor: sd})` (parents with `os.MkdirAll` and their normal ACL), so the DACL exists from the first instant.
- A new unexported `createPrivateExclusive(path) (*os.File, error)` creates files with `windows.CreateFile(…, CREATE_NEW, …)` and the same security attributes, wrapped by `os.NewFile`. `FileKeySource.Key` (replaces `os.OpenFile(O_EXCL, 0600)`) and `writeFileAtomic` (replaces `os.CreateTemp` + `chmodPrivate` with a random `.tmp-*` name and `CREATE_NEW`) use it. On Unix the same function is `os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, 0o600)`, which is today's behaviour.
- `os.Rename` (`MoveFileEx` with replace) keeps the temp file's security descriptor, so the replaced `secrets.enc` stays private.
- Bootstrap files (`os.WriteFile` in `loadOrGenSecret`, `writePEM`) are written only after `EnsurePrivateDir(secrets)` succeeded, so they inherit the private DACL atomically; no change to those functions is required. The same holds for `secrets\bundle-signing.key` in #64 (it uses `loadOrGenSecret`).

**4. Verification (the Windows `checkPerm`).** `checkPerm(path, fi)` on Windows reads `windows.GetNamedSecurityInfo(path, SE_FILE_OBJECT, OWNER_SECURITY_INFORMATION|DACL_SECURITY_INFORMATION)` and fails when any of these holds:

- `fi` is a symlink or other reparse point (already refused for `dir` files; extended to the directory itself);
- the owner SID is not in `privateSIDs()` (a planted object; not repairable, see D3);
- the DACL is NULL (grants everyone) or cannot be read;
- any `ACCESS_ALLOWED*` ACE (including inherit-only ACEs on a directory, which would propagate to new files) names a SID outside `privateSIDs()`. Tolerated extra SIDs: `CREATOR OWNER` (`S-1-3-0`, resolves to whoever may create files, i.e. an allowed SID) and `OWNER RIGHTS` (`S-1-3-4`). Unknown or callback/object ACE types that grant access fail closed. Deny ACEs are ignored. Strictness matches Unix `mode & 0o077 != 0` (any bit for group/other fails), so a `Users` ACE with only `READ_ATTRIBUTES` also fails.

The decision logic is a pure function, `judgeDACL(owner string, aces []aceInfo, allowed map[string]bool) error`, in a file without a build tag (`private_acl.go`), so its table tests run on Linux in the required `go` job; only the Win32 extraction (`windows.GetAce`, `ACL.AceCount`) is in `perm_windows.go`. Error text names the path, the offending SID in string form (for example `S-1-5-32-545`) and a fix hint (restart `nurtuo standalone` to repair, or `icacls` for a custom path); it never contains file content.

**5. Repair (`EnsurePrivateDir`, D2).** New exported API in `internal/secrets` (both OSes):

```go
// EnsurePrivateDir creates path (0700 on Unix, protected owner-only DACL on Windows) and, on
// Windows, verifies and repairs the directory and everything below it. It returns the paths whose
// DACL it tightened. It fails closed if any object has a foreign owner or cannot be fixed.
func EnsurePrivateDir(path string) (repaired []string, err error)
```

Windows algorithm: create missing leaf as in 3; walk the tree without following reparse points (refuse them); for each object: foreign owner → error, no change; DACL acceptable → nothing; DACL too open → `SetNamedSecurityInfo` with the private DACL (directories: inheritable; `SetNamedSecurityInfo` also propagates to children whose DACL is not protected), record the path, re-run `checkPerm`, error if still too open. The function never adds an ACE outside `privateSIDs()` and never removes allowed ones, so it cannot weaken protection.

Unix: `os.MkdirAll(path, 0o700)` and `nil` repaired list, exactly today's behaviour (D8).

**6. Call sites.**

| Call site | Change |
|---|---|
| `DataDirs.Ensure` (`standalone_layout.go:71`) | `secrets.EnsurePrivateDir(d.Secrets)` instead of `os.MkdirAll(d.Secrets, 0o700)`; repaired paths are printed once to stderr (`standalone: tightened permissions on <path>`) by `runStandalone`. Signature becomes `Ensure() ([]string, error)` or takes a writer (implementer choice; internal). |
| `localBootstrapper.Bootstrap` (`standalone_bootstrap.go:56`) | same helper instead of `MkdirAll`. All bootstrap files are written and read below a verified directory. |
| `Dir.Set` (`dir.go:83`), `Encrypted.save` (`encrypted.go:185`), `FileKeySource.Key` (`encrypted.go:68`) | `EnsurePrivateDir(dir)` instead of `MkdirAll(dir, 0o700)` (on Unix identical). For `FileKeySource` with a custom `key_file`, only the file is protected via `createPrivateExclusive`; its parent is created with `EnsurePrivateDir` only when missing (an existing operator directory is not rewritten). |
| `secrets.Open` (`open.go`) for `dir` / `encrypted-file` (D9, pending owner) | on Windows, when `Path` exists, run `EnsurePrivateDir(Path)` and protect an existing `KeyFile`; error → `Open` fails, so the runtime refuses to start (`run.go:45`) and `nurtuo secret` exits 1. Unix: unchanged. |
| read paths (`Dir.checkDir`, `Dir.Get`, `FileKeySource.Key`, `Encrypted.load`) | unchanged; they now call the real Windows `checkPerm`. Windows only: `Encrypted.load` also checks its directory. |

**7. Failure modes and what the operator sees.**

| Situation | Behaviour |
|---|---|
| Fresh install | directory created private; no message |
| Existing install, inherited `Users` read | DACL repaired at start; stderr lists repaired paths; start continues. Release note: the material was readable before; rotate on multi-user hosts (D12) |
| `secrets\` or a file inside owned by another (non-allowed) account | start refused: `standalone: <path> is owned by S-1-5-21-…-1002, not by this account, SYSTEM or Administrators; remove it or take ownership after checking its contents`. Exit 1. No content read. |
| Repair fails (access denied: install created by a different non-admin user) | start refused with path and Win32 error; fix: run as that user or as administrator |
| Read path finds a too-open DACL after start (someone changed it) | the read fails with the path and SID; deployments fail `Prepare` with that message (no secret value in it); next start repairs |
| Run as a different account later (for example a virtual service account `NT SERVICE\…`) | access denied, refused; documented (gap 4) |

**Rollback.** Reverting the code restores the no-ops. The tightened DACLs stay on disk and are harmless for the account that ran Nurtuo (it remains in the DACL); an operator who needs inheritance back can run `icacls <path> /reset /T`. No data migration.

**Alternatives considered.**

- One-off DACL only on `secrets\` in `cmd/nurtuo`: leaves the `dir` provider, `key_file` and #64 unprotected and duplicates code; rejected (D1).
- `SetNamedSecurityInfo` after `os.MkdirAll` / `os.OpenFile` only: simpler but has the open-handle race in 3; rejected for creation, used for repair.
- Warn-only on a too-open DACL: weakens protection; rejected (D3).
- Moving the default root to `%LOCALAPPDATA%`: changes a documented (stable) layout and breaks the service story; rejected (D14).
- DPAPI-wrapped master key: protects against copies of the data dir, but not the other bootstrap files; separate work (connections-and-secrets gap 2).

### Overlaps / related work

- **#64 / bundle-dir phase 2** (approved D3, not implemented): `secrets\bundle-signing.key` will be written by `loadOrGenSecret` below the verified `secrets\` and inherits the private DACL with no extra code. `<data-dir>\bundles` (`0700`) is outside `secrets\`; #64 should call `secrets.EnsurePrivateDir(<data-dir>\bundles)` (same function; `0700` on Unix, private DACL plus owner check on Windows, which also blocks a pre-planted `bundles\` folder; ProgramData lets any user create it before the first start). No change to D3 of that spec. This spec does not create `bundles\`.
- **#54 / workspace isolation** (parallel work): control plane (TypeScript) authorization for `POST /v1/workspaces`. No shared code, file or contract with this spec; nothing here depends on it or blocks it.
- **Postgres data / logs** (D7): same ProgramData inheritance, not covered here (gap 3).

### Dependency

`golang.org/x/sys` v0.50.0 is already in `core/runtime/go.mod` (`// indirect`) and in the Windows build of `cmd/nurtuo` through `github.com/kardianos/service` (`GOOS=windows go list -deps ./cmd/nurtuo` lists `golang.org/x/sys/windows`). Using it directly only drops the `// indirect` comment; no new module, license BSD-3-Clause (listed in `.github/licenses/agpl-allow.txt`, checked by the `licenses` job). Verified to exist in that version: `SetNamedSecurityInfo`, `GetNamedSecurityInfo`, `ACLFromEntries`, `GetAce`, `CreateWellKnownSid`, `Token.GetTokenUser`, `CreateDirectory`, `PROTECTED_DACL_SECURITY_INFORMATION`. Not wrapped: `LogonUserW`, `ImpersonateLoggedOnUser` (needed only by the gated two-user test; lazy `advapi32` procs in the test file).

## Contracts

| Contract | Change | Stability |
|---|---|---|
| Runtime config keys (`secrets.provider`, `secrets.path`, `secrets.key_file`) | none | stable |
| CLI flags (`nurtuo standalone`, `nurtuo secret`) | none | stable |
| Data directory layout (`postgres/`, `logs/`, `secrets/`, `secrets/store`) | none (paths unchanged); Windows ACL of `secrets\` becomes protected | stable layout; ACL is new behaviour |
| `internal/secrets` API (`EnsurePrivateDir`) | new exported function in an internal package | internal |
| Error messages | new texts | internal (not a contract) |

No proto, REST, flow or bundle format change.

## Compatibility

No public contract changes. Behaviour changes on Windows only:

- Existing standalone installs: DACLs of `secrets\` are tightened at the next start. Other local non-admin users lose read access (intended). Tools or scripts running as a different non-admin account that read `admin-token` stop working; documented in release notes.
- Existing installs where `secrets\` content was created by another non-admin account (planted or multi-user use): start is refused until the operator resolves it (fail closed).
- Distributed runtime on Windows with `dir` / `encrypted-file` (D9): a store directory an operator created under `ProgramData` is tightened at the next start, or start fails when it has a foreign owner. Operators who intentionally granted a group read access lose it (they can switch to `env`). Pending owner decision.
- Linux / macOS: no change (D8).

Changeset: none (`core/` is not a published npm package or Go module tag).

## Security

- Threat addressed: another interactive or remote-desktop user, or a low-privileged local process under another account, on the same Windows host reading or pre-planting standalone secret material.
- Not addressed: administrators and `SYSTEM` (by design, D4); malware running as the Nurtuo account; backups of the data dir (connections-and-secrets gap 2); data in `postgres\data` and `logs\` (D7).
- Fails closed: foreign owner, NULL DACL, unknown granting ACE, reparse points, repair failure.
- Never weakens: repair only removes non-allowed ACEs and inheritance; never adds a principal outside `privateSIDs()`.
- Secrets never appear in errors or logs; messages contain paths and SIDs only.
- Interim: until the implementation ships, "do not run `nurtuo standalone` on a multi-user Windows host" (D5).
- AGENTS.md: security work → spec approval before code; no check is disabled or skipped.

## Decisions

Recommendations are not decisions until Łukasz confirms the `needs-owner` rows. Implementer defaults are reversible and do not block approval.

| ID | Question | Options | Recommendation | Why | Class |
|---|---|---|---|---|---|
| D1 | Helper scope | (a) one-off DACL on `secrets\` in `cmd/nurtuo`; (b) real Windows `checkPerm`/`chmodPrivate` + exported `EnsurePrivateDir` in `internal/secrets` for every private path | **(b)** | the four existing `checkPerm` call sites, the `dir` provider, `key_file` and #64 (`bundles\`, `bundle-signing.key`) get the same protection; no new package; no bundle delivery work | implementer-default |
| D2 | Repair existing installs at startup? | (a) only at creation; (b) verify and repair at standalone start (and store write paths) | **(b)**, never adding principals, never changing the owner, foreign owner → refuse | every install before this change has an inherited `Users` ACE; creation-only would leave all of them open | implementer-default |
| D3 | Verify-on-read failure mode | fail closed / tighten and continue / warn only | **fail closed after an attempted repair** at start; read paths fail closed without repairing | matches Unix `checkPerm`; warn-only would weaken protection (that alternative would need the owner) | implementer-default |
| D4 | DACL shape | (a) token user only (issue wording); (b) token user + `SYSTEM` + `BUILTIN\Administrators`; protected (no inheritance from ProgramData), inheritable ACEs on directories, files inherit, explicit DACL on stand-alone files | **(b)** | equivalent of `0700`/`0600` plus root; (a) locks out an elevated operator (`nurtuo secret set`) and a later `LocalSystem` service and breaks backup; admins can take ownership anyway | **needs-owner** (security policy; deviates from "owner-only" in the issue) |
| D5 | Interim warning "do not run standalone on a multi-user Windows host" | in this spec PR / in the implementation PR / tiny docs PR now; `SECURITY.md` / `docs/security.md` / `docs/operations.md` | **tiny docs-only PR right after this spec PR merges** (no spec needed: docs-only), in `docs/security.md` ("Secrets handling") and `docs/operations.md` (standalone section); not `SECURITY.md` (vulnerability reporting policy). This PR stays spec-only (like #70); the implementation PR replaces the warning with the new behaviour | the warning ships even if implementation slips; the risk is already public in issue #57 and gap 1 | implementer-default |
| D6 | Windows CI | (a) `//go:build windows` tests only, run manually; (b) new non-required `go-windows` job on `windows-latest` | **(b)**: `go test ./internal/secrets/... ./cmd/nurtuo/...` (without `-race`, it needs cgo) in `core/runtime`, plus the gated two-user test with `NURTUO_TEST_WINDOWS_OTHER_USER=1` | repo is public, GitHub-hosted runners are free; no Windows runner exists today (all jobs `ubuntu-latest`; the `go` job only cross-compiles `cmd/nurtuo` for Windows and never compiles Windows test files); AGENTS.md: CI changes that only add jobs need no owner | implementer-default |
| D7 | Data root, `postgres\data`, `logs\` | (a) `secrets\` only; (b) also the data root and Postgres cluster | **(a) in this spec; (b) as a follow-up issue/spec** | the issue is about secrets; the root holds Postgres binaries (`postgres\runtime`, planting risk) and a database with users and audit, which needs its own analysis (embedded Postgres behaviour on Windows, operators reading logs) | **needs-owner** (scope) |
| D8 | Unix parity (bootstrap files unchecked; `MkdirAll` never re-modes) | change Linux/macOS too / Windows only | **Windows only**; Unix helpers keep today's semantics; Unix gap filed as follow-up | keeps the change Windows-scoped and reviewable | implementer-default |
| D9 | Non-standalone runtime on Windows (`nurtuo run` / `nurtuo service`, `secrets.provider: dir \| encrypted-file`) | (a) repair + fail closed in `secrets.Open`; (b) read paths fail closed, no repair; (c) standalone only, distributed stays a no-op | **(a)** | read checks are shared (`checkPerm`), so (c) would need a Windows-only bypass; (b) breaks existing service installs without a fix path; (a) changes ACLs on operator-chosen paths, so it is the owner's call | **needs-owner** (scope and behaviour change for existing installs) |
| D10 | `golang.org/x/sys/windows` | direct use / write own syscalls | **direct use** | already in `go.mod` (indirect) and in the Windows binary via `kardianos/service`; BSD-3-Clause; not a new production dependency | implementer-default |
| D11 | Overlap with #54 | — | **none**; keep independent | control plane authorization vs runtime file ACLs | implementer-default (informational) |
| D12 | Material already exposed on existing multi-user hosts | auto-rotate / document | **document** in release notes: rotate `admin-token`, `db-password`, connection secrets (master key rotation has no tool, gap 5 of connections-and-secrets) | no rotation tooling exists; out of scope | implementer-default |
| D13 | Make `go-windows` a required check | yes / not now | **not now**; revisit when stable | required checks are branch protection (repository settings) | **needs-owner** (non-blocking; repo settings) |
| D14 | Change `DefaultDataRoot` on Windows | keep / move to `%LOCALAPPDATA%` | **keep** | documented stable layout; profile paths do not fit a service | implementer-default |
| D15 | Split into more than one spec? | one / several | **one spec, three phases** | one capability (Windows private ACLs); data root split out by D7 | implementer-default |
| D16 | Interaction with #64 | — | #64 calls `EnsurePrivateDir(<data-dir>\bundles)`; `bundle-signing.key` inherits from `secrets\`; no change to bundle-dir D3 | reuse, no respecification | implementer-default (informational) |

## Step plan

### Phase 1: interim warning (docs only, D5)

1. Add "do not run `nurtuo standalone` on a multi-user Windows host; other local users can read `%ProgramData%\Nurtuo\secrets`" to `docs/security.md` ("Secrets handling") and `docs/operations.md` (standalone section), and correct the stale "profile ACL" sentence. Verified by `node scripts/check-docs-links.mjs`. Separate tiny PR, not this one.

### Phase 2: Windows private-ACL helpers and standalone wiring (Closes #57 together with Phase 3)

1. Characterization: add `TestDataDirsEnsureUnixModes` (Unix) asserting today's modes, and the pure `judgeDACL` table tests. Green on unchanged code (for `judgeDACL`, the function lands with its tests). Verified by `go test ./internal/secrets/... ./cmd/nurtuo/...` on Linux.
2. Implement `privateSIDs`, ACL build/extract, `judgeDACL`, real Windows `checkPerm` and `chmodPrivate`, `createPrivateExclusive`, `EnsurePrivateDir` (`perm_windows.go`, `private_acl.go`; Unix counterparts in `perm_unix.go`). Verified by Windows unit tests (Tests) and `GOOS=windows go vet ./internal/secrets/...` on Linux.
3. Use them in `Dir.Set`, `Encrypted.save`, `FileKeySource.Key`, `writeFileAtomic`. Verified by the existing `secrets_test.go` suite on Linux (unchanged) and Windows.
4. Wire `DataDirs.Ensure` / `localBootstrapper.Bootstrap` / `runStandalone` (repaired-path message). Verified by `standalone_windows_test.go`.
5. Add the `go-windows` CI job (D6). Verified by the job running green on the PR.

### Phase 3: verify everywhere and docs

1. `secrets.Open` repair for `dir` / `encrypted-file` on Windows (only if D9 = (a); with (b) skip it; with (c) the spec must first be amended with a standalone-only switch for the Windows `checkPerm`). Verified by `TestOpenRepairsWindowsStore`.
2. Gated two-user test (`NURTUO_TEST_WINDOWS_OTHER_USER=1`) in the `go-windows` job. Verified by the job log showing the test ran, not skipped.
3. Docs: replace the interim warning with the new behaviour (which principals, repair message, foreign-owner refusal, rotation advice) in `docs/security.md` and `docs/operations.md`; fix the `perm_windows.go` comment; update connections-and-secrets gap 1 status via this spec's changelog; move this spec to `implemented/`. Verified by `node scripts/check-docs-links.mjs`.

Phase 2 and Phase 3 step 1 must land in the same PR: the real Windows `checkPerm` also guards the distributed runtime's read paths, so shipping it without the D9 outcome would make an existing too-open Windows store fail with no repair path. Phase 3 steps 2 and 3 may follow separately. After Phase 2 (with Phase 3 step 1) fresh and existing standalone installs are private and repaired, and the system works.

### Deferred (not in this spec)

- Data root / `postgres\data` / `logs\` (D7 follow-up).
- `bundles\` creation (#64, uses `EnsurePrivateDir`).
- Unix bootstrap-file checks (D8 follow-up).

## Acceptance criteria

- WHEN `nurtuo standalone` has started once on Windows with the default data root THEN a second, non-administrator local user cannot open `%ProgramData%\Nurtuo\secrets`, `secrets\store\master.key`, `secrets\store\secrets.enc`, `secrets\admin-token`, `secrets\db-password`, `secrets\server.key` or `secrets\client.key` for reading (`ERROR_ACCESS_DENIED`), and cannot create a file in `secrets\` or `secrets\store\`.
- WHEN those objects are inspected THEN their DACL is protected and every allow ACE names only the token user, `SYSTEM` or `Administrators` (or the D4 outcome), and the owner is one of those.
- WHEN an existing install has `secrets\` with inherited `Users` read access THEN the next `nurtuo standalone` start tightens it, prints each repaired path to stderr and starts normally.
- WHEN `secrets\` or any object below it is owned by a SID outside the allowed set THEN `nurtuo standalone` exits 1 with an error naming the path and the SID, without reading or modifying the object.
- WHEN after start an allow ACE for `Users` is added to `secrets\store\secrets.enc` THEN `nurtuo secret get` and the runtime's secret resolution fail with an error naming the path (no secret value in the message), and the next standalone start repairs it.
- WHEN the DACL of a checked object is NULL or contains an unknown granting ACE type THEN verification fails closed.
- WHEN `writeFileAtomic` replaces `secrets.enc` THEN the new file has the private DACL from its first instant (created with security attributes, not tightened afterwards).
- WHEN the same code runs on Linux or macOS THEN all existing tests pass unchanged and file/directory modes and errors are as before.
- WHEN (D9 = a) the Windows runtime starts with `secrets.provider: encrypted-file` and an existing too-open `path` THEN it is repaired, or start fails on a foreign owner.
- WHEN #64 calls `EnsurePrivateDir(<data-dir>\bundles)` THEN it gets `0700` on Unix and the private DACL on Windows without new helper code.

## Tests

Tests ship in the same PR as the code. Windows files use `//go:build windows`; they run in the new `go-windows` job (`windows-latest`, `working-directory: core/runtime`, `go test -count=1 ./internal/secrets/... ./cmd/nurtuo/...`). Linux CI additionally compiles them with `GOOS=windows go vet ./internal/secrets/... ./cmd/nurtuo/...` (in the new job or as a step there, not in the required `go` job).

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestJudgeDACL` in `core/runtime/internal/secrets/private_acl_test.go` (no build tag) | table: allowed-only ACEs ok; `Users`/`Everyone`/`Authenticated Users` allow ACE (any mask, incl. inherit-only) rejected; foreign owner rejected; NULL DACL rejected; unknown ACE type rejected; deny ACEs ignored; `CREATOR OWNER`/`OWNER RIGHTS` tolerated; error contains path and SID, never content | `cd core/runtime && go test ./internal/secrets -run TestJudgeDACL` | runs in the required `go` job |
| `TestEnsurePrivateDirCreatesProtectedDACL` in `internal/secrets/perm_windows_test.go` | new dir: protected flag set, ACE set equals `privateSIDs()`, inheritable flags; file created inside inherits the same SIDs | `go-windows` job | `t.TempDir()` under `%TEMP%` |
| `TestEnsurePrivateDirRepairsInheritedUsersACE` | dir and a child file with an explicit `BUILTIN\Users` read ACE added via `SetNamedSecurityInfo` (unprotected) → repaired list contains both, ACE gone, re-check passes | same | |
| `TestCheckPermWindowsRejectsOpenDACL` | `Dir.Get`, `FileKeySource.Key`, `Encrypted.load` fail on a file with a `Users` ACE; message has path and `S-1-5-32-545`, not the value | same | Windows counterpart of `TestDirPermissionsAndSymlinks` |
| `TestCheckPermWindowsRejectsNullDACL` | `SetNamedSecurityInfo` with a NULL DACL → rejected | same | |
| `TestWriteFileAtomicPrivateFromCreation` | temp file created via `createPrivateExclusive` has the protected DACL before any byte is written (inspect handle path right after create via a test seam) and `secrets.enc` keeps it after rename | same | seam: unexported hook between create and write |
| `TestFileKeySourcePrivateKeyFile` | new `master.key` at a custom `key_file` in a non-private parent: file has explicit private DACL, parent unchanged | same | |
| `TestEnsurePrivateDirRefusesReparsePoint` | junction (`mklink /J` via `cmd /c`) or symlink inside the tree → error | same | skip if neither can be created |
| `TestDataDirsEnsureSecretsPrivate` in `core/runtime/cmd/nurtuo/standalone_windows_test.go` | `DataDirs.Ensure` + `localBootstrapper.Bootstrap` on a temp root: every file under `secrets\` passes `checkPerm`; second run reports no repairs | `go-windows` job | |
| `TestStandaloneRepairsLegacySecretsACL` | pre-create `secrets\` with a `Users` ACE and an `admin-token`; Ensure repairs, prints the path, token value unchanged | same | |
| `TestOpenRepairsWindowsStore` in `internal/secrets/perm_windows_test.go` | only if D9 = (a): `secrets.Open` on an existing too-open store dir repairs it | same | |
| `TestOtherLocalUserCannotReadSecrets` in `internal/secrets/perm_windows_otheruser_test.go` | creates a temporary local user (`net user <random> <random-password> /add`, deleted in `t.Cleanup`), `LogonUserW` + `ImpersonateLoggedOnUser` (lazy `advapi32` procs), then: open of each named file → `ERROR_ACCESS_DENIED`; create in `secrets\` → denied; with the other user pre-creating `secrets\store\` before the first run → `EnsurePrivateDir` / `FileKeySource` fail closed on foreign owner | `go-windows` job with `NURTUO_TEST_WINDOWS_OTHER_USER=1` (runner is admin) | skipped without the variable or without admin rights; the random password is never logged |
| `TestDataDirsEnsureUnixModes` in `cmd/nurtuo/standalone_test.go` (Unix) | characterization: `secrets` `0700`, others `0755`; an existing `0755` `secrets` is left unchanged (D8) | `cd core/runtime && go test ./cmd/nurtuo -run TestDataDirsEnsureUnixModes` | green on unchanged code |
| existing `secrets_test.go`, `secret_test.go`, `standalone_test.go`, `standalone_e2e_test.go` | regression | `cd core/runtime && go test -race ./internal/secrets/... ./cmd/nurtuo/...` | Linux, unchanged |

Manual check before closing #57 (the issue notes the risk was never verified on a real host): on a Windows 11 or Server VM with two local accounts, run `nurtuo standalone` as account A (non-elevated), then as account B run `type %ProgramData%\Nurtuo\secrets\admin-token` → "Access is denied"; record `icacls` output in the PR.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Until the implementation ships, user B on a shared Windows host reads `secrets\store\master.key` and `secrets.enc` and decrypts every connection secret, or reads `admin-token` and logs in as the bootstrap owner | critical | standalone | this spec; interim warning (Phase 1, D5) |
| 2 | Before the first start of a fresh install, user B pre-creates `%ProgramData%\Nurtuo\secrets\store\master.key` with a known key; `FileKeySource` would use it | high | secrets | owner check in `checkPerm` / `EnsurePrivateDir` refuses foreign-owned objects (fail closed) |
| 3 | User B reads `postgres\data` (users, password hashes, audit) or plants an executable in `postgres\runtime` that the next start runs as the Nurtuo account | high | standalone | out of scope (D7); follow-up issue for the data root |
| 4 | An install created by account A is later run by non-admin account C or a virtual service account; repair fails with access denied and standalone refuses to start | medium | standalone ops | clear error with path; documented: run as A, as administrator, or `icacls` grant; D4 (b) avoids it for admins and `SYSTEM` |
| 5 | Repair at `secrets.Open` removes a group ACE an operator added on purpose for a distributed runtime store; the runtime then cannot read it under another account | medium | runtime | D9 owner decision; release notes; operators can use `env` |
| 6 | Unix: an existing `secrets/` with mode `0755` or a `0644` `admin-token` is never detected (bootstrap files are not checked, `MkdirAll` never re-modes) | medium | standalone (Unix) | out of scope (D8); follow-up |
| 7 | ACL evaluation misses a granting ACE kind (object, callback, conditional) and accepts an open DACL | medium | secrets | unknown granting ACE types fail closed; `judgeDACL` table tests |
| 8 | The two-user test is flaky or unavailable on hosted runners (user creation policy), so the real cross-user check silently never runs | medium | CI | test logs "skipped" loudly; Phase 3 step 2 verification requires the job log; manual check before closing #57 |
| 9 | Antivirus or backup tools running as a non-SYSTEM account lose access to `secrets\` | low | ops | documented; `SYSTEM` stays in the DACL |
| 10 | Material read before upgrade stays compromised | high | ops | D12 release-note advice; rotation tooling is connections-and-secrets gaps 3/5 |

## Improvement opportunities

- DPAPI-protected `master.key` in `KeychainKeySource` (connections-and-secrets gap 2).
- A `nurtuo standalone --check` that prints the ACL state without starting.
- Apply `EnsurePrivateDir` to the whole data root once D7 is decided.

## References

- Issue #57; PR #47 (gap list); issue #64 and [runtime bundle_dir](2026-11-23-runtime-bundle-dir.md) D3; issue #54 and [workspace isolation](2026-11-22-workspace-isolation.md).
- [ADR 0010](../adr/0010-connections-and-secrets.md); [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md); [runtime-cli-service-standalone](implemented/2026-11-21-runtime-cli-service-standalone.md).
- Microsoft docs: `SetNamedSecurityInfoW`, `CreateDirectoryW` security attributes, "Default owner for objects created by members of the Administrators group" policy, ACE inheritance rules.
- Pre-implement report: [analysis/pre-implement-windows-secrets-dacl.md](analysis/pre-implement-windows-secrets-dacl.md).

## Changelog

- 2026-11-24: created as draft.
