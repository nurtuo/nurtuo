<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Verification skill and feature map

Status: draft. Date: 2026-11-26.

## TLDR

Add a project-local agent skill, `verify-nurtuo`, that launches an isolated Nurtuo stack (standalone runtime, control plane, canvas), checks it with a read-only doctor, drives it through the canvas, `nurtuoctl` and the control-plane API, and stores evidence that survives cleanup. A feature map, with one file per user-facing feature, says how to prove each feature end to end. Recenzent uses it to check live behavior in review, not only the diff. The skill follows pstack `create-verification-skill` and is later kept up to date with `maintain-verification-skill`.

## Status and links

| | |
|---|---|
| Pull requests | this PR (spec only) |
| Issues | none assigned |
| User docs | internal reviewer skill |
| ADRs | none (see Open questions Q1, Q2) |
| Code (planned) | `.agents/skills/verify-nurtuo/` (copied to `.claude/skills/` and `.cursor/skills/`), `core/web/e2e-live/` |
| Method | pstack `create-verification-skill` (`SKILL.md` and `references/feature-map-example/`) |

## Problem and goals

The missing proof is a live canvas-to-control-plane session. Two existing browser specs intercept control-plane calls and two exercise client-side canvas paths. The export journey also runs a real runtime against pinned SFTP in CI. CLI e2e uses a fake control plane, and the standalone handshake/deploy test covers the backend (with a database substitute available) without the browser or CLI.

- Goals: one documented way for an agent to launch an isolated stack, decide whether it can be driven, run a feature journey on each surface, capture proof and tear the stack down. Also a feature map that Recenzent can match to a PR's changed paths.
- Non-goals: replacing unit, e2e or interop tests; a new required CI check; changing any product behavior, CLI flag, API or config key; production or remote environments.

## Surfaces

| Surface | Entry | Harness |
|---|---|---|
| Canvas (`core/web`) | single page `/`, Vite dev server, proxies `/v1` to `NURTUO_CP_URL` (`core/web/vite.config.ts`) | Playwright (chromium), reusing `@nurtuo/web` Playwright install |
| `nurtuoctl` (`cli/`, bin `dist/index.js`) | `--server`/`--token`, `NURTUO_SERVER`/`NURTUO_TOKEN`, `NURTUO_CONFIG`; `--json`; exit codes 0/1/2/3/4 | plain pipes with separate streams and exit status |
| Control-plane API (`core/control-plane/openapi.yaml`) | HTTP on `PORT` (standalone `--port`) | `curl` with a cookie jar and `X-Nurtuo-Csrf`, or a Bearer `nrt_...` token |
| Runtime (`nurtuo`, `core/runtime/cmd/nurtuo`) | `nurtuo standalone`, `nurtuo flow run`, `nurtuo version` | plain pipes; observed through the API (`/v1/runtimes`, deployment status) |

MCP (`core/mcp`, `nurtuo-mcp --transport http`) is a later surface (phase 4).

## Design

### Launch

`make dev` cannot be used as is: it starts only the canvas through turbo (only `core/web` has a `dev` script), and `nurtuo standalone` with the default data dir (`~/.local/share/nurtuo`) and port 8080. Standalone also needs `core/control-plane/dist/main.js`, which exists only after a build. The skill ships `launch.sh`, which:

1. On Linux create a unique owner-only `$RUN` under `NURTUO_VERIFY_ROOT` (default `$TMPDIR/nurtuo-verify`, with /tmp as fallback). Use umask 077. Pin the harness and product commit SHAs independently. For each product run create a detached clean worktree at `$RUN/src`, install with the frozen lockfile, build the control-plane, CLI and web package closures there and build the Go executable into `$RUN/bin`. Use that worktree's CLI, Vite root and absolute `--control-plane-entry "$RUN/src/core/control-plane/dist/main.js"`. Stage harness journeys from the same pinned revision for both products, with private fixture copies, dependencies and output directories.
2. Build and drive from an allowlisted environment with a private HOME, tool paths and explicitly selected build caches. Construct each child's settings from validated run state. Drop inherited Nurtuo variables, proxy, curl, Node and browser overrides, including `NURTUO_INSECURE_NO_AUTH`, `NURTUO_TRUST_PROXY` (even empty), workspace/admin overrides, server, data dir and token. Disable curl startup files and redirects. Drivers accept structured supported operations, fixed relative API routes and validated operands. Reject caller-supplied global flags (including equals forms, aliases and config, credential or destination overrides), absolute or authority-relative URLs and unexpected arguments before any request. Browser requests stay on recorded origins.
3. Gate startup on a product revision with verified explicit loopback binding (Q8). Today's standalone drops the host from `--gateway-addr` when configuring the child and both Node listeners omit the host. Until the prerequisite lands, skip live runs before spawning standalone. Supply numeric loopback addresses to both supported listeners, start standalone with its private data directory and start Vite from `core/web` with `--host 127.0.0.1 --port $WEB --strictPort` and run-local `NURTUO_CP_URL`. Allocate distinct ports and fail on a bind race. Before sending credentials inspect all actual CP, gateway, DB and web listeners with `ss -ltnp`; require loopback-only addresses and the ownership checks below. Stop on a wildcard, unexpected owner or unverifiable listener.
4. Journal run id, canonical paths, SHAs, ports, supervisor PID/start time and PGID for each dedicated group atomically as resources are acquired. Record CP child identity and the detached postmaster identity from `$RUN/data/postgres/data/postmaster.pid`, including Linux process start times. Parse state as data. Recover partial acquisitions only after checking ownership against the private run directory.
5. Require anonymous `GET /v1/auth/me` to return 401 directly and via Vite. Authenticate as `admin@nurtuo.local` using the private bootstrap password in `data/secrets/admin-token`. Require authenticated proxied `/v1/auth/me` to return 200 and `/v1/runtimes` to list standalone online. Use the setup session and CSRF header to mint a named workspace token with run-bounded `expiresInSeconds`. Grant only scopes used by selected operations (component:read for doctor, component:write for push, runtime:read and deployment:read/create for deploy, spelled `deployment:read` and `deployment:create`). Local tooling needs no token. Record its id privately. The harness itself runs `nurtuoctl login --server "$RECORDED_CP_URL" --token-stdin` through a private pipe with `NURTUO_CONFIG=$RUN/private/cli.json`. Validate the stored destination before each CLI drive.

### Doctor

Doctor reuses the private setup session for read-only checks. Validate the scrubbed child environment, clean product and harness identities, and supervisor start times. Resolve socket PIDs to the live descendants in their recorded groups (Node owns the CP and gateway, a Vite child may own web). Match Postgres separately by PID, start time and exact data directory. Require the original CP child throughout each journey, checking again before accepting evidence. A supervised restart loses in-memory connection secrets and invalidates the run even if health recovers. Repeat the launch address, session/proxy and runtime checks, serve canvas /, and require CLI component list --json to exit 0. Report the failed stage and stop on failure.

### Drive

- Canvas journeys in `core/web/e2e-live/` use Chromium, a dedicated Playwright config and tsconfig, the recorded Vite base URL, existing accessible names and React Flow node ids. Start the browser from the scrubbed environment. Drive the real UI without route mocks. Give F1 its own browser session so logout leaves the setup session available.
- CLI and runtime commands execute once on plain pipes. Preserve separate stdout, stderr and exit status subject to the evidence rules. Login stdin is private. Never replay a mutation for a transcript.
- API steps use a private cookie jar plus CSRF header or a scoped bearer. Confirm mutations from a second read-only view where available. Report unmet prerequisites as skipped.

### Evidence and proof standards

Keep jars, CLI config, logs and browser profile in private disposable storage. Disable Playwright tracing, video, automatic screenshots, request dumps and failure attachments throughout live runs. Use an allowlist per step. Login, token operations and connection writes record only operation name, status and approved assertions. Never serialize raw headers, bodies, argv or streams. Other steps may retain selected safe fields from separate streams, safe ARIA text and masked screenshots only of approved post-action views after sensitive fields disappear. Exclude generic runner reports and stacks that echo inputs.

Stage artifacts privately. On both success and failure check every candidate's type and fields, reject archives (including trace zips), and scan textual content for actual run secrets and canaries, including the admin/signing password, session, CSRF, API and SFTP credentials. Test image masking with a visible canary and reject unapproved images. Publish only passing artifacts to `$RUN/evidence/<feature-id>/` with feature, entry, SHAs, timestamp and assertion. On scan failure discard the batch and report only a safe failure stage.

### Cleanup

While the original CP is healthy revoke the setup token by id with the setup session and verify denial of its bearer. Send SIGINT to the revalidated standalone group and wait for graceful shutdown. Postgres starts its own session. If still running, verify start time and exact run data path, then use `$RUN/data/postgres/runtime/bin/pg_ctl stop -m fast -D "$RUN/data/postgres/data"`. Escalate only against individually revalidated survivors. Recheck identity, group membership and run paths before signaling, including Vite. Refuse ambiguous or recycled IDs. Recover an unjournaled postmaster from its private pidfile only with corroborating process and directory evidence. Remove only containers with the recorded id and run label, remove the registered worktree, delete private material, and verify owned listeners are gone. Keep sanitized evidence. Surface incomplete cleanup and preserve private recovery state for retry.

### Feature map (seed)

`features/README.md` holds the baseline, the conventions, a path-to-feature table for Recenzent, and the index. Each feature file uses the four H2s of the pstack example.

| ID | Feature | Entry points | End-to-end proof |
|---|---|---|---|
| F1 | Sign in and session | canvas `Email`/`Password`/`Sign in`, `Workspace` select, `Log out`; `POST /v1/auth/login`, `GET /v1/auth/me` | the `Workspace` select shows `default`; a reload restores the session; while authenticated send a valid component creation without `X-Nurtuo-Csrf`, require 403 with `error.code == "csrf"` and verify the component is absent; then click `Log out` and require 401 from `/v1/auth/me` when replaying that session’s former cookie |
| F2 | Edit, validate and save a flow on the canvas | palette drag, connect, `Configure <stepId>`, Undo/Redo, `Diagnostics (N errors, M warnings)`, `Save flow`, `Load JSON` | diagnostics name the broken step; the saved file passes `nurtuoctl validate --json` (`valid: true`); `Load JSON` of the saved file gives the same nodes and edges |
| F3 | Local flow tooling | `nurtuoctl validate`, `fmt --check`, `diff --exit-code`, `init -t http-get` | exit codes and `--json` diagnostics on a valid and a broken copy of `examples/flows/get-employee.flow.json` |
| F4 | Push and pull revisions | `nurtuoctl flow push --create`, `component revisions`, `flow pull`; `POST /v1/components/{id}/revisions` | revision number and `sha256:` digest listed by the CLI and by `GET /v1/components/{id}/revisions`; the pulled file equals the canonical pushed file |
| F5 | Workspace SFTP connections | canvas `Connections` dialog (`Connection id`, `Host key`, `Save connection`, `Delete`); `PUT/GET/DELETE /v1/workspaces/default/connections/{id}` | after save, `GET` returns the settings and `secrets: {password: "set"}`, never the value; after `Delete`, `GET` returns 404 |
| F6 | Deploy to the standalone runtime | `nurtuoctl deploy <c> --runtime standalone --wait`, `deploy status`; `POST /v1/deployments` | match the deployment ID and intended revision against status and runtime desired state. For the bundle-less fixture require terminal API status `failed`, message prefix `no bundle_dir configured` and CLI exit 1. Timeout fails separately. Add a `running` / exit 0 assertion only for a fixture supplying an executable bundle after [#109](https://github.com/nurtuo/nurtuo/pull/109) |
| F7 | Run a flow locally | `nurtuo flow run <flow> --connection <file> --json`; canvas export, as in `core/web/e2e/flow-run-export.spec.ts` | the output equals a known file served by a run-labelled `atmoz/sftp` container at the digest pinned in `.github/workflows/ci.yml`, with the host key checked through `known_hosts` |

F2, F3 and the F7 export half need no control plane. For F7 reuse the CI fixture, generate private conn.json with the actual mapped loopback port, verified host key and a run-generated password passed via `NURTUO_SECRET_SFTP_PASSWORD`. Label the container `nurtuo-verify=<run-id>` and record its id. F5 loses secret values if the CP restarts.

### How Recenzent uses it

Route by the changed operation within a file. The index records an assertion or a reason for deferral/exclusion for each shipped command, route and surface. Shared paths select candidate journeys only.

| Changed behavior | Journey or disposition |
|---|---|
| CP auth and canvas session | F1 local sign-in, restoration, CSRF and logout. Defer OIDC/SAML pending IdP fixtures. |
| Canvas editing, diagnostics, import/export | F2 and F7 export. Defer canvas Run pending its [spec](2026-11-23-canvas-run.md). |
| CLI flowtools and SDK schema/canonicalization | F3 local diagnostics and F4 revision round trip where affected |
| CLI flow push/pull, CP revisions | F4. Defer standalone component get/create/delete and CLI completion until dedicated assertions exist. |
| CP connections and canvas ConnectionsPanel | F5 save, masked read and delete |
| Gateway, bundle, runtime desired state, CLI deploy/status | F6 terminal outcome. Defer CLI runtime list and CLI logout until their own output/state is checked. CLI login is exercised in setup. |
| Runtime flow execution, SFTP connector and canvas export | F7 known-file result. Other connectors/SDK operations need named fixtures before claiming coverage. |
| Workspace membership/authorization, workspace management, token list and audit | Deferred pending multi-user and audit assertions. Token creation/login and revocation/denial covered in setup/teardown only. |
| Runtime run --config, secret and validate-key | Deferred pending config, keystore and key fixtures. Host service installation excluded from this local stack. |
| MCP inventory, reads, validate/apply_ops and deploy | Deferred beyond F7 until a transport/client and permission fixtures exist. Deploy requires explicit opt-in and an isolated target. |

For a mapped assertion run doctor, drive, recheck health and clean up. Retry a failure on a fresh healthy head run, then use the identical harness and assertion on a separate clean worktree pinned to the recorded main SHA. Keep independent builds and fixtures. Classify unavailable infrastructure, an unhealthy run or intermittent results as skipped/inconclusive with stage and reason. A repeatable head failure with a passing supported base is a regression. Only matching assertion and failure cause on a supported base establish a pre-existing failure. If the base lacks the product operation, record unsupported-on-base and judge the new feature against its stated acceptance criteria. A missing base harness is immaterial. Report verified/failed/skipped by feature and flag a missing map entry as minor. Severity policy awaits Q6.

## Contracts

The skill reads internal run settings. Product listener configuration requires separate approval and delivery under Q8.

## Security

Run live journeys only on Linux with inspectable process and socket ownership. The current wildcard listeners make the existing product ineligible for local live launch. Apply the pre-credential gate, private environment and evidence allowlists above to every path, including failure reporting.

## Testing

- PR1 self-proof exercises launch, doctor, F1 and idempotent cleanup. Inject conflicting dummy server/token, insecure auth and proxy settings and attempted CLI overrides, including login destination. Assert a second local receiver gets zero requests and anonymous access still fails.
- Exercise wildcard rejection before login, interrupted partial startup, orphaned Postgres, CP restart, stale/reused identity and repeated cleanup while a sibling run remains healthy. Check retained evidence on successful and deliberately failing secret-bearing steps, including a forbidden trace fixture containing a compressed canary. Verify safe evidence survives teardown and private material is removed.
- Add the currently local skill-sync checker to CI in PR1, shellcheck helpers, check shell SPDX explicitly (boundaries omits shell) and typecheck live specs with their own config.
- Extend optional Linux web-e2e (Q5), reuse its pinned SFTP and Chromium setup including OS deps, enforce a bounded job timeout and run cleanup on failure/cancellation with runner teardown as fallback. Include runtime, web, CP, CLI, skill/live spec, shared SDK, dependency and workflow paths. Measure cold/warm elapsed time and runner-minutes including independent base install/build and rerun before setting the budget.

## Gaps and risks

- Offline Postgres provisioning (medium). Its cache is fixed at `data/postgres/cache`. Seed a private copy before launch from a version/OS/architecture-keyed archive cache under the verification root. After successful startup atomically publish completed cache entries. Cache those entries in CI. An offline cache miss is a skipped precondition.
- Interrupted shutdown (medium). Exercise the detached database and identity-checked recovery paths above. A stale PID alone never authorizes a signal.
- CSS selector drift (low). Prefer roles and document React Flow node selectors in F2.

## Phased plan

1. After approval and the listener prerequisite, PR1 delivers the skill source and mirrors, launch/doctor/drive/cleanup, indexed dispositions, F1 and self-proof. Move status to in progress.
2. PR2 adds F3 and F4.
3. PR3 adds F2 and F5.
4. PR4 adds F6 and F7. Move to implemented after proof. MCP needs a subsequent scoped phase.

## Improvement opportunities

Add successful bundle deployment and MCP coverage when their prerequisites exist.

## Open questions (for Łukasz)

| Decision | Recommendation |
|---|---|
| Q1. Where should the skill live? | Author in `.agents/skills/verify-nurtuo/`, sync both Claude and Cursor mirrors and update AGENTS.md and NOTICE for first-party provenance. |
| Q2. Which license? | Accept inherited MIT under the nearest LICENSE for the skill and helpers. Keep live tests under core’s AGPL. Choosing Apache-2.0 instead needs a local LICENSE and licensing documentation, with owner review before merge. |
| Q3. Which first journey? | Start with F1, including authenticated CSRF rejection and revoked-session replay. |
| Q4. How should the CLI run? | Use plain pipes once per command for separate streams and exit status. |
| Q5. Where should smoke run? | Extend optional web-e2e across product and harness paths. Measure cold, warm and baseline costs before setting a budget. |
| Q6. Should live findings affect merging? | Approve an explicit amendment to the review rules for mapped behavior. Grade a reproducible healthy-head regression as blocker, a demonstrated new-feature acceptance failure as major and a matching baseline failure as informational. Infrastructure and flakes are inconclusive. |
| Q7. Who can inspect evidence and for how long? | Keep sanitized local evidence seven days, then prune it. CI emits only a safe assertion summary, SHAs, feature, exit status and stage before teardown. Exclude live artifacts from upload. |
| Q8. How should listeners be confined? | Approve a separate product spec/PR for explicit loopback hosts on both servers. Gate live launch on that capability and verify actual sockets before login. |
| Q9. How should head and main be compared? | Pay for separate clean worktrees/builds and one pinned harness. Record unsupported base features separately and judge them on head against explicit acceptance criteria. |
| Q10. What may survive a secret-bearing step? | Keep only allowlisted assertion fields and approved masked post-action views. Disable raw traces and test evidence with real values and canaries on success and failure. |
