<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Verification skill and feature map

Status: draft. Date: 2026-11-26.

## TLDR

Add a project-local agent skill, `verify-nurtuo`, that launches an isolated Nurtuo stack (standalone runtime, control plane, canvas), checks it with a read-only doctor, drives it through the canvas, `nurtuoctl` and the control-plane API, and stores evidence that survives cleanup. A feature map, with one file per user-facing feature, says how to prove each feature end to end. Recenzent uses it to check live behavior in review, not only the diff. The skill follows pstack `create-verification-skill` and is later kept up to date with `maintain-verification-skill`.

## Status and links

| | |
|---|---|
| Pull requests | this PR (spec only) |
| ADRs | none (see Open questions Q1, Q2) |
| Code (planned) | `.agents/skills/verify-nurtuo/` (copied to `.claude/skills/` and `.cursor/skills/`), `core/web/e2e-live/` |
| Method | pstack `create-verification-skill` (`SKILL.md` and `references/feature-map-example/`) |

## Problem and goals

Today nothing drives a live backend. All four Playwright specs in `core/web/e2e/` run against a bare Vite server and fake the control plane with `page.route`. `cli/src/e2e.test.ts` uses `startFakeControlPlane`. `TestStandaloneE2EHandshakeAndDeploy` (`core/runtime/cmd/nurtuo/standalone_e2e_test.go`) is the only full-stack test, and it has no UI or CLI. A reviewer therefore cannot see that a change works for a user.

- Goals: one documented way for an agent to launch an isolated stack, decide whether it can be driven, run a feature journey on each surface, capture proof and tear the stack down. Also a feature map that Recenzent can match to a PR's changed paths.
- Non-goals: replacing unit, e2e or interop tests; a new required CI check; changing any product behavior, CLI flag, API or config key; production or remote environments.

## Surfaces

| Surface | Entry | Harness |
|---|---|---|
| Canvas (`core/web`) | single page `/`, Vite dev server, proxies `/v1` to `NURTUO_CP_URL` (`core/web/vite.config.ts`) | Playwright (chromium), reusing `@nurtuo/web` Playwright install |
| `nurtuoctl` (`cli/`, bin `dist/index.js`) | `--server`/`--token`, `NURTUO_SERVER`/`NURTUO_TOKEN`, `NURTUO_CONFIG`; `--json`; exit codes 0/1/2/3/4 | PTY transcript via `script(1)`, plus plain pipes for `--json` assertions |
| Control-plane API (`core/control-plane/openapi.yaml`) | HTTP on `PORT` (standalone `--port`) | `curl` with a cookie jar and `X-Nurtuo-Csrf`, or a Bearer `nrt_...` token |
| Runtime (`nurtuo`, `core/runtime/cmd/nurtuo`) | `nurtuo standalone`, `nurtuo flow run`, `nurtuo version` | PTY transcript; observed through the API (`/v1/runtimes`, deployment status) |

MCP (`core/mcp`, `nurtuo-mcp --transport http`) is a later surface (phase 4).

## Design

### Launch

`make dev` cannot be used as is: it starts only the canvas through turbo (only `core/web` has a `dev` script), and `nurtuo standalone` with the default data dir (`~/.local/share/nurtuo`) and port 8080. Standalone also needs `core/control-plane/dist/main.js`, which exists only after a build. The skill ships `launch.sh`, which:

1. Runs `make bootstrap` once when `node_modules` is missing, then builds what the journeys need: `pnpm --filter @nurtuo/control-plane... --filter @nurtuo/cli... --filter @nurtuo/web... build` and `CGO_ENABLED=0 go build -o $RUN/bin/nurtuo ./core/runtime/cmd/nurtuo`.
2. Creates `RUN=${NURTUO_VERIFY_ROOT:-${TMPDIR:-/tmp}/nurtuo-verify}/<run-id>` with `data/`, `logs/`, `evidence/`, and `state.env`. `state.env` holds the run id, PIDs, ports, git HEAD and the dirty flag.
3. Picks free loopback ports for the control plane, the gateway and the canvas. It starts `nurtuo standalone --data-dir $RUN/data --port $CP --gateway-addr 127.0.0.1:$GW` in its own process group, then `vite --port $WEB --strictPort` with `NURTUO_CP_URL=http://127.0.0.1:$CP`. Embedded Postgres already uses a free port.
4. Is ready only when the stack is usable, not merely when a process answers. Standalone's own check (`GET /` < 500) is not enough. Ready requires that `POST /v1/auth/login` as `admin@nurtuo.local` (password in `$RUN/data/secrets/admin-token`) succeeds, that `GET /v1/runtimes` lists runtime `standalone` with `status: online`, and that `GET http://127.0.0.1:$WEB/v1/auth/me` returns 200 through the proxy. The script then writes a per-run `NURTUO_CONFIG` that `nurtuoctl login --token-stdin` fills with a token minted by `POST /v1/workspaces/default/tokens`.

Seed: standalone's bootstrap workspace `default`, owner `admin@nurtuo.local` and runtime `standalone`. Fixtures come from `examples/flows/*.flow.json` and `examples/connections/sftp.connection.json`. Auth stays on. `NURTUO_INSECURE_NO_AUTH=1` is never used, because the journeys must prove the real auth path.

Isolation: everything a run owns sits under `$RUN` and its ports. Two runs can work side by side. The skill refuses to drive any URL or PID that is not in the current `state.env`.

### Doctor

`doctor.sh <run-id>` is read-only apart from one session login. It checks that every PID in `state.env` is alive and owns its port (`ss -ltnp`), that git HEAD and the dirty flag still match launch, that `GET /v1/auth/me` with the run's session returns 200, that `standalone` is `online`, that the canvas serves `/` and proxies `/v1/auth/me`, and that `nurtuoctl component list --json` exits 0. It prints one line per check and exits non-zero on the first failure.

### Drive

- Canvas: journeys live in `core/web/e2e-live/*.spec.ts`. They use their own `playwright.config.ts` with `baseURL` from `NURTUO_VERIFY_WEB_URL`, no `webServer` and no `page.route`. Selectors follow the existing specs: roles and accessible names (`Sign in`, `Workspace`, `Connections`, `Save connection`, `Configure <stepId>`, `Diagnostics`). React Flow nodes keep `.react-flow__node[data-id=...]`, because the repo has no `data-testid`. Run them with `pnpm --filter @nurtuo/web exec playwright test -c e2e-live/playwright.config.ts --output $RUN/evidence/<feature>/pw`.
- CLI: `verify.sh cli <run-id> -- nurtuoctl ...` runs the command under `script -qec` with the run's `NURTUO_CONFIG`. It records the command, the transcript, stdout, stderr and the exit code.
- API: `verify.sh api <run-id> <METHOD> <path> [body]` sends the run's session cookie and CSRF header and records the request and the response status and body.

### Evidence and proof standards

Evidence lives in `$RUN/evidence/<feature-id>/`, outside `data/`, and cleanup keeps it. Each artifact records the feature id, the entry point, git HEAD and a timestamp. The admin password, session cookies, tokens and connection secrets are redacted before writing.

- Drive the real user path: no internal setters, test-only endpoints, mocks or `NURTUO_INSECURE_NO_AUTH`.
- Capture the action and the resulting state. For the canvas that means a Playwright trace, an ARIA snapshot and a screenshot. For the CLI it means the transcript and the exit code.
- Confirm every mutation from a second, read-only view: CLI after UI, API after CLI.
- Report an unreachable entry point as skipped, with the command and the unmet precondition. Never report it as verified through another path.

### Cleanup

`cleanup.sh <run-id>` kills only the process groups recorded in `state.env` (never by name), removes containers labelled with the run id, deletes `data/`, `bin/` and `logs/` except copies under `evidence/`, and checks that the ports are free. A cleanup that deletes `evidence/` fails the skill's own proof.

### Feature map (seed)

`features/README.md` holds the baseline, the conventions, a path-to-feature table for Recenzent, and the index. Each feature file uses the four H2s of the pstack example.

| ID | Feature | Entry points | End-to-end proof |
|---|---|---|---|
| F1 | Sign in and session | canvas `Email`/`Password`/`Sign in`, `Workspace` select, `Log out`; `POST /v1/auth/login`, `GET /v1/auth/me` | the `Workspace` select shows `default`; a reload restores the session; after `Log out`, `/v1/auth/me` returns 401 and a mutating call without `X-Nurtuo-Csrf` is rejected |
| F2 | Edit, validate and save a flow on the canvas | palette drag, connect, `Configure <stepId>`, Undo/Redo, `Diagnostics (N errors, M warnings)`, `Save flow`, `Load JSON` | diagnostics name the broken step; the saved file passes `nurtuoctl validate --json` (`valid: true`); `Load JSON` of the saved file gives the same nodes and edges |
| F3 | Local flow tooling | `nurtuoctl validate`, `fmt --check`, `diff --exit-code`, `init -t http-get` | exit codes and `--json` diagnostics on a valid and a broken copy of `examples/flows/get-employee.flow.json` |
| F4 | Push and pull revisions | `nurtuoctl flow push --create`, `component revisions`, `flow pull`; `POST /v1/components/{id}/revisions` | revision number and `sha256:` digest listed by the CLI and by `GET /v1/components/{id}/revisions`; the pulled file equals the canonical pushed file |
| F5 | Workspace SFTP connections | canvas `Connections` dialog (`Connection id`, `Host key`, `Save connection`, `Delete`); `PUT/GET/DELETE /v1/workspaces/default/connections/{id}` | after save, `GET` returns the settings and `secrets: {password: "set"}`, never the value; after `Delete`, `GET` returns 404 |
| F6 | Deploy to the standalone runtime | `nurtuoctl deploy <c> --runtime standalone --wait`, `deploy status`; `POST /v1/deployments` | status and `GET /v1/runtimes/standalone/desired-state` show the revision. Today the runtime reports `error` ("no bundle_dir configured", `docs/operations.md`) and the deployment never reaches `running`; that needs #64 phase 4 ([#109](https://github.com/nurtuo/nurtuo/pull/109)) |
| F7 | Run a flow locally | `nurtuo flow run <flow> --connection <file> --json`; canvas export, as in `core/web/e2e/flow-run-export.spec.ts` | the output equals a known file served by a run-labelled `atmoz/sftp` container at the digest pinned in `.github/workflows/ci.yml`, with the host key checked through `known_hosts` |

Later candidates: MCP `validate`/`apply_ops`/`deploy`, and canvas Run when the [canvas Run spec](2026-11-23-canvas-run.md) lands.

### How Recenzent uses it

1. Map the PR's changed paths to features with the README table (for example `core/web/src/ui/ConnectionsPanel.tsx` maps to F5, `cli/src/**` to F3, F4 and F6).
2. Launch the PR head, run doctor, drive the matched features and run cleanup. Doctor failing on the PR head while passing on `main` is itself a finding.
3. A failed journey is rerun on `main`. If it fails only on the PR, it is a blocker (regression) or a major (new feature not working). If it fails on both, it is reported as pre-existing and does not block.
4. The review comment lists each feature id with its result (verified, failed or skipped with a reason) and the evidence paths. Recenzent posts text summaries only, redacted, and no secrets.
5. A PR that adds or changes a user-facing feature without a matching map entry gets a minor finding, and the map is updated through `maintain-verification-skill`.

## Contracts

None changed. All new files are internal: the skill, its helpers and `core/web/e2e-live/`. Env vars that only the skill reads (`NURTUO_VERIFY_ROOT`, `NURTUO_VERIFY_WEB_URL`) are internal.

## Security

The stack binds only to loopback. Each run has its own data dir and secrets, so the developer's `~/.local/share/nurtuo` is never touched. Real auth is always on. Evidence redaction covers the admin password, cookies, `nrt_` tokens, CSRF tokens and connection secrets. The SFTP password is generated per run, as in CI.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| skill self-proof | launch, doctor, F1, cleanup, then evidence still present | `launch.sh` / `doctor.sh` / F1 journey / `cleanup.sh` | output attached to each implementation PR |
| `scripts/check-agent-skills-sync.sh` | `.claude/skills` and `.cursor/skills` equal `.agents/skills` | as is | already in CI |
| `shellcheck` on helpers | script errors | in the `licenses`/lint job | new step |
| optional `verify-smoke` job | the self-proof on Linux CI | non-required job | Q5 |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The first `nurtuo standalone` downloads embedded Postgres binaries; an offline sandbox times out at launch | medium | launch | doctor reports it as a failed precondition; cache dir under `NURTUO_VERIFY_ROOT` reused across runs |
| 2 | F6 can never reach `running` until #109 merges, so Recenzent might mark every deploy PR as failing | medium | F6 | Gotcha records the expected outcome; until #109 the journey asserts a non-`running` status with the `no bundle_dir configured` message, then `running` |
| 3 | The canvas has no `data-testid`; a CSS refactor of `.react-flow__node` breaks F2 journeys without a behavior change | low | F2 | prefer roles; flag the selectors in Gotchas; adding test ids is a separate small fix |
| 4 | A run killed mid-way leaves Postgres and ports bound | low | cleanup | `cleanup.sh` works from `state.env` alone and is safe to run twice |

## Phased plan

1. PR1: `.agents/skills/verify-nurtuo/SKILL.md`, `launch.sh`, `doctor.sh`, `verify.sh`, `cleanup.sh`, `features/README.md`, F1 (`sign-in.md` plus `core/web/e2e-live/sign-in.spec.ts`) proven once, the skill copies, and the self-proof output in the PR. The spec moves to `in progress`.
2. PR2: F3 and F4 (CLI and API only).
3. PR3: F2 and F5 (canvas).
4. PR4: F6 and F7 (container and runtime), then MCP. The spec moves to `implemented/`.

## Open questions (for Łukasz)

- Q1, skill location: `AGENTS.md` makes `.agents/skills/` the source and `.cursor/skills/` a byte-identical copy, so a skill only in `.cursor/skills/verify-nurtuo/` fails `check-agent-skills-sync.sh`. Recommendation: source in `.agents/skills/verify-nurtuo/`, copied by `--fix`, and update `AGENTS.md` (Skills table) and `NOTICE.md` (which today says every directory is imported from mattpocock/skills).
- Q2, license: `.agents/skills/` holds MIT third-party code, and the license map in `AGENTS.md` does not cover this directory. Recommendation: Apache-2.0 with SPDX headers on the skill and helpers (it imports nothing from `core/`), listed as first-party in `NOTICE.md`. `core/web/e2e-live/` stays AGPL like the rest of `core/web`.
- Q3, the first proven feature in PR1: Recommendation: F1 sign-in. It is the smallest journey that crosses the canvas, the proxy, the control plane, Postgres and real auth, and it replaces the mocked `session.spec.ts` path with a live one.
- Q4, the CLI harness: `node-pty` would be a new dependency, which needs your approval. Recommendation: `script(1)` from util-linux, with no new dependency. `nurtuoctl` is non-interactive apart from `login --token-stdin`.
- Q5, CI: Recommendation: add a non-required `verify-smoke` job that runs the self-proof on PRs touching `.agents/skills/verify-nurtuo/**` or `core/web/e2e-live/**`. Adding a job needs no owner approval, but the extra 5-10 minutes of CI time is your call.
- Q6, Recenzent severity: Recommendation: as in "How Recenzent uses it" step 3 (regression = blocker, new feature not working = major, pre-existing = report only). Live verification is required for PRs that touch a mapped path and optional for docs-only PRs.
- Q7, where evidence goes for the review: Recommendation: keep artifacts local in `$RUN/evidence/` and post only the redacted text summary in the review comment. No uploads to GitHub or other external services for now.
