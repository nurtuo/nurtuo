<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Private DACL for the Windows standalone data root, postgres and logs

Status: draft. Date: 2026-11-25.

## TLDR

`nurtuo standalone` on Windows keeps its data root in `%ProgramData%\Nurtuo`, which inherits read and create access for the local `Users` group. Only `secrets\` is protected today (issue #57). This spec extends the same protected DACL (running account + `SYSTEM` + `BUILTIN\Administrators`) to the whole data root, including `postgres\data`, `postgres\runtime`, `postgres\cache` and `logs\`, and moves the ACL helpers out of `internal/secrets`. It needs owner approval of D1-D5 before any code.

## Status and links

| | |
|---|---|
| Pull requests | [PR #112](https://github.com/nurtuo/nurtuo/pull/112) (spec only) |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md) (secrets); no new ADR proposed |
| Issues | https://github.com/nurtuo/nurtuo/issues/78 (follow-up of https://github.com/nurtuo/nurtuo/issues/57) |
| Code | `core/runtime/internal/secrets/perm_windows.go`, `core/runtime/internal/secrets/private_acl.go`, `core/runtime/cmd/nurtuo/standalone_layout.go` (`DataDirs.EnsureReport`) |
| Parent spec | [Windows secrets DACL](implemented/2026-11-24-windows-secrets-dacl.md) (D1, D4, D7, D9, D13) |

## Problem and goals

Another local user can (a) read `postgres\data` (users, password hashes, audit records), (b) plant a binary in `postgres\runtime` that the next start runs as the Nurtuo account, (c) do the same through `postgres\cache`, if the cache is extracted to `runtime`, and (d) read `logs\`.

- Goals: after `standalone` starts on Windows, the data root and everything below it is owned by an allowed principal and has a private DACL under D2, protected on managed roots; a too-open tree that Nurtuo manages (see Repair semantics) is repaired at startup, and an operator-owned directory is only checked, with an `icacls` hint; a foreign owner or a reparse point fails closed; embedded Postgres still starts; operators have a documented way to read logs.
- Non-goals: `secrets\` semantics (unchanged, #57); Unix behaviour (unchanged); operator-chosen secrets paths (D9 of the parent spec still applies); encrypting data at rest; Windows service ACLs.

## Decisions (owner approval needed)

Recommendations are in bold; none is approved yet.

- **D1 Shared ACL layer.** **Move the private-path helpers (`EnsurePrivateDir`, `checkPerm`, `judgeDACL`, `mkdirPrivate`, `setPrivateDACL`, `visitTree`, `aclError`) into a new package `core/runtime/internal/privacl`, and let `internal/secrets` call it.** Pure move, no behaviour change, resolves the `TODO(#78)`. Alternative: keep them in `internal/secrets` and export `EnsurePrivateDir` only (smaller diff, but the data root keeps depending on the secrets package).
- **D2 Principal set.** **Same as D4 of the parent spec: the running account, `SYSTEM`, `BUILTIN\Administrators`.** No extra "operators" group.
- **D3 Scope of the walk.** **Walk the entire existing root, including `secrets\`, `postgres\data`, `postgres\runtime`, `postgres\cache` and `logs\`, on every start.** Repair only in managed mode. Measure the startup cost of walking large databases before reconsidering the scope.
- **D4 Operator-chosen data root.** **Check an existing `NURTUO_DATA_DIR` without repairing it unless the root and its marker meet the trust checks below.** If the root is absent, create it with the private DACL at creation and exclusively create `.nurtuo-managed` as a regular file with its own protected private DACL. If a root appears before exclusive creation succeeds, validate it as an existing root. Leave the existing parent untouched. The check-only policy follows D9 of the parent spec.
- **D5 Rejected roots.** **Startup fails with an error, before any check or change, if the data root is a drive root (`C:\`, `\\?\C:\`, a UNC share root) or is `%ProgramData%` itself.** Also rejected: a root that is a parent of `%ProgramData%`, `%SystemRoot%` or a user profile root. The message names the offending path and asks for a dedicated subdirectory.

## Repair semantics

- **Managed root** (repair allowed) means the root selected by the Windows default when `NURTUO_DATA_DIR` is unset, a root Nurtuo exclusively created this run, or an existing configured root with a trusted `.nurtuo-managed` marker. To trust the marker, require both the root and the marker to already pass the owner, private DACL and effective-access checks below and to have protected DACLs. Inspect the marker as a regular file without following reparse points. An absent or untrusted marker grants no repair authority. Check the root as operator-chosen, including checking the marker itself if present.
- **Operator root** (check only) means any other existing configured root. Nurtuo changes no existing ACL in the tree. A rejection names the offending path and reason and gives per-path `icacls` guidance for restoring the D2 grants and removing offending allow ACEs.

Both modes apply these standalone checks. The existing secrets APIs retain the #57 policy.

- **Owner**: must be the running account, `SYSTEM` or `BUILTIN\Administrators`. Any other owner fails closed. Repair never changes an owner (the take-ownership hint is printed).
- **Allow ACEs**: only the D2 principals; an allow ACE for any other SID, or an unknown ACE type, is too open. A NULL DACL is too open.
- **Effective access**: use the actual running token to verify rights needed on each object, such as directory traversal, enumeration and required child creation, and file read, write or execute access as applicable. An empty DACL, an inherit-only grant or a SYSTEM-only grant to a token lacking that SID is insufficient. Check-only roots fail on insufficient access. Managed paths can be repaired if safely inspectable, and must pass the effective-access check afterwards.
- **Deny ACEs**: any deny ACE (explicit or inherited) fails the check, for any SID. A deny on a D2 principal can lock Nurtuo out, and a deny on another SID is an operator rule we must not rewrite. Repair never removes or reorders deny ACEs; it refuses with the path and the ACE, in both modes.
- **Inheritance**: on a managed root, the root's DACL must be protected (inheritance from `%ProgramData%` disabled); Nurtuo sets this only on a root it manages (at creation, or when repairing a managed root). A child may inherit or hold explicit ACEs; explicit ACEs are judged by the same allow rules. A child of a managed root with a repairable DACL is repaired by replacing its DACL with the protected private one, never by editing single ACEs. Inheritance is never disabled on a directory Nurtuo did not create or does not manage, and never on the parent of the root. In an operator root a non-protected DACL passes only if it satisfies the allow-ACE and effective-access rules, including allow ACEs that propagate to children.
- **Reparse points**: refused in both modes; never followed.

## Design

1. On Windows, `DataDirs.EnsureReport` normalizes the path, applies D5, then inspects the existing path components (including ancestors) for reparse points without following them. Classify the root and walk any existing tree before writing anything. Managed preflight records repairable DACLs and refuses foreign owners, reparse points, deny ACEs and unreadable objects. Operator preflight requires every object to pass. Rejection leaves the existing ACLs and filesystem entries unchanged.
2. After preflight, create a missing root with its private DACL at creation and add the marker for a newly created configured root. Repair an existing managed tree top-down and re-check it before use. Repairs keep the owner and use only D2 principals. Then create missing layout directories with private DACLs in either mode. The walk covers `secrets\` and its report entries. Unix behavior stays unchanged.
3. Embedded Postgres: it runs as the Nurtuo account. The DACL grants that account `FILE_ALL_ACCESS` with `OI|CI`, so `initdb` and the server create files that inherit it. Windows Postgres starts with a restricted token that still contains the user SID, so access by the user ACE should work. This is a hypothesis; it must be confirmed on a real host (see Testing and gap 1).
4. Document log access through the running account, `SYSTEM` or Administrators, including elevation when using the Administrators grant. Apply D2 to logs on every start.
5. Startup output lists every tightened path (`standalone: tightened permissions on ...`), as for `secrets\`.
6. After ACL validation and any repair, verify Postgres artifacts before any executable is invoked. Verify cached archives against an expected digest for the pinned version and platform obtained from a trusted source outside the data root. Reuse a runtime tree only if its full contents and paths match the verified distribution. If a cached archive or runtime tree cannot be verified, obtain a verified archive and extract into a fresh private directory. Start Postgres from the verified tree exclusively. Fail startup if a verified distribution cannot be prepared. Apply the gate to existing installs, including operator roots that pass preflight, and on retries. Keep `postgres\data` in place.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| ACL of `%ProgramData%\Nurtuo` and below | now protected, three principals | stable once shipped |
| `NURTUO_DATA_DIR` handling | existing dir: check + hint unless marker and root are trusted under D4; D5 rejects unsafe roots | stable |
| Log access | `logs\` uses the D2 principal set | stable |
| Go package `internal/privacl` | new (D1) | internal |
| Unix behaviour, CLI flags, config keys, proto | none | unchanged |

## Compatibility

This applies to Windows standalone installs, including upgrades. Behaviour change: other local users lose read access to the data root and to logs; log readers must use a D2 principal. No breaking change to an API, proto or flag, so no breaking-change gate. Unix unchanged.

## Security

Private DACLs restrict future access under D2. The Postgres integrity gate covers runtime files and cached archives that predate hardening. Earlier disclosure or tampering with database contents requires separate operator assessment. Preflight fails closed on unsafe paths or ACLs that the selected mode cannot accept or repair. As in the parent spec, path-based use after verification requires protection against replacement through the parent. If an untrusted principal can replace a validated object, hold it and the relevant path components stable with Windows handles during validation and use, or fail closed. Repair stays inside the managed root.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| existing `judgeDACL` table tests, moved with D1 | decision logic, all OS | `cd core/runtime && go test ./internal/privacl/...` | |
| new Windows tests for `EnsureReport` | root, `postgres\data`, `runtime`, `cache`, `logs` private after create; open tree repaired; foreign owner and junction refused | `GOOS=windows go vet ./...`, run on the Windows CI job | `//go:build windows`, non-required job |
| Windows e2e: embedded Postgres starts under the protected DACL | gap 1 | Windows CI job | env-gated if the runner cannot download Postgres |
| Windows marker trust | trusted marker in a private protected configured root permits child repair; a foreign-owned marker or a marker in an open existing root grants no repair authority; compare existing ACLs byte for byte | Windows CI job | include a missing layout child and verify it remains absent on rejection |
| Windows read-only preflight | drive/share root, `%ProgramData%`, ancestor junction, descendant reparse point, deny ACE, foreign owner or failing operator ACL rejects startup before creating any layout directory or marker or changing any ACL | Windows CI job | verify a passing operator root can create missing layout directories privately |
| Windows effective access | empty, inherit-only and SYSTEM-only DACLs fail in check-only mode when the running token lacks the required rights; a usable private DACL passes | Windows CI job | run the SYSTEM-only case as a non-SYSTEM account |
| Windows Postgres upgrade | invalid runtime files and cached archives are never used after ACL repair or on retry; verified replacement launches; unavailability of verified artifacts fails startup; database files stay in place | Windows CI job | cover managed and checked operator roots |
| manual two-account check | second local user cannot read `logs\` or write `postgres\runtime` | real Windows host | hard exit criterion, as for #57 |
| Unix regression | `EnsureReport` creates the same layout | `go test ./cmd/nurtuo/...` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Embedded Postgres fails to start on Windows under the protected DACL (restricted token) | high | runtime | verify on a Windows host before merge; if it fails, add the needed principal only after owner approval |
| 2 | Full walk of a large `postgres\data` slows every start | medium | runtime | measure before proposing any change to D3 |
| 3 | Data root previously used by another account is foreign-owned and start fails | medium | ops | existing take-ownership hint; document in `docs/operations.md` |
| 4 | Antivirus or backup agents run as another account and lose access | low | ops | document access under an existing D2 principal |

## Improvement opportunities

A `nurtuo standalone doctor` command that prints the data root ACL status without starting anything.

## References

- [Windows secrets DACL](implemented/2026-11-24-windows-secrets-dacl.md); issue #78.

## Changelog

- 2026-11-25: created (draft).
- 2026-11-25: resolved D4/D9 conflict (operator roots are check-only), added D5 (rejected roots) and the Repair semantics section (owner, deny ACEs, inheritance, managed vs operator root) after review of #112.
- 2026-11-25: specified marker trust, read-only preflight, D2 log access, Postgres artifact verification and effective-access checks after re-review of #112.
