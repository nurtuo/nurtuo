<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Private DACL for the Windows standalone data root, postgres and logs

Status: draft. Date: 2026-11-25.

## TLDR

`nurtuo standalone` on Windows keeps its data root in `%ProgramData%\Nurtuo`, which inherits read and create access for the local `Users` group. Only `secrets\` is protected today (issue #57). This spec extends the same protected DACL (running account + `SYSTEM` + `BUILTIN\Administrators`) to the whole data root, including `postgres\data`, `postgres\runtime`, `postgres\cache` and `logs\`, and moves the ACL helpers out of `internal/secrets`. It needs owner approval of D1-D4 before any code.

## Status and links

| | |
|---|---|
| Pull requests | none yet (spec only) |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md) (secrets); no new ADR proposed |
| Issues | https://github.com/nurtuo/nurtuo/issues/78 (follow-up of https://github.com/nurtuo/nurtuo/issues/57) |
| Code | `core/runtime/internal/secrets/perm_windows.go`, `core/runtime/internal/secrets/private_acl.go`, `core/runtime/cmd/nurtuo/standalone_layout.go` (`DataDirs.EnsureReport`) |
| Parent spec | [Windows secrets DACL](implemented/2026-11-24-windows-secrets-dacl.md) (D1, D4, D7, D9, D13) |

## Problem and goals

Another local user can (a) read `postgres\data` (users, password hashes, audit records), (b) plant a binary in `postgres\runtime` that the next start runs as the Nurtuo account, (c) do the same through `postgres\cache`, if the cache is extracted to `runtime`, and (d) read `logs\`.

- Goals: after `standalone` starts on Windows, the data root and everything below it is owned by an allowed principal and has a protected DACL that grants only the D4 principals; a too-open existing tree is repaired at startup; a foreign owner or a reparse point fails closed; embedded Postgres still starts; operators have a documented way to read logs.
- Non-goals: `secrets\` semantics (unchanged, #57); Unix behaviour (unchanged); operator-chosen secrets paths (D9 of the parent spec still applies); encrypting data at rest; Windows service ACLs.

## Decisions (owner approval needed)

Recommendations are in bold; none is approved yet.

- **D1 Shared ACL layer.** **Move the private-path helpers (`EnsurePrivateDir`, `checkPerm`, `judgeDACL`, `mkdirPrivate`, `setPrivateDACL`, `visitTree`, `aclError`) into a new package `core/runtime/internal/privacl`, and let `internal/secrets` call it.** Pure move, no behaviour change, resolves the `TODO(#78)`. Alternative: keep them in `internal/secrets` and export `EnsurePrivateDir` only (smaller diff, but the data root keeps depending on the secrets package).
- **D2 Principal set.** **Same as D4 of the parent spec: the running account, `SYSTEM`, `BUILTIN\Administrators`.** No extra "operators" group.
- **D3 Scope of the walk.** **Call `EnsurePrivateDir(Root)` once; it covers `postgres\data`, `runtime`, `cache` and `logs\` by inheritance and repairs any child that is too open.** Cost: a full walk of `postgres\data` on every start. Alternative: walk everything except `postgres\data`, and for `postgres\data` check the top directory and repair once (marker file). Recommendation is the full walk until a measured start-up cost says otherwise.
- **D4 Operator-chosen data root.** **`NURTUO_DATA_DIR` is treated like the default root: Nurtuo repairs it, but never touches its parent.** Alternative: apply D9 (check only, print an `icacls` fix) when `NURTUO_DATA_DIR` is set. The repair is riskier there because the root may be a shared folder.

## Design

1. `DataDirs.EnsureReport` on Windows creates `Root` with the private DACL (`mkdirPrivate`, set at create time, not after), then the other directories, then runs `EnsurePrivateDir(Root)`. `Secrets` remains covered, so its separate call can go; its report entries stay. On Unix nothing changes (no tightening of existing directories).
2. Two-pass repair as today: pass 1 finds too-open objects and refuses, before any change, on a foreign owner, a reparse point or an unreadable object; pass 2 repairs top-down and re-checks. A repair never changes an owner and never adds a principal.
3. Embedded Postgres: it runs as the Nurtuo account. The DACL grants that account `FILE_ALL_ACCESS` with `OI|CI`, so `initdb` and the server create files that inherit it. Windows Postgres starts with a restricted token that still contains the user SID, so access by the user ACE should work. This is a hypothesis; it must be confirmed on a real host (see Testing and gap 1).
4. Logs: `logs\` is readable by the running account, `SYSTEM` and Administrators. A non-admin operator gets access by an explicit `icacls "<root>\logs" /grant "DOMAIN\user:(OI)(CI)R"`, documented in `docs/operations.md`. Nurtuo never adds that ACE (D2). Files created later inherit it.
5. Startup output lists every tightened path (`standalone: tightened permissions on ...`), as for `secrets\`.
6. First start after upgrade on a host that already has a populated, open data root: repaired in place. If a previous run used another account (for example a service as `SYSTEM`, then a user), the objects are foreign-owned and start fails with the existing take-ownership hint.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| ACL of `%ProgramData%\Nurtuo` and below | now protected, three principals | stable once shipped |
| Log access | operators need an explicit `icacls` grant (documented) | stable |
| Go package `internal/privacl` | new (D1) | internal |
| Unix behaviour, CLI flags, config keys, proto | none | unchanged |

## Compatibility

Windows only, and only for hosts whose data root is open today. Behaviour change: other local users lose read access to the data root and to logs; scripts that read logs as a non-admin user break until granted. No breaking change to an API, proto or flag, so no breaking-change gate. Unix unchanged.

## Security

Closes the data read and planted-binary risks from issue #78. The check is fail-closed: foreign owner, reparse point, NULL DACL, an allow ACE for any other SID, or an unknown allow ACE type refuse to start. Accepted assumption from the parent spec: verify-then-open; swapping an object needs write access to a parent that is now private. The parent of the data root (`%ProgramData%`) is not changed.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| existing `judgeDACL` table tests, moved with D1 | decision logic, all OS | `cd core/runtime && go test ./internal/privacl/...` | |
| new Windows tests for `EnsureReport` | root, `postgres\data`, `runtime`, `cache`, `logs` private after create; open tree repaired; foreign owner and junction refused | `GOOS=windows go vet ./...`, run on the Windows CI job | `//go:build windows`, non-required job |
| Windows e2e: embedded Postgres starts under the protected DACL | gap 1 | Windows CI job | env-gated if the runner cannot download Postgres |
| manual two-account check | second local user cannot read `logs\` or write `postgres\runtime` | real Windows host | hard exit criterion, as for #57 |
| Unix regression | `EnsureReport` creates the same layout | `go test ./cmd/nurtuo/...` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Embedded Postgres fails to start on Windows under the protected DACL (restricted token) | high | runtime | verify on a Windows host before merge; if it fails, add the needed principal only after owner approval |
| 2 | Full walk of a large `postgres\data` slows every start | medium | runtime | measure; fall back to the D3 alternative |
| 3 | Data root previously used by another account is foreign-owned and start fails | medium | ops | existing take-ownership hint; document in `docs/operations.md` |
| 4 | Antivirus or backup agents run as another account and lose access | low | ops | document the `icacls` grant |

## Improvement opportunities

A `nurtuo standalone doctor` command that prints the data root ACL status without starting anything.

## References

- [Windows secrets DACL](implemented/2026-11-24-windows-secrets-dacl.md); issue #78.

## Changelog

- 2026-11-25: created (draft).
