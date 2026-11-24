<!-- SPDX-License-Identifier: Apache-2.0 -->
# Pre-implement: Owner-only Windows DACL for standalone secrets and private files

**Readiness:** no-go
**Spec:** docs/specs/2026-11-24-windows-secrets-dacl.md

Date: 2026-11-24. Analysis only. The spec's product decisions were not changed. Checked against `main` at `7953d4f`.

## Blockers

- Status is `draft`. The readiness gate cannot pass until the owner approves it.
- Three blocking `needs-owner` decisions are open:
  - **D4 (Q1):** DACL shape. The recommendation (token user + `SYSTEM` + `BUILTIN\Administrators`) departs from the issue's "owner-only".
  - **D7 (Q2):** scope. Is it `secrets\` only, or also the data root and `postgres\data`?
  - **D9 (Q3):** should repair and fail-closed also apply to the non-standalone Windows runtime (`secrets.Open`)? The answer changes Phase 3 step 1. It also decides whether Phase 2 can ship as designed, because the real `checkPerm` guards the shared read paths.
- D13 (Q4: make the Windows job a required check) is also `needs-owner` but non-blocking.

When D4, D7 and D9 are answered and the spec is `approved`, the remaining findings below are Warnings or Important. That would give a `conditional` verdict.

## Rules loaded

- `AGENTS.md` / `CLAUDE.md` require:
  - spec first, because this is security work (secrets) and the status must be `approved` before code;
  - tests in the same PR;
  - SPDX headers; `core/` is AGPL;
  - CI changes that only add jobs need no owner approval;
  - required checks and branch protection fall under Ask First;
  - a new production dependency falls under Ask First.
- `docs/specs/README.md` and `_template.md`: all required sections are present. Goal, Open Questions, Scope / Non-goals, Design, Step plan, Acceptance criteria and Tests are there, along with Contracts, Compatibility, Security, Gaps and risks, References and Changelog.
- Compatibility doc: `docs/specs/2026-11-22-backward-compatibility-contract.md` exists but is a draft. The spec has its own Contracts and Compatibility sections.
- Lessons: `docs/lessons/ci-label-gates-and-duplex-grpc-tests.md` was opened because the spec adds a non-required CI job. It describes a non-blocking job that did not compile, so its test never ran. That applies directly to the proposed `go-windows` job (see W3). `proto-message-name-descriptor-breaks-csharp.md` does not match this area and was not opened.

## Codebase grounding (spec claim -> code)

| Spec claim | Evidence | OK |
|---|---|---|
| Windows `checkPerm` / `chmodPrivate` are no-ops | `core/runtime/internal/secrets/perm_windows.go:11,13` | yes |
| Unix `checkPerm` rejects `mode & 0o077`; `chmodPrivate` = `chmod 0600` | `perm_unix.go:14-21` | yes |
| `checkPerm` call sites: `Dir.checkDir`, `Dir.Get`, `FileKeySource.Key`, `Encrypted.load` | `dir.go:30,61`, `encrypted.go:52,147` | yes |
| `chmodPrivate` only in `writeFileAtomic`, after `os.CreateTemp` | `dir.go:125,131` | yes |
| `MkdirAll(…, 0o700)` in `Dir.Set`, `FileKeySource.Key`, `Encrypted.save`; key via `O_EXCL` `0600` | `dir.go:83`, `encrypted.go:68,72,185` | yes |
| `Encrypted` does not check its directory | `encrypted.go` `load`/`save` | yes |
| `DefaultDataRoot` prefers `%ProgramData%\Nurtuo`, then `%LOCALAPPDATA%` | `cmd/nurtuo/standalone_layout.go:24-35` | yes |
| `DataDirs.Ensure` uses `MkdirAll` `0755` / `0700` | `standalone_layout.go:65-72` | yes |
| Bootstrap files written with `os.WriteFile` and never checked on read | `standalone_bootstrap.go:56,67,70,152,156` | yes |
| Standalone store at `secrets\store`, `encrypted-file` | `standalone_register.go:72`, `secret.go:125` | yes |
| `secrets.Open` runs at runtime start | `internal/runtime/run.go:45` | yes |
| `nurtuo service` runs the distributed runtime, not standalone | `cmd/nurtuo/service.go` (`serviceConfig` args `service run --config`) | yes |
| No Windows runner in CI; `go` job only cross-compiles `cmd/nurtuo` | `.github/workflows/ci.yml` (all `runs-on: ubuntu-latest`; lines 30-34) | yes |
| `golang.org/x/sys` v0.50.0 is indirect and already in the Windows build | `core/runtime/go.mod:44`; `GOOS=windows go list -deps ./cmd/nurtuo` lists `golang.org/x/sys/windows` (via `kardianos/service`) | yes |
| x/sys API names exist (`SetNamedSecurityInfo`, `GetNamedSecurityInfo`, `ACLFromEntries`, `GetAce`, `CreateWellKnownSid`, `GetTokenUser`, `CreateDirectory`, `PROTECTED_DACL_SECURITY_INFORMATION`, `ACL.AceCount`); `LogonUserW` / `ImpersonateLoggedOnUser` not wrapped | module cache `golang.org/x/sys@v0.50.0/windows/{security_windows.go,zsyscall_windows.go}` | yes |
| BSD-3-Clause is allowed | `.github/licenses/agpl-allow.txt` | yes |
| Prior-doc drift ("profile ACL") | `perm_windows.go:9-10`, ADR 0010 `dir` row, `docs/security.md` "Secrets handling" | yes, recorded in the spec |
| Proposed test names do not collide | `standalone_test.go` has no `TestDataDirsEnsure*`; `secrets_test.go` has no `TestJudgeDACL` / `TestCheckPermWindows*` | yes |

No mis-description of the code was found.

Not verified (and the spec says so):

- the default ACL of `C:\ProgramData` on a real host;
- whether hosted `windows-latest` runners allow `net user /add` and `LogonUserW` in a test.

## Findings

- **Critical (readiness gate): the spec is `draft` with blocking owner decisions D4, D7 and D9.**
  - Evidence: spec "Open Questions" and "Decisions".
  - Fix: owner answers, then set `approved` with the answers recorded in Decisions and the Changelog.
- **Warning W1: D9 is coupled to Phase 2.**
  - The real Windows `checkPerm` also guards the distributed runtime's read paths.
  - With D9 = (c) (standalone only), the design needs a standalone-only switch that the spec does not describe. The spec says to amend it first, which is correct.
  - Fix: if the owner picks (c), add that switch to Design before approval.
- **Warning W2: the ProgramData ACL assumptions are unverified.**
  - The design relies on `Users` read and `Users` create-files/folders being inherited, which drives gap 2 (key planting).
  - The issue itself says this was never verified on a host.
  - Fix: during implementation, record the `icacls %ProgramData%` and `icacls %ProgramData%\Nurtuo\secrets` output before and after in the PR. The spec's manual check covers "after"; add "before".
- **Warning W3: a non-required CI job can rot silently.** See the lesson `ci-label-gates-and-duplex-grpc-tests.md`.
  - If `go-windows` is not required (D13 recommendation), a compile break or a permanent skip of `TestOtherLocalUserCannotReadSecrets` can go unnoticed.
  - Fix:
    - the job should fail when the gated test reports `SKIP` while `NURTUO_TEST_WINDOWS_OTHER_USER=1` is set (for example `go test -json` plus a check);
    - keep `GOOS=windows go vet` on Linux so compile breaks also show up in a fast job.
- **Important I1: the `DataDirs.Ensure` signature change touches five callers.**
  - Callers: `standalone.go:56,163`, `standalone_e2e_test.go:62`, `standalone_test.go:258`, `standalone_enroll_test.go:50,79`.
  - The spec leaves the shape to the implementer (internal API).
  - Fix: pick one shape in the implementation PR and update all callers. Alternatively, keep `Ensure() error` and add a separate `EnsureReport`.
- **Important I2: the test seam for `TestWriteFileAtomicPrivateFromCreation` is unspecified.**
  - Fix: name the hook (for example a package-level `afterPrivateCreate func(*os.File)` used only in tests) when writing the test.
- **Important I3: direct use of `golang.org/x/sys/windows`.**
  - The module is already linked, so this is not a new module. AGENTS.md "Ask First: a new production dependency" does not strictly apply.
  - D10 classifies it as an implementer default with that evidence.
  - Fix: none required. Mention it in the implementation PR body so the reviewer sees the `go.mod` `// indirect` removal.
- **Important I4: the error-message SID is not resolved to a name.**
  - Operators see `S-1-5-21-…`.
  - Fix (optional): add the account name via `LookupAccountSid` when it resolves. Keep the SID for the case where it does not.
- **Important I5: existing material may already be compromised.**
  - D12 is documentation only and no rotation tool exists.
  - Fix: the release note text should be part of the implementation PR's docs step (Phase 3 step 3). The spec already says so.

Contract and compatibility audit:

- No proto, REST, CLI flag, runtime config key or bundle/flow format changes. The data directory layout paths are unchanged.
- Behaviour changes are Windows-only and covered by the Compatibility section:
  - repaired DACLs;
  - fail-closed on a foreign owner;
  - the distributed runtime, pending D9.
- No migration is needed. The spec gives the rollback (`icacls /reset`).

Repo-rule compliance:

- The code goes in `core/runtime` (AGPL). The CI job only adds a job, so no owner approval is needed. Required-check promotion is correctly marked `needs-owner` (D13).
- Tests are planned in the same PR.
- No violations found.

Risks:

- **High:** cross-user denial is proven only by the gated test plus a manual check (gap 8, W3).
- **Medium:** lock-out of a later different account (gap 4). Mitigated by D4 (b) and a clear error.
- **Medium:** ACE-type coverage (gap 7). Mitigated by fail-closed on unknown types and by table tests.

## During implementation

These do not apply while the verdict is `no-go`. They become the conditions if the owner approves with D4/D7/D9 answered:

- W2: record `icacls` output before and after on a real host in the PR.
- W3: make the gated two-user test fail the job when it skips under the env var; add `GOOS=windows go vet`.
- I1, I2: fix the `Ensure` shape and the test seam.

## Next action

- Owner answers D4, D7 and D9, and optionally D13.
- The spec author records the answers, sets Status `approved` and adds a changelog line.
- Re-run this check. The expected verdict is `conditional` with the items above.
- Independently of approval, ship the Phase 1 interim warning as a docs-only PR (D5).

```
Readiness: no-go
Report: docs/specs/analysis/pre-implement-windows-secrets-dacl.md
Spec: docs/specs/2026-11-24-windows-secrets-dacl.md
```
