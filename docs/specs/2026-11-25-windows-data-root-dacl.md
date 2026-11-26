<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Private DACL for the Windows standalone data root, postgres and logs

Status: draft. Date: 2026-11-25.

## TLDR

`nurtuo standalone` on Windows keeps its data root in `%ProgramData%\Nurtuo`, which inherits read and create access for the local `Users` group. Only `secrets\` is protected today (issue #57). This spec extends the same protected DACL (running account + `SYSTEM` + `BUILTIN\Administrators`) to the whole data root, including `postgres\data`, `postgres\runtime`, `postgres\cache` and `logs\`, and moves the ACL helpers out of `internal/secrets`. `secrets\` keeps the #57 policy (D6). It needs owner approval of D1-D6 before any code.

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
- Non-goals: `secrets\` semantics (unchanged, #57; the data-root walk stops at `secrets\`, see D6); Unix behaviour (unchanged); operator-chosen secrets paths (D9 of the parent spec still applies); encrypting data at rest; Windows service ACLs.

## Decisions (owner approval needed)

Recommendations are in bold; none is approved yet.

- **D1 Shared ACL layer.** **Move the private-path helpers (`EnsurePrivateDir`, `checkPerm`, `judgeDACL`, `mkdirPrivate`, `setPrivateDACL`, `visitTree`, `aclError`) into a new package `core/runtime/internal/privacl`, and let `internal/secrets` call it.** Pure move, no behaviour change, resolves the `TODO(#78)`. Alternative: keep them in `internal/secrets` and export `EnsurePrivateDir` only (smaller diff, but the data root keeps depending on the secrets package).
- **D2 Principal set.** **Same as D4 of the parent spec: the running account, `SYSTEM`, `BUILTIN\Administrators`.** No extra "operators" group.
- **D3 Scope of the walk.** **Walk the entire existing root except the `secrets\` subtree (D6), including `postgres\data`, `postgres\runtime`, `postgres\cache` and `logs\`, on every start.** Repair only in managed mode. Measure the startup cost of walking large databases before reconsidering the scope.
- **D4 Operator-chosen data root.** **Check an existing `NURTUO_DATA_DIR` without repairing it unless the root and its marker meet the trust checks below.** If the root is absent, create it with the private DACL at creation and exclusively create `.nurtuo-managed` as a regular file with its own protected private DACL. If a root appears before exclusive creation succeeds, validate it as an existing root. Leave the existing parent untouched. The check-only policy follows D9 of the parent spec.
- **D5 Rejected roots.** **Startup fails with an error, before any check or change, if the data root is a drive root (`C:\`, `\\?\C:\`, a UNC share root) or is `%ProgramData%` itself.** Also rejected: a root that is a parent of `%ProgramData%`, `%SystemRoot%` or a user profile root. The message names the offending path and asks for a dedicated subdirectory.
- **D6 Ownership of `secrets\`.** Issue #78 excludes `secrets\`. **The #57 policy owns the `secrets\` subtree in both root modes: the data-root preflight and repair treat `secrets\` as a boundary (they neither judge, repair nor report it or anything below it), and the existing `secrets.EnsurePrivateDir(secrets\)` call stays the only code that checks and repairs it, with the #57 rules unchanged (repaired in a managed root and in an operator root, as today).** D5 and the ancestor reparse check of Design step 1 still run first, because they concern the root path. `EnsurePrivateDir(secrets\)` runs before any data-root repair, so `secrets\` already has its protected DACL and inheritable ACEs propagated from a repaired root stop there. Alternative (scope extension, needs explicit owner approval because #78 excludes it): apply this spec's rules (deny ACEs, effective access, hard links, check-only on operator roots) to `secrets\` as well, amending the #57 behaviour; the hard-link and deny-ACE gaps of #57 would then close here instead of in a follow-up (gap 5).

## Repair semantics

- **Managed root** (repair allowed) means the root selected by the Windows default when `NURTUO_DATA_DIR` is unset, a root Nurtuo exclusively created this run, or an existing configured root with a trusted `.nurtuo-managed` marker. To trust the marker, require both the root and the marker to already pass the owner, private DACL and effective-access checks below and to have protected DACLs. Inspect the marker as a regular file without following reparse points. An absent or untrusted marker grants no repair authority. Check the root as operator-chosen, including checking the marker itself if present.
- **Operator root** (check only) means any other existing configured root. Nurtuo changes no existing ACL in the tree. A rejection names the offending path and reason and gives per-path `icacls` guidance for restoring the D2 grants and removing offending allow ACEs.

Both modes apply these standalone checks to the root and everything below it except the `secrets\` subtree, which keeps the #57 policy in both modes (D6). In an operator root, therefore, `secrets\` is still repaired as today; this is the one exception to check-only.

- **Owner**: must be the running account, `SYSTEM` or `BUILTIN\Administrators`. Any other owner fails closed. Repair never changes an owner (the take-ownership hint is printed).
- **Allow ACEs**: only the D2 principals; an allow ACE for any other SID, or an unknown ACE type, is too open. A NULL DACL is too open.
- **Effective access**: use the actual running token to verify rights needed on each object, such as directory traversal, enumeration and required child creation, and file read, write or execute access as applicable. An empty DACL, an inherit-only grant or a SYSTEM-only grant to a token lacking that SID is insufficient. Check-only roots fail on insufficient access. Managed paths can be repaired if safely inspectable, and must pass the effective-access check afterwards.
- **Deny ACEs**: any deny ACE (explicit or inherited) fails the check, for any SID. A deny on a D2 principal can lock Nurtuo out, and a deny on another SID is an operator rule we must not rewrite. Repair never removes or reorders deny ACEs; it refuses with the path and the ACE, in both modes.
- **Inheritance**: the private DACL is the #57 one, `D:P(A;OICI;FA;;;<running account>)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)`. Every inheritable allow ACE (`OI` or `CI`, explicit or inherited, including inherit-only ones) on a directory is judged like a direct grant, so `CREATOR OWNER` (`S-1-3-0`), `CREATOR GROUP` (`S-1-3-1`) or any other non-D2 SID in it is too open. Required state per mode:
  - *Managed root.* The root is protected and holds exactly the private DACL; Nurtuo sets protection only on a root it manages (at creation or when repairing it) and never on the parent of the root. Each layout directory Nurtuo creates, and each object it repairs, also gets the protected private DACL. Any other descendant passes if it is either protected with only D2 allow ACEs, or not protected and its ACEs are D2-only, where the inherited ones (`ID`) are the parent's inheritable D2 grants: `(A;OICIID;FA;;;X)` on a directory, `(A;ID;FA;;;X)` on a file. A descendant that fails is repaired by replacing its whole DACL with the protected private one, never by editing single ACEs. Setting a DACL on a directory propagates inheritable ACEs to non-protected children; this is intended, because preflight has already vetted that subtree (no reparse point, no multiply-linked file, same volume) and `secrets\` is protected before any data-root repair (D6).
  - *Operator root.* Nurtuo changes no inheritance and no ACE on existing objects. The root and its descendants may be non-protected. Each object passes only if its effective ACEs (explicit plus inherited) satisfy the allow, deny and effective-access rules, and each directory has inheritable grants that are D2-only and give the running account at least traverse, list, add file, add subdirectory, read, write and delete with `OI|CI`, so files and directories created later (by Nurtuo or Postgres) are private and usable. Missing layout directories Nurtuo creates in an operator root get the protected private DACL at creation.
  - *New descendants.* In both modes, an object created after startup by the running process or by Postgres under its restricted token gets only inherited D2 ACEs (owner: the running account) and denies every other account; the Testing table verifies this under the real tokens.
- **Reparse points**: refused in both modes; never followed.
- **Hard links and file identity**: a file's DACL belongs to the file, not to the name, so a hard link to a file outside the root would carry a repair outside it. Preflight opens every object by handle without following reparse points (`FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS`) and records its identity (volume serial number and 128-bit file ID from `GetFileInformationByHandleEx(FileIdInfo)`) and its link count (`FileStandardInfo.NumberOfLinks`). Any file with more than one link is refused in both modes, before any ACL write, naming the path and the link count; the operator removes the extra link or moves the file out. An object whose volume serial differs from the root's is refused as leaving the root. Repair reopens each object by handle, compares its identity and link count with the preflight record, and writes the DACL through that handle (`SetSecurityInfo`), never by path; a mismatch aborts the repair with the path and stops startup.

## Design

1. On Windows, `DataDirs.EnsureReport` normalizes the path, applies D5, then inspects the existing path components (including ancestors) for reparse points without following them. Classify the root and walk any existing tree before writing anything. Managed preflight records repairable DACLs with each object's file identity and refuses foreign owners, reparse points, multiply-linked files, objects on another volume, deny ACEs and unreadable objects. Operator preflight requires every object to pass. Rejection leaves the existing ACLs and filesystem entries unchanged.
2. After preflight, create a missing root with its private DACL at creation and add the marker for a newly created configured root. Repair an existing managed tree top-down and re-check it before use. Repairs keep the owner and use only D2 principals. Then create missing layout directories with private DACLs in either mode. `secrets\` is handled by `secrets.EnsurePrivateDir` under #57 before any data-root repair, and its report entries are merged into the startup output unchanged (D6). Unix behavior stays unchanged.
3. Embedded Postgres: it runs as the Nurtuo account. The DACL grants that account `FILE_ALL_ACCESS` with `OI|CI`, so `initdb` and the server create files that inherit it. Windows Postgres starts with a restricted token that still contains the user SID, so access by the user ACE should work. This is a hypothesis; it must be confirmed on a real host (see Testing and gap 1).
4. Document log access through the running account, `SYSTEM` or Administrators, including elevation when using the Administrators grant. Apply D2 to logs on every start.
5. Startup output lists every tightened path (`standalone: tightened permissions on ...`), as for `secrets\`.
6. After ACL validation and any repair, verify Postgres artifacts before any executable is invoked. Verify cached archives against an expected digest for the pinned version and platform obtained from a trusted source outside the data root. Reuse a runtime tree only if its full contents and paths match the verified distribution. If a cached archive or runtime tree cannot be verified, obtain a verified archive and extract into a fresh private directory. Start Postgres from the verified tree exclusively. Fail startup if a verified distribution cannot be prepared. Apply the gate to existing installs, including operator roots that pass preflight, and on retries. Keep `postgres\data` in place.
7. Cluster gate: a verified distribution can still load code named by the cluster configuration, so on Windows an existing `postgres\data` (one with `PG_VERSION`) starts only if it is safe as defined here; otherwise startup fails closed before any Postgres executable (`postgres`, `pg_ctl`, `initdb`) is invoked, on first start and on retries. An existing cluster is safe when all of the following hold after steps 1, 2 and 6:
   - `postgres\data` passed preflight (and any repair) under step 1 and 2, so every file in it is owned by a D2 principal, single-linked and free of reparse points;
   - Postgres reads configuration only from `postgresql.conf`, `postgresql.auto.conf`, `pg_hba.conf` and `pg_ident.conf` directly in `postgres\data`: no `include`, `include_if_exists` or `include_dir` directive, and no `config_file`, `hba_file`, `ident_file` or `data_directory` setting;
   - none of these settings has a value other than the Postgres default: `shared_preload_libraries`, `local_preload_libraries`, `session_preload_libraries`, `dynamic_library_path`, `jit_provider`, `archive_mode`, `archive_command`, `archive_library`, `restore_command`, `archive_cleanup_command`, `recovery_end_command`, `ssl_passphrase_command`, `ssl` and the `ssl_*_file` settings, `unix_socket_directories`; `listen_addresses` is unset or loopback only, and `log_directory` is unset or a relative path inside `postgres\data`;
   - every `pg_hba.conf` record is a `host` or `hostssl` record for `127.0.0.1/32` or `::1/128` with method `password`, `md5` or `scram-sha-256` (embedded-postgres runs `initdb -A password`), and `pg_ident.conf` has no mappings.
   The parser follows the Postgres configuration grammar (later lines win, quoted values, `#` comments) and treats an unparsable line as unsafe. Nurtuo never edits cluster configuration. The error names the file, the line and the setting, and tells the operator to review the cluster, remove the setting or restore `postgres\data` from a backup they trust, then restart. A missing or empty `postgres\data` is initialised by `initdb` from the verified distribution as today. Unix behaviour stays unchanged.

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

Private DACLs restrict future access under D2. The Postgres integrity gate covers runtime files and cached archives that predate hardening, and the cluster gate (Design step 7) covers configuration that can load code, run commands or open local authentication. Earlier disclosure or tampering with database contents requires separate operator assessment. Preflight fails closed on unsafe paths or ACLs that the selected mode cannot accept or repair. As in the parent spec, path-based use after verification requires protection against replacement through the parent. If an untrusted principal can replace a validated object, hold it and the relevant path components stable with Windows handles during validation and use, or fail closed. Repair stays inside the managed root.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| existing `judgeDACL` table tests, moved with D1 | decision logic, all OS | `cd core/runtime && go test ./internal/privacl/...` | |
| new Windows tests for `EnsureReport` | root, `postgres\data`, `runtime`, `cache`, `logs` private after create; open tree repaired; foreign owner and junction refused | `GOOS=windows go vet ./...`, run on the Windows CI job | `//go:build windows`, non-required job |
| Windows e2e: embedded Postgres starts under the protected DACL | gap 1 | Windows CI job | env-gated if the runner cannot download Postgres |
| Windows marker trust | trusted marker in a private protected configured root permits child repair; a foreign-owned marker or a marker in an open existing root grants no repair authority; compare existing ACLs byte for byte | Windows CI job | include a missing layout child and verify it remains absent on rejection |
| Windows read-only preflight | drive/share root, `%ProgramData%`, ancestor junction, descendant reparse point, deny ACE, foreign owner or failing operator ACL rejects startup before creating any layout directory or marker or changing any ACL | Windows CI job | verify a passing operator root can create missing layout directories privately |
| Windows `secrets\` boundary (D6) | in a managed root and in an operator root: a too-open `secrets\` (inherited `Users` ACE) is repaired exactly as by #57 with the same report line; a foreign owner or reparse point in `secrets\` fails as in #57; a deny ACE or a second hard link inside `secrets\` is handled as by #57 (not by this spec's rules); repairing a too-open managed root leaves the `secrets\` DACL byte-identical; an operator root with a failing `logs\` still fails before `secrets\` is touched | Windows CI job | the existing #57 tests (`TestEnsurePrivateDir*`, `TestOpenFailsClosedOnOpenOperatorStore`) stay green unchanged |
| Windows inheritance of new descendants | in a managed and an operator root, after startup the test process (real token) creates a file and a subdirectory in the root, `postgres\data` and `logs\`, and Postgres creates its own files and WAL under its restricted token; each new object's owner is the running account and its DACL holds only inherited D2 ACEs with the flags above; a second local non-admin account (token from `LogonUser`) gets `ACCESS_DENIED` opening a new file for read and creating a file in a new directory; a `CREATOR OWNER` inherit-only ACE is repaired on a managed child and rejected in an operator root; a second start reports no tightened path | Windows CI job | the second account needs an admin runner; if account creation is unavailable the test logs `SKIPPED: no second account` and the real-host gate applies |
| Windows hard link | a file in `postgres\runtime` and in `logs\` of a too-open managed root hard-linked to a file outside the root: startup refuses before any ACL write, and the DACLs of both names (inside and outside) are byte-identical to before; the same in an operator root; an object replaced between preflight and repair (different file ID) aborts the repair with no ACL change on the replacement | Windows CI job | links created with `os.Link` (`CreateHardLink`); the outside file is in a temp dir with a deliberately open DACL |
| Windows effective access | empty, inherit-only and SYSTEM-only DACLs fail in check-only mode when the running token lacks the required rights; a usable private DACL passes | Windows CI job | run the SYSTEM-only case as a non-SYSTEM account |
| Windows Postgres upgrade | invalid runtime files and cached archives are never used after ACL repair or on retry; verified replacement launches; unavailability of verified artifacts fails startup; database files stay in place | Windows CI job | cover managed and checked operator roots |
| cluster gate parser table tests | each unsafe setting, include directive, non-loopback or `trust`/`peer`/`ident` `pg_hba.conf` record, override by a later line or by `postgresql.auto.conf`, and an unparsable line is rejected with file, line and setting; a cluster freshly made by `initdb -A password` passes | `cd core/runtime && go test ./cmd/nurtuo/...` | pure function, runs on every OS |
| Windows cluster gate | an existing `postgres\data` with `shared_preload_libraries` set (and separately with `include_dir`, `archive_command`, `pg_hba.conf` `trust`): startup fails with the file and setting, and no Postgres process is started (the test injects a process starter that fails if called) on first start and on retry; the cluster files are unchanged | Windows CI job | cover managed and checked operator roots |
| manual two-account check | second local user cannot read `logs\` or write `postgres\runtime` | real Windows host | hard exit criterion, as for #57 |
| Unix regression | `EnsureReport` creates the same layout | `go test ./cmd/nurtuo/...` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Embedded Postgres fails to start on Windows under the protected DACL (restricted token) | high | runtime | verify on a Windows host before merge; if it fails, add the needed principal only after owner approval |
| 2 | Full walk of a large `postgres\data` slows every start | medium | runtime | measure before proposing any change to D3 |
| 3 | Data root previously used by another account is foreign-owned and start fails | medium | ops | existing take-ownership hint; document in `docs/operations.md` |
| 4 | Antivirus or backup agents run as another account and lose access | low | ops | document access under an existing D2 principal |
| 5 | Under D6, `secrets\` keeps the #57 rules, which ignore deny ACEs and do not refuse multiply-linked files | medium | secrets | follow-up issue against #57 after approval, unless the owner picks the D6 alternative |

## Improvement opportunities

A `nurtuo standalone doctor` command that prints the data root ACL status without starting anything.

## References

- [Windows secrets DACL](implemented/2026-11-24-windows-secrets-dacl.md); issue #78.

## Changelog

- 2026-11-25: created (draft).
- 2026-11-25: resolved D4/D9 conflict (operator roots are check-only), added D5 (rejected roots) and the Repair semantics section (owner, deny ACEs, inheritance, managed vs operator root) after review of #112.
- 2026-11-25: specified marker trust, read-only preflight, D2 log access, Postgres artifact verification and effective-access checks after re-review of #112.
