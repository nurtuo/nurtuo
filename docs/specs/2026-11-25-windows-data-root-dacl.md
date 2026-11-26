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

- Goals: after `standalone` starts on Windows, the data root and everything below it (except `secrets\`, which stays under #57, D6) is owned by an allowed principal and has a private DACL under D2, protected on managed roots; a too-open tree that Nurtuo manages (see Repair semantics) is repaired at startup, and an operator-owned directory is only checked, with an `icacls` hint; a foreign owner, a reparse point or a multiply-linked file fails closed; an existing Postgres cluster with unsafe configuration does not start; embedded Postgres still starts; operators have a documented way to read logs.
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
- **Operator root** (check only) means any other existing configured root. Nurtuo changes no existing ACL in the tree outside `secrets\` (D6). A rejection names the offending path and reason and gives per-path `icacls` guidance for restoring the D2 grants and removing offending allow ACEs.

Both modes apply these standalone checks to the root and everything below it except the `secrets\` subtree, which keeps the #57 policy in both modes (D6). In an operator root, therefore, `secrets\` is still repaired as today; this is the one exception to check-only.

- **Owner**: must be the running account, `SYSTEM` or `BUILTIN\Administrators`. Any other owner fails closed. Repair never changes an owner (the take-ownership hint is printed).
- **Allow ACEs**: only the D2 principals; an allow ACE for any other SID, or an unknown ACE type, is too open. A NULL DACL is too open.
- **Effective access**: use the actual running token to verify rights needed on each object, such as directory traversal, enumeration and required child creation, and file read, write or execute access as applicable. An empty DACL, an inherit-only grant or a SYSTEM-only grant to a token lacking that SID is insufficient. Check-only roots fail on insufficient access. Managed paths can be repaired if safely inspectable, and must pass the effective-access check afterwards.
- **Deny ACEs**: any deny ACE (explicit or inherited) fails the check, for any SID. A deny on a D2 principal can lock Nurtuo out, and a deny on another SID is an operator rule we must not rewrite. Repair never removes or reorders deny ACEs; it refuses with the path and the ACE, in both modes.
- **Inheritance**: the private DACL is the #57 one, `D:P(A;OICI;FA;;;<running account>)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)`. Every inheritable allow ACE (`OI` or `CI`, explicit or inherited, including inherit-only ones) on a directory is judged like a direct grant, so `CREATOR OWNER` (`S-1-3-0`), `CREATOR GROUP` (`S-1-3-1`) or any other non-D2 SID in it is too open. Required state per mode:
  - *Managed root.* The root is protected and holds exactly the private DACL; Nurtuo sets protection only on a root it manages (at creation or when repairing it) and never on the parent of the root. Each layout directory Nurtuo creates, and each object it repairs, also gets the protected private DACL. Any other descendant passes if it is either protected with only D2 allow ACEs, or not protected and its ACEs are D2-only, where the inherited ones (`ID`) are the parent's inheritable D2 grants: `(A;OICIID;FA;;;X)` on a directory, `(A;ID;FA;;;X)` on a file. A descendant that fails is repaired by replacing its whole DACL with the protected private one, never by editing single ACEs. A repair write never propagates to children (see Repair containment): each object that needs a new DACL is written on its own, and an object that passes keeps its DACL byte for byte.
  - *Operator root.* Nurtuo changes no inheritance and no ACE on existing objects. The root and its descendants may be non-protected. Each object passes only if its effective ACEs (explicit plus inherited) satisfy the allow, deny and effective-access rules, and each directory has inheritable grants that are D2-only and give the running account at least traverse, list, add file, add subdirectory, read, write and delete with `OI|CI`, so files and directories created later (by Nurtuo or Postgres) are private and usable. Missing layout directories Nurtuo creates in an operator root get the protected private DACL at creation.
  - *New descendants.* In both modes, an object created after startup by the running process or by Postgres under its restricted token gets only inherited D2 ACEs (owner: the running account) and denies every other account; the Testing table verifies this under the real tokens.
- **Reparse points**: refused in both modes; never followed.
- **Hard links and file identity**: a file's DACL belongs to the file, not to the name, so a hard link to a file outside the root would carry a repair outside it. Preflight opens every object by handle without following reparse points (`FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS`) and records its identity (volume serial number and 128-bit file ID from `GetFileInformationByHandleEx(FileIdInfo)`) and its link count (`FileStandardInfo.NumberOfLinks`). Any file with more than one link is refused in both modes, before any ACL write, naming the path and the link count; the operator removes the extra link or moves the file out. An object whose volume serial differs from the root's is refused as leaving the root.
- **Repair containment**: invariant: the data-root repair writes a DACL only to an object that preflight recorded inside the root, reopened by handle with the same volume serial and file ID, and (for a file) with exactly one link, checked through that handle immediately before the write. Nothing else in the tree, and nothing outside it, is ever written, including through inheritance. Procedure, top-down:
  1. Writes use `SetKernelObjectSecurity` on the verified handle with `DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION`. This call does not propagate inheritable ACEs to existing children. `SetNamedSecurityInfo` and `SetSecurityInfo` do propagate, so the data-root repair never uses them (the #57 `setPrivateDACL` keeps them for `secrets\`, D6).
  2. Handles on the root and on every directory on the path to the object being repaired are held open, without `FILE_SHARE_DELETE`, from the reopen until the end of the repair, so those directories cannot be renamed or replaced meanwhile.
  3. A directory is repaired before anything below it. Once its private DACL is written, other accounts can no longer create, rename or link entries in it. Repair then lists the directory through its handle and compares names and file IDs with the preflight record. An added, removed or replaced entry aborts the repair: no further write, startup stops, and the error names the path.
  4. Each child that needs repair is then reopened and checked (identity, and link count for a file). A mismatch aborts the repair before that child is written. After the write, the link count is read again through the same handle; a count above one stops startup. The written object is still the one preflight vetted inside the root, so the extra name points to a private Nurtuo object and gives no access outside.
  5. After the repair, a verification walk (same handle-based inspection, operator-mode rules, no writes) must pass over the whole tree except `secrets\`; any failure stops startup. This also catches entries created through handles opened before the repair, because they are not owned by a D2 principal or are not private.

## Design

1. On Windows, `DataDirs.EnsureReport` normalizes the path, applies D5, then inspects the existing path components (including ancestors) for reparse points without following them. Classify the root and walk any existing tree before writing anything. Managed preflight records repairable DACLs with each object's file identity and refuses foreign owners, reparse points, multiply-linked files, objects on another volume, deny ACEs and unreadable objects. Operator preflight requires every object to pass. Rejection leaves the existing ACLs and filesystem entries unchanged.
2. After preflight, in this order: (a) create a missing root with its private DACL at creation and add the marker for a newly created configured root; (b) run `secrets.EnsurePrivateDir(secrets\)` under #57 (D6), whose report entries are merged into the startup output unchanged; (c) repair an existing managed tree top-down under Repair containment, then run the verification walk, keeping owners and using only D2 principals; (d) create missing layout directories with private DACLs in either mode. A preflight rejection stops before (a), so `secrets\` is not touched either. Unix behavior stays unchanged.
3. Embedded Postgres: it runs as the Nurtuo account. The DACL grants that account `FILE_ALL_ACCESS` with `OI|CI`, so `initdb` and the server create files that inherit it. Windows Postgres starts with a restricted token that still contains the user SID, so access by the user ACE should work. This is a hypothesis; it must be confirmed on a real host (real-host gate R2, gap 1).
4. Document log access through the running account, `SYSTEM` or Administrators, including elevation when using the Administrators grant. Apply D2 to logs on every start.
5. Startup output lists every tightened path (`standalone: tightened permissions on ...`), as for `secrets\`.
6. After ACL validation and any repair, verify Postgres artifacts before any executable is invoked. Verify cached archives against an expected digest for the pinned version and platform obtained from a trusted source outside the data root. Trusted digest source: a SHA-256 table keyed by Postgres version, OS and architecture, committed in `core/runtime/cmd/nurtuo` next to the version pin and compiled into the binary. It is changed only in the PR that changes the pin, by a maintainer who downloads the archive from Maven Central (`io.zonky.test.postgres:embedded-postgres-binaries-*`) and checks it against the checksum Maven Central publishes; the PR states both values. The `.sha256` file that embedded-postgres fetches from the same host is not trusted (same origin, and the library skips the check when that file is missing). A platform with no table entry fails startup on Windows. Downloads use the same table. Reuse a runtime tree only if its full contents and paths match the verified distribution. If a cached archive or runtime tree cannot be verified, obtain a verified archive and extract into a fresh private directory. Start Postgres from the verified tree exclusively. Fail startup if a verified distribution cannot be prepared. Apply the gate to existing installs, including operator roots that pass preflight, and on retries. Keep `postgres\data` in place.
7. Cluster gate: a verified distribution can still load code named by the cluster configuration, so on Windows an existing `postgres\data` (one with `PG_VERSION`) starts only if it is safe as defined here; otherwise startup fails closed before any Postgres executable (`postgres`, `pg_ctl`, `initdb`) is invoked, on first start and on retries. An existing cluster is safe when all of the following hold after steps 1, 2 and 6:
   - `postgres\data` passed preflight (and any repair) under step 1 and 2, so every file in it is owned by a D2 principal, single-linked and free of reparse points;
   - Postgres reads configuration only from `postgresql.conf`, `postgresql.auto.conf`, `pg_hba.conf` and `pg_ident.conf` directly in `postgres\data`: no `include`, `include_if_exists` or `include_dir` directive, and no `config_file`, `hba_file`, `ident_file` or `data_directory` setting;
   - none of these settings has a value other than the Postgres default: `shared_preload_libraries`, `local_preload_libraries`, `session_preload_libraries`, `dynamic_library_path`, `jit_provider`, `archive_mode`, `archive_command`, `archive_library`, `restore_command`, `archive_cleanup_command`, `recovery_end_command`, `ssl_passphrase_command`, `ssl` and the `ssl_*_file` settings, `unix_socket_directories`; `listen_addresses` is unset or loopback only, and `log_directory` is unset or a relative path inside `postgres\data`;
   - every `pg_hba.conf` record is a `host` or `hostssl` record for `127.0.0.1/32` or `::1/128` with method `password`, `md5` or `scram-sha-256` (embedded-postgres runs `initdb -A password`), and `pg_ident.conf` has no mappings.
   The parser follows the Postgres configuration grammar (later lines win, quoted values, `#` comments) and treats an unparsable line as unsafe. Nurtuo never edits cluster configuration. The error names the file, the line and the setting, and tells the operator to review the cluster, remove the setting or restore `postgres\data` from a backup they trust, then restart. A missing or empty `postgres\data` is initialised by `initdb` from the verified distribution as today. Unix behaviour stays unchanged.

## Phases and exit criteria

Each phase is one PR (or a stack merged in order), cites this spec, and lands its tests. A phase is done only when every exit criterion is met and the evidence is linked in the PR.

1. **Helper move (D1).** Move the helpers to `internal/privacl` with no behaviour change. Exit: `go test -race ./internal/privacl/... ./internal/secrets/...` and `GOOS=windows go vet ./...` pass in `core/runtime`; every #57 test (`TestJudgeDACL`, `TestEnsurePrivateDir*`, `TestCheckPermWindows*`, `TestWriteFileAtomicPrivateFromCreation`, `TestFileKeySourcePrivateKeyFile`, `TestOpenFailsClosedOnOpenOperatorStore`, `TestDirSetCreatesMissingOperatorDirPrivate`) passes unmodified except for import paths, on Linux CI and in the Windows job; the exported API of `internal/secrets` is unchanged.
2. **Root policy.** D5, preflight, root classification and marker trust, hard links and file identity, inheritance, effective access, repair under Repair containment, the `secrets\` boundary (D6), startup report. Exit: the Windows rows of the Testing table for preflight, marker trust, hard link, repair containment, inheritance, effective access and `secrets\` boundary pass in the Windows CI job with none skipped except as allowed below, and the job log is linked in the PR; Unix regression passes; a second start on a repaired tree reports no tightened path.
3. **Postgres.** Pinned digest table, artifact verification (step 6), cluster gate (step 7), retry behaviour, restricted-token startup. Exit: cluster gate parser tests pass on every OS; the Windows rows for Postgres upgrade, cluster gate and embedded Postgres e2e pass in the Windows CI job; a retry after a failed verification never starts unverified binaries (asserted by the injected process starter); restricted-token startup under the protected DACL passes on a real host (gate R2 below).
4. **Docs and real-host gates.** Update `docs/operations.md` and `docs/security.md` (log access, take-ownership hint, cluster gate recovery). Exit: `node scripts/check-docs-links.mjs` passes; gates R1–R3 below are recorded as passed. Issue #78 closes only after this phase.

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

Private DACLs restrict future access under D2. The Postgres integrity gate covers runtime files and cached archives that predate hardening, and the cluster gate (Design step 7) covers configuration that can load code, run commands or open local authentication. Earlier disclosure or tampering with database contents requires separate operator assessment. Preflight fails closed on unsafe paths or ACLs that the selected mode cannot accept or repair. As in the parent spec, path-based use after verification requires protection against replacement through the parent. If an untrusted principal can replace a validated object, hold it and the relevant path components stable with Windows handles during validation and use, or fail closed. Repair stays inside the managed root: it writes only preflight-recorded, re-verified, single-linked objects through their own handles with a non-propagating call (Repair containment).

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
| Windows repair containment | a Windows-only test hook runs between preflight and the repair, and again just before each write. Cases, each in a too-open managed root: (a) a new entry inside a repaired directory hard-linked to an outside file; (b) a recorded file replaced by a hard link to an outside file; (c) a second link from an outside directory added to a recorded inside file just before its write; (d) a link count raised right after a write; (e) a non-protected child with only inherited D2 ACEs under a repaired parent. (a)-(c) abort before the affected object is written; the outside file's DACL and the new entry's DACL are byte-identical to before; startup stops with the path. (d) stops startup after the write. (e) keeps the child's DACL byte-identical (no propagation). Each case asserts that no `SetNamedSecurityInfo`/`SetSecurityInfo` call happened, through the writer seam. | Windows CI job | the hook and the writer seam exist only in `_test` builds |
| Windows effective access | empty, inherit-only and SYSTEM-only DACLs fail in check-only mode when the running token lacks the required rights; a usable private DACL passes | Windows CI job | run the SYSTEM-only case as a non-SYSTEM account |
| Windows Postgres upgrade | a cached archive whose SHA-256 differs from the committed table, a runtime tree with a changed, added or missing file, and a platform with no table entry: invalid runtime files and cached archives are never used after ACL repair or on retry; verified replacement launches; unavailability of verified artifacts fails startup; database files stay in place | Windows CI job | cover managed and checked operator roots |
| cluster gate parser table tests | each unsafe setting, include directive, non-loopback or `trust`/`peer`/`ident` `pg_hba.conf` record, override by a later line or by `postgresql.auto.conf`, and an unparsable line is rejected with file, line and setting; a cluster freshly made by `initdb -A password` passes | `cd core/runtime && go test ./cmd/nurtuo/...` | pure function, runs on every OS |
| Windows cluster gate | an existing `postgres\data` with `shared_preload_libraries` set (and separately with `include_dir`, `archive_command`, `pg_hba.conf` `trust`): startup fails with the file and setting, and no Postgres process is started (the test injects a process starter that fails if called) on first start and on retry; the cluster files are unchanged | Windows CI job | cover managed and checked operator roots |
| manual two-account check | gate R1 | real Windows host | hard exit criterion, as for #57; see Real-host gates |
| Unix regression | `EnsureReport` creates the same layout | `go test ./cmd/nurtuo/...` | |

### Real-host gates

CI cannot prove these, or may skip them on hosted runners (no second account, no restricted-token Postgres). Each is a hard exit criterion. Record the result in this table (date, Windows build, account types, commands run and output or a linked log) in the PR of the phase that needs it; a skipped CI row counts as not run.

| # | Gate | Needed by | Result |
|---|---|---|---|
| R1 | Second local non-admin user cannot read `logs\` or `postgres\data`, and cannot create or replace files in `postgres\runtime` or `postgres\cache`, on a fresh install and on an upgraded pre-hardening install | phase 4 | not run |
| R2 | Embedded Postgres runs `initdb`, starts, restarts and survives a retry under the protected DACL with its restricted token (gap 1) | phase 3 | not run |
| R3 | The CI rows that logged `SKIPPED` (second account, SYSTEM-only as a non-SYSTEM account) pass when run by hand | phase 2 | not run |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Embedded Postgres fails to start on Windows under the protected DACL (restricted token) | high | runtime | real-host gate R2 before phase 3 is done; if it fails, add the needed principal only after owner approval |
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
- 2026-11-26: after Astra review of #112: hard links refused and file identity pinned during repair; cluster gate for an existing `postgres\data`; `secrets\` left to the #57 policy as pending D6; inheritance defined per root mode with new-descendant tests; phases with exit criteria, committed digest table and real-host gates. D1-D6 still await owner approval.
