<!-- SPDX-License-Identifier: Apache-2.0 -->
# Pre-implement: `nurtuo flow run`

**Readiness:** conditional
**Spec:** docs/specs/2026-11-23-nurtuoctl-run.md

Date: 2026-11-23. Analysis only: the spec and application code were not edited.

## Blockers

None. No Critical finding. The spec is `approved`, has no blocking Open Question, has every required
section, and its "What exists today" section matches the code (verified below).

## Rules loaded

- The repository has no `AGENTS.md` or `CLAUDE.md` (searched `git ls-files`); the closest rules are
  `Makefile` (validation targets), `.github/workflows/ci.yml`, `.github/PULL_REQUEST_TEMPLATE.md` and
  `scripts/check-boundaries.sh`. Said here so the absence is explicit.
- No compatibility/contracts document exists beyond the spec's own Compatibility section (protected
  surfaces `nurtuo.flow/v1`, `nurtuo.connection/v1`, SFTP connector and `nurtuo run` are untouched).
- Lessons: `docs/lessons/` does not exist.

## Codebase grounding (spec claim -> what the code does)

| Spec claim | Verified in | Result |
|---|---|---|
| `register(name, fn)`, `func(args, stdout, stderr) int`, commands `run/standalone/service/secret/version`, `role` stub | `core/runtime/cmd/nurtuo/main.go` | matches |
| `nurtuo run` rejects positionals with exit 2 (`fs.NArg() > 0`) | `cmd/nurtuo/run.go` | matches |
| `nativeConnectors()` = `connhost.Builtins()` (http, sftp, postgres, nats) | `run.go`, `connhost/registry.go` | matches |
| `engine.New()` registers only message/setProperties/map; `start` with no runner is passed through; unknown kinds fail with `no_runner` | `engine/runner.go`, `engine/interp.go:242-251` | matches; see item 3 |
| `Result.Returned/Failed/Stop`, `StopInfo.Status` is set from the Stop step's `status` | `engine/interp.go:49-70,196-201` | matches |
| `connections.Parse`, `Resolver.Resolve`, `CheckLiterals`, secret-path detection via `writeOnly` | `connections/connections.go` | matches |
| `Manager.Prepare` rejects duplicate ids, missing id, kind mismatch; builds and `Test`s; `Commit`, `Lookup`, `Close` | `connhost/manager.go` | matches (see item 4 on error classification) |
| Providers `env` (`NURTUO_SECRET_<NAME>`), `dir` (0700/0600), `Redactor` | `secrets/*.go` | matches; there is **no** chained/multi provider (item 2) |
| `ConnectorConfig` has no path field | `sdk/go/flow/flow.go:94` | matches |
| SFTP settings incl. `knownHosts` content, `hostKeyPolicy` values, default `known_hosts` | `connectors/sftp/config.go` | matches |
| No `nurtuoctl run`, no SFTP example, no `examples/connections/` | `cli/src`, `examples/` | matches (searched `cli/src`, `examples/`) |
| Server pattern in `connectors/sftp/helper_test.go` (`startServer`) | actual location is `connectors/sftp/sftp_test.go:31-84` (`helper_test.go` only holds `dialConnector`) | **path wrong** (Warning, item 6) |
| E2E dir `core/runtime/test/e2e` | only `core/runtime/test/bench` exists (`internal/e2e` is separate) | assumption, new dir (item 5) |

## Findings

- **Warning 1: stdout/exit mapping depends on `Result` semantics the spec compresses.** Evidence: spec "Output format"; `Result.Failed`, `Result.Stop`, `ctx.Err()` returned with a partial Result. Fix: during implementation, map in this order: usage/validation -> 2; `Run` error or ctx cancel -> 1; `Failed.Len() > 0` -> 1; `Stop != nil` with `Status != 0` -> 3; Stop with Status 0 -> 0 with empty output (spec silent; recorded as an [ASSUMPTION] changelog line).
- **Warning 2: env + `--secrets-dir` ("env first, then dir") needs a small chained `secrets.Provider`.** Evidence: `secrets/open.go` only opens one provider. Fix: add a ~15 line chained provider in `cmd/nurtuo/flowrun.go` (try env, on `ErrNotFound` try dir); do not change the `secrets` package.
- **Warning 3: `start` needs no runner.** Evidence: `engine/interp.go:242-246` passes `start` through when none is registered, whereas `listener.Register` would fail an http listener start without `path`. Fix: do not register `listener.StartRunner`; either register nothing or a no-op runner (Decision 11).
- **Warning 4: `Manager.Prepare` errors are unclassified (exit 2 vs 1).** Evidence: `Prepare` returns plain errors for missing id/kind mismatch (spec: exit 2), literal secret (spec: exit 2), missing secret and failed `Test` (exit 1). Fix: do the id/kind cross-check and the literal check in the command before `Prepare` (and after `Describe` for secret paths, or by checking `Resolve` errors), so those exit 2 before any network access; everything from `Prepare` after that is exit 1. Duplicate `--connection` ids are checked by the command.
- **Warning 5: e2e directory does not exist and `dispatch` is in `package main`.** Evidence: `core/runtime/test/` holds only `bench`. Fix: `test/e2e` builds the `nurtuo` binary with `go build` in `t.TempDir()` and runs it via `os/exec` (also checks real exit codes). New files carry the AGPL SPDX header (`check-boundaries.sh`).
- **Warning 6: spec cites the wrong helper file.** Evidence: see table. Fix: copy `startServer`/`serveConn` from `connectors/sftp/sftp_test.go` (it is a `_test.go` of another module; spec already says copy). Implementation PR corrects the path in the spec changelog.
- **Warning 7: test dependencies.** `github.com/pkg/sftp` and `golang.org/x/crypto` are `// indirect` in `core/runtime/go.mod`; a test that imports them makes them direct. Fix: `go mod tidy` in `core/runtime`; verify `go-licenses` (AGPL allowlist) still passes (both already in the module graph).
- **Warning 8: no local Docker.** `docker` is not installed on this machine, so `TestFlowRunSFTPContainer` can only be verified as "skips when unset"; the CI job is the real verification and cannot run here. Fix: report this honestly in the PR; the in-process SSH tests carry the functional coverage locally.
- **Warning 9: `known_hosts` for the default `--known-hosts` with non-22 ports.** Evidence: SFTP spec gap 4, `connectors/sftp/config.go` uses `x/crypto/ssh/knownhosts`. The e2e host uses a mapped port, so the generated `known_hosts` line must be `[127.0.0.1]:<port> ssh-ed25519 ...`. Fix: generate it in that form.
- **Important (gap): the spec's `Prepared` Test uses the 15 s `DefaultTestTimeout`.** Fix: keep the default; `--timeout` bounds the whole run through the context.
- **Nice-to-have: `nurtuo run` usage hint** (Gap 1) is in scope only as "usage error hints `nurtuo flow run`"; keep it to the message text, exit code 2 unchanged.

## Contract and compatibility audit

| Surface | Phase | Verdict |
|---|---|---|
| `nurtuo.flow/v1`, `nurtuo.connection/v1` | all | unchanged; connection files are never modified (settings are patched in memory) |
| SFTP connector / `connector.yaml` | all | unchanged |
| `nurtuo run` | Phase 1 | only the usage text may gain a hint; exit code 2 and flags unchanged |
| CLI (`nurtuo flow run`) | all | new, marked experimental; exit-code table documented in `docs/cli.md` |
| `docs/flow-reference.md` generated file | Phase 3 | the new example flow must validate (`gen-flow-reference.mjs --validate --check`) |

No break without a migration path.

## Risks

| Risk | Level | Mitigation |
|---|---|---|
| Hostile flow with real credentials runs mutating ops | High (spec-accepted, documented) | spec Gap 5 documents it; cli.md warns; `--read-only` is a recorded follow-up |
| Container e2e flakes or cannot be run locally | Medium | gate on `NURTUO_TEST_SFTP_E2E=1` + `docker info`, pinned image, `docker rm -f` cleanup |
| Large stdout / closed pipe | Medium | `io.Copy`, treat `EPIPE` as exit 1 without a stack |
| Secrets leaking via errors/logs | Medium | `Redactor` wraps stderr output; tests `TestFlowRunRedaction`, wrong-password test |

## During implementation

1. Add the exit-code mapping from Warning 1; record the Stop-without-status choice in the spec changelog.
2. Chained env-then-dir provider local to the command (Warning 2).
3. Do not register `listener.StartRunner`; start is a pass-through (Warning 3).
4. Classify validation errors in the command before `Prepare` (Warning 4).
5. Build the binary in `test/e2e` and run it via `os/exec` (Warning 5); gate by env + `docker info`.
6. Copy the SSH test server from `connectors/sftp/sftp_test.go`; fix the path in the spec changelog (Warning 6).
7. `go mod tidy` in `core/runtime`; re-run `check-boundaries.sh` (Warning 7).
8. CI: new job (or step) in `.github/workflows/ci.yml` that sets `NURTUO_TEST_SFTP_E2E=1`; do not touch other jobs, the CLA allowlist or repo settings.
9. State in the PR that the container e2e could not run locally (Warning 8).
10. Keep a `known_hosts` line in `[host]:port` form for the mapped port (Warning 9).
11. Docs: `docs/cli.md` and `docs/getting-started.md` with the `--set path=/downloads/version.txt` example; `connectors/sftp.md` section; run `make docs`.

## Next action

Implement with implement-spec, phases 1 to 4, in order, including the items above.

Readiness: conditional
Report: docs/specs/analysis/pre-implement-nurtuoctl-run.md
Spec: docs/specs/2026-11-23-nurtuoctl-run.md
