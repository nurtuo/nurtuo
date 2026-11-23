<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: canvas Run (test-run a flow from the canvas against a real connection)

Status: draft. Date: 2026-11-23.

Extends: [canvas-editor](implemented/2026-11-21-canvas-editor.md) (test mode, Epic 10),
[connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md),
[control-plane-api-and-data](implemented/2026-11-21-control-plane-api-and-data.md),
`docs/specs/2026-11-23-nurtuoctl-run.md` (`nurtuo flow run`; on branch `docs/spec-nurtuoctl-run`,
PR #50, not on `main` yet, so it is not linked).
Related: [workspace-isolation](2026-11-22-workspace-isolation.md) (draft).

> **Draft.** Every item marked **[DEFAULT, pending Łukasz]** is a reversible, lowest-blast-radius
> default chosen so the design can be written down. None of them is a decision. The
> [Decisions](#decisions-for-łukasz) table lists them with a recommendation; the spec cannot become
> `approved` until those rows are answered.

## TLDR

In the canvas the user builds `Start -> Connector (sftp, GET) -> Return Documents`, picks or creates
an SFTP connection (password kept as a workspace secret, referenced as `secret://<name>`, never in
the flow JSON), presses **Run**, and sees the returned document and a status colour on every step.
The control plane gets a workspace-scoped `POST /v1/workspaces/{ws}/test-runs` that runs the
unsaved flow through **the same engine as `nurtuo flow run` (PR #51)**, with no bundle, no deploy and
no runtime gateway (so the standalone handshake bug #56 is not on the path). The one thing to know:
the control plane is Node/TypeScript and the engine is Go `internal` code, so "in-process engine"
means the control plane starts the `nurtuo` binary (`nurtuo flow run --json`) as a short-lived child
process per run (Decision D7).

## Status and links

| | |
|---|---|
| Pull requests | this spec only; implementation PRs after approval. Depends on https://github.com/nurtuo/nurtuo/pull/51 (`nurtuo flow run`), https://github.com/nurtuo/nurtuo/pull/50 (its spec) and https://github.com/nurtuo/nurtuo/pull/53 (`web-e2e` job), none merged on `main` at the time of writing |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md) (connections and secrets), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) (per-document status), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (gateway; not used here) |
| Issues | https://github.com/nurtuo/nurtuo/issues/54, https://github.com/nurtuo/nurtuo/issues/55 (workspace isolation), https://github.com/nurtuo/nurtuo/issues/56 (standalone handshake, bypassed) |
| Code (existing) | `core/web/src/ui/{App,Canvas,ConfigDialog}.tsx`, `core/web/src/model/*.ts`, `core/control-plane/src/{api,main}.ts`, `core/control-plane/src/auth/{middleware,policy,permissions,endpoints}.ts`, `core/control-plane/openapi.yaml`, `core/runtime/internal/engine/interp.go`, `core/runtime/cmd/nurtuo/standalone.go`, `connectors/sftp/{config,sftp}.go`; on PR #51: `core/runtime/cmd/nurtuo/{flow,flowrun}.go`, `core/web/e2e/flow-run-export.spec.ts` |
| User docs | [concepts.md](../concepts.md), [connectors/sftp.md](../connectors/sftp.md), [auth.md](../auth.md), [security.md](../security.md) |

## Goal

Who: an integration developer working in the canvas. Today the canvas (`core/web`) is a local file
editor with no network calls; to see a flow work the user must "Save flow", write a connection file,
export a secret into the environment and run `nurtuo flow run` in a terminal (PR #51), or deploy to a
runtime, which in standalone does not work yet (#56). Proposed: a **Run** button that executes the
current canvas flow against a connection stored in the user's workspace and shows the output and a
per-step status in the canvas within seconds. Why: it closes the "test mode with per-step colours"
item of M1 Epic 10 ([ARCHITECTURE.md](../ARCHITECTURE.md) §10, canvas-editor gap 2) with the engine
that already exists, and it is the shortest path to "build a flow, see it work".

### Motivating example

1. Canvas: drag **Start**, **Connector**, **Return Documents**; connect them.
2. Connector config: `connector: sftp`, `operation: GET`, `connection: rebex-demo` (picked from a
   list).
3. Connections panel, "New SFTP connection" `rebex-demo`: host `test.rebex.net`, port 22, user and
   password from the public Rebex demo page (the password goes to the workspace secret
   `rebex-demo-password`; the connection stores `"password": "secret://rebex-demo-password"`), host
   key: "Fetch host key" shows the SHA-256 fingerprint, the user compares it with the one Rebex
   publishes and presses "Trust" (D4).
4. **Run**, input property `path=/readme.txt` (D15). The results panel shows `succeeded`, the text
   of the file, and `start-1`, `connector-1`, `returndocuments-1` (the ids the palette assigns) coloured green.

## Decisions for Łukasz

Each row is an open question. The recommendation is what the Design below assumes, marked
**[DEFAULT, pending Łukasz]**. "Blocking" means the spec cannot be approved without an answer.

| ID | Question | Options | Recommendation | Why (short) | Blocking |
|---|---|---|---|---|---|
| D1 | Sync or async test run? | (a) synchronous request/response; (b) job + poll; (c) job + WebSocket/SSE | **(a) synchronous** `POST .../test-runs` returning the full result, bounded by the D2 timeout; the response carries a `runId` so (b) can be added later without breaking clients | one SFTP GET takes seconds; no job table, no worker, no socket; fastest path to a result | yes |
| D2 | Timeouts and size limits? | numbers | **run timeout 30 s default, client may ask up to 120 s; request body ≤ 1 MiB (413); returned payload ≤ 1 MiB total decoded and ≤ 20 documents (else the run fails with `output_too_large`); 2 concurrent runs per workspace, 4 per instance (429)** | protects the Node process (it buffers bodies, control-plane gap 2) and keeps the response a single JSON object | no (numbers adjustable) |
| D3 | Where are secrets stored in M1, and how does the canvas refer to them? | (a) control-plane secret store scoped per workspace; (b) local/dev file (`nurtuo secret`, `dir`/`encrypted-file` under the data dir); (c) OS keychain; (d) runtime-side store only | **(a) workspace-scoped control-plane secret store behind a `SecretStore` interface, with an in-memory backend only in this slice** (values live in the control-plane process, never on disk, lost on restart; a persistent encrypted backend is a separate spec). The canvas and connections refer to secrets only as `secret://<name>`; the password is never in the flow JSON, the connection JSON, a response, a log or the audit log | (b) puts plaintext-equivalent files next to their key (connections-and-secrets gaps 1 and 2) and is per host, not per workspace; (c) is a stub (`KeychainKeySource`); (a)+memory is reversible and has no at-rest key management to get wrong | yes |
| D4 | How is the SFTP host key accepted in the UI? | (a) TOFU: server scans, user confirms the fingerprint; (b) paste fingerprint or `known_hosts` line; (c) upload a `known_hosts` file; (d) insecure toggle | **(a) + (b), never (d).** "Fetch host key" shows the SHA-256 fingerprint(s); nothing is stored until the user presses "Trust this key"; the trusted key is saved as `hostKeyPolicy: fingerprint`. Paste of a fingerprint or `known_hosts` content is always available. `hostKeyPolicy: insecure` is rejected by the API (400) and there is no UI for it. A later mismatch is a hard error, never a re-prompt that overwrites | no insecure default (brief); TOFU with explicit confirmation is what OpenSSH users know; `fingerprint` avoids SFTP gap 4 (known_hosts key-type mismatch) | yes |
| D5 | Is run history persisted? | (a) canvas memory only (this session); (b) stored per workspace with retention N days | **(a) not persisted.** The result lives in canvas memory and is cleared on any edit, reload or logout; the server keeps only the audit row (action `test_run.execute`, status, no payload) | output documents are user data that may be sensitive; storing them needs retention, RBAC and size policy (later spec) | no |
| D6 | Who may create connections, set secrets and press Run? | permission matrix | **new permissions `connection:read` (all roles that have `component:read`), `connection:write`, `connection:delete`, `secret:write`, `secret:delete`, `test_run:execute` (owner, admin, editor). `viewer` and `deployer` read connections only; `runtime` gets none. In this slice the secret and test-run routes are `userOnly` (no API tokens)**. Secret values are never readable by anyone (there is no `secret:read` of values; listing returns names only) | a run uses workspace credentials against external systems, so it is an editor action, like writing a component; `userOnly` can be relaxed later (additive) | yes |
| D7 | Execution path? | (a) control plane runs the engine via the `nurtuo` binary as a child (`nurtuo flow run --json`), no deploy; (b) a new long-running Go "test-run" service; (c) deploy to a runtime and trigger it via the gateway (`run_now`) | **(a) for this slice.** Same engine and connector registry as PR #51 (`engine.New`, `connhost.Builtins`, `connhost.Manager`), one process per run, no bundle, no gateway, so #56 does not block it. **Explicitly deferred:** test runs on a deployed runtime through the gateway (after #56), test runs of saved component revisions, async jobs, MCP `test_run` tool | the control plane is TypeScript and cannot link Go `internal` packages; (a) reuses an approved, tested entrypoint; process isolation means a hung or crashing run cannot take the control plane down | yes |
| D8 | Workspace isolation for this slice? | what is required now vs later | **Required now:** all new routes are path-scoped `/v1/workspaces/{ws}/...` (the existing membership check in `resolveWorkspace` already isolates them); new data (connections, secrets) is keyed by `workspace.id` from day one; a run resolves connections and secrets only from the URL's workspace; per-run temp dir and minimal child environment; a foreign id is 404. **Can wait:** #55 (`workspace_id` on components etc.) and #54 (gate workspace creation), because the run takes the flow inline and never reads a component. **Condition:** if D10 later allows running a saved component, #55 must land first | the new tables never exist without `workspace_id`, so they cannot leak even before #55; #54 lets anyone create a workspace, but a new workspace starts empty and sees only its own connections | yes |
| D9 | API shape and auth for the test-run endpoint; how the canvas talks to the control plane | (a) path-scoped routes + session cookie + CSRF, canvas same-origin through a proxy; (b) context routes (`X-Nurtuo-Workspace`) + bearer token typed into the canvas; (c) CORS | **(a).** Routes under `/v1/workspaces/{ws}/` implemented as auth-module endpoints that receive the resolved `workspace`. Canvas: a minimal login (`POST /v1/auth/login`), workspace picker (`GET /v1/workspaces`), `X-Nurtuo-Csrf` from the login response; dev and Playwright use a Vite `server.proxy` for `/v1` so the cookie is same-origin. **The new routes are not served when `NURTUO_INSECURE_NO_AUTH=1`** (404) | no CORS to get wrong, no token in browser storage; path scoping is the fail-closed form; an unauthenticated SSRF-capable endpoint is unacceptable even in dev | yes |
| D10 | How does the canvas ship the flow; can unsaved state run? | (a) inline flow JSON in the request (unsaved state); (b) component id + revision; (c) both | **(a) inline only.** The body carries the current `nurtuo.flow/v1` document from `FlowStore` (no layout); the server validates it again. No component id in this slice | the canvas has no save-to-control-plane yet (canvas-editor gap 2); (b) depends on #55 for isolation | yes |
| D11 | Egress and side effects of a server-side run (SSRF) | (a) any connector, any operation, any host; (b) `sftp` only, read-only operations, private/loopback/link-local addresses refused unless the operator allows them | **(b).** This slice allows only connector `sftp` with operations `GET` and `LIST`; other connectors or operations are 400 `not_allowed_in_test_run`. Hosts that resolve to loopback, private, link-local or metadata ranges are refused unless `NURTUO_TEST_RUN_ALLOW_PRIVATE=1` (CI sets it for the container) | a run makes the control-plane host open connections chosen by any editor; without limits it is a port scanner and an internal-network client (cloud metadata, internal Postgres) | yes |
| D12 | How are secret values handed to the child process? | (a) `NURTUO_SECRET_<NAME>` in a minimal, non-inherited environment; (b) a per-run temp `--secrets-dir` (0700/0600) deleted afterwards; (c) stdin | **(a)**, with the child environment built from scratch (no `DATABASE_URL`, `NURTUO_AUTH_SECRET`, bootstrap password etc.), `HOME` pointed at the per-run temp dir, and secret names whose `EnvVar` form collides in a workspace refused at write time | env stays in memory; (b) writes plaintext to disk briefly; (c) needs a new CLI input mode | no |
| D13 | Which SFTP server does the Playwright test use? | (a) `atmoz/sftp` container in CI (as PR #53); (b) `test.rebex.net`; (c) both | **(a) in CI** (pinned digest, random password generated per job, host key from `ssh-keyscan`); **(b) only as a manual smoke test gated by `NURTUO_E2E_REBEX=1`, never in CI** | CI must not depend on a third-party server's availability or contents; no password committed in either case | no |
| D14 | Split into more than one spec? | (a) one spec, phased; (b) split: connections/secrets API, test-run endpoint, canvas UI, CI e2e | **(a) one spec with six independently shippable phases**; the persistent secret backend (D3), runtime-deployed test runs (D7) and run history (D5) are separate future specs | the four parts are one user story and only useful together; phases keep each PR small | no |
| D15 | How does the user give the SFTP path? (`ConnectorConfig` has no `path`) | (a) "Input properties" in the Run dialog, sent as the initial document's properties (same as `--set`); (b) require a `setProperties` step; (c) add `path` to the flow format | **(a)**, pre-filled with `path=` when the flow has an SFTP `GET` step; (b) keeps working | no flow-format change (`nurtuo.flow/v1` is ADDITIVE-ONLY with `additionalProperties: false`); mirrors the CLI | no |
| D16 | Per-step status from the engine: extend `nurtuo flow run --json`? | (a) add an additive `steps` array (from `engine.Result.Trace`) to the `--json` envelope and to the JSON error object; (b) a separate flag `--trace`; (c) a new Go binary for the control plane | **(a)**, as a small follow-up to PR #51's experimental contract | `Result.Trace` already exists and is not emitted; additive JSON keeps CLI users unaffected | yes (touches an approved spec's contract) |

## Open Questions

All are answered by the Decisions table; numbering matches.

- **Q1 (blocking):** synchronous run response, or async job? (D1)
- **Q2 (non-blocking):** are 30 s / 120 s, 1 MiB in and out, 20 documents, 2/4 concurrent runs acceptable? (D2)
- **Q3 (blocking):** workspace-scoped in-memory secret store for this slice, persistent store later? (D3)
- **Q4 (blocking):** TOFU with explicit confirmation plus paste, stored as `fingerprint`, no insecure option? (D4)
- **Q5 (non-blocking):** no run history beyond the audit row? (D5)
- **Q6 (blocking):** the permission set and role grants of D6, and `userOnly` for secrets and runs? (D6)
- **Q7 (blocking):** control plane spawns `nurtuo flow run` per run (not literally in the Node process)? (D7)
- **Q8 (blocking):** is "path-scoped routes + workspace-keyed new data, #54/#55 not required" acceptable? (D8)
- **Q9 (blocking):** canvas login + same-origin proxy, new routes off under `NURTUO_INSECURE_NO_AUTH=1`? (D9)
- **Q10 (blocking):** inline unsaved flow only, no component id? (D10)
- **Q11 (blocking):** only `sftp` `GET`/`LIST` and no private addresses by default? (D11)
- **Q12 (non-blocking):** minimal child environment for secrets? (D12)
- **Q13 (non-blocking):** `atmoz/sftp` in CI, Rebex manual only? (D13)
- **Q14 (non-blocking):** one spec, six phases? (D14)
- **Q15 (non-blocking):** path as an input property in the Run dialog? (D15)
- **Q16 (blocking):** additive `steps` in `nurtuo flow run --json`? (D16)

## Scope

In scope:

- `nurtuo flow run --json`: additive `steps` array (D16).
- Control plane: workspace-scoped connections and write-only secrets API; test-run endpoint that
  spawns `nurtuo flow run`; SFTP host-key scan endpoint (TOFU); new permissions; OpenAPI and policy
  entries; isolation tests.
- Canvas (`core/web`): control-plane client, login and workspace picker, connections panel with SFTP
  form and host-key flow, connection picker in the connector config, Run button, Run dialog (input
  properties), results panel, per-step colouring.
- Playwright: UI tests with a mocked API in the `web-e2e` job, and a full canvas-run e2e against
  `atmoz/sftp` in that job.

Non-goals (deferred, each needs its own spec or decision):

- Running on a deployed runtime via the gateway; fixing #56.
- Saving or loading flows to/from the control plane (canvas-editor gap 2), revisions, optimistic save.
- Persistent or external secret stores (Vault, keychain), secret rotation.
- Run history, run sharing, async jobs, streaming progress.
- Connectors other than `sftp` and mutating operations in test runs (D11); `script` steps (no runner).
- Any change to `nurtuo.flow/v1`, `nurtuo.connection/v1` (fields), the SFTP connector or `nurtuo run`.
- Workspace isolation of existing tables (#55) and the workspace-creation gate (#54).
- MCP tools for test runs.

## Design

### What exists today (code on `main` at `ad72ae2`, plus the named PR branches)

| Area | Built | Missing for this feature |
|---|---|---|
| Canvas `core/web` | `App.tsx` toolbar (Undo, Redo, Load JSON, Save flow, Save layout), palette from `STEP_CATALOG` (labels "Start", "Connector", "Return Documents"), `Canvas.tsx` custom `step` node with diagnostics severity styling, `ConfigDialog.tsx` generated from `configSchemaOf(kind)` via `fieldsOf` (`formSchema.ts`); the connector `connection` field is free text. `FlowStore` (`flowStore.ts`) publishes `FlowState` and runs `validateFlow` on every change. No network calls; `@nurtuo/client` is a dependency but not imported | any control-plane client, login, workspace, connections UI, Run button, results panel, run status on nodes. "Test mode with per-step colours" is listed as not built in the canvas-editor spec |
| Control plane `core/control-plane` (Node, TypeScript) | REST routes in `api.ts` `route(repo, req)` for components, revisions, runtimes, deployments; auth hook `createAuthHook` (`auth/middleware.ts`): match policy (fail closed) -> authenticate (cookie or bearer) -> CSRF for cookie mutations -> resolve workspace (`path`/`context`/`none`) -> `authorize(permission, {workspaceId})` -> handler -> audit (hashes only, `beforeHash`/`afterHash`). Auth-module handlers in `auth/endpoints.ts` (`endpoints: Record<string, Endpoint>`, keyed by policy action) receive `EndpointCtx.workspace`; legacy `api.ts` routes do not. `policy.test.ts` fails when an `openapi.yaml` operation has no policy. Permissions in `auth/permissions.ts` mirrored by `migrations/0103_auth_seed_roles.sql` | **no connection resource, no secret store, no run endpoint** (`openapi.yaml` paths: components, runtimes, deployments, auth, scim, roles, workspaces/members/tokens/audit); no request body limit (`createHttpServer` concatenates all chunks, control-plane gap 2); no CORS (none needed with D9) |
| Engine `core/runtime/internal/engine` (Go, `internal`) | `engine.New()`, `(*Engine).Run(ctx, *flow.Flow, *docs.Set, *Process) (*Result, error)`; `Result{Returned, Failed, Stop, Trace}`; `StepTrace{StepID, Kind, In, Out, Failed, Dropped}` appended per step visit (`interp.go`), returned also on error/cancel | nothing outside the engine reads `Trace` |
| `nurtuo flow run` (PR #51, `core/runtime/cmd/nurtuo/flowrun.go`) | `flowRun(args, stdout, stderr, reg)`: `loadFlow` (`flow.ValidateJSON`+`flow.Parse`), `loadConnections`, `crossCheck`, `applyHostKeys` (`--known-hosts` > inline `knownHosts` > `~/.ssh/known_hosts`; file-level `insecure` without `--insecure-host-key` = exit 2), `checkLiterals`, `chainProvider{secrets.NewEnv(), secrets.NewDir()}`, `connhost.NewManager(reg, &connections.Resolver{...})`, `Prepare`/`Commit`, `eng.Registry.RegisterConnectorLookup(mgr)`, one injected document with `--set` properties, `--timeout`, `writeOutput` `--json` envelope `{status: 200, documents: [{id, properties, payload(base64)}]}`, JSON errors on stderr `{error: {code, message}}`, exit codes 0/1/2/3, `Redactor` on all stderr | per-step status in the envelope (D16); not merged |
| Connections and secrets (runtime) | `nurtuo.connection/v1` (`connections.Parse`), `secret://<name>` (name `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}`), providers `env` (`NURTUO_SECRET_<NAME>`, `EnvVar` upper-cases and maps other characters to `_`), `dir`, `encrypted-file`; `CheckLiterals`; `Redactor` | per-workspace secret scoping (a documented non-goal of that spec); nothing in the control plane |
| SFTP connector `connectors/sftp` | settings `host`, `port`, `user`, `password`, `privateKey`, `passphrase`, `hostKeyPolicy` (`known_hosts` default, `fingerprint`, `insecure`), `knownHosts` (content), `hostKeyFingerprint` (compared with `ssh.FingerprintSHA256`), `rootPath`, `timeoutMs`; `GET` emits `<step>.out` with properties `path`, `filename`; the fingerprint callback's error is `host key mismatch: got <fp>, want <fp>` (`config.go`) | a way to read a server's host key without trusting it (needed for TOFU, D4) |
| Standalone `core/runtime/cmd/nurtuo/standalone.go` | the Go binary supervises the Node control plane as a child (`--control-plane-cmd node`, `controlPlaneEnv(dsn, creds, port, gatewayAddr)`) | it does not tell the control plane where the `nurtuo` binary is |
| Playwright | `core/web/e2e/canvas.spec.ts` (10 tests, Vite on `127.0.0.1:5199`, not in CI on `main`); on PR #51/#53: `core/web/e2e/flow-run-export.spec.ts` builds the SFTP flow, exports it and, when `NURTUO_FLOW_RUN_BIN` is set, runs it with the binary; PR #53 adds the `web-e2e` job (Node 24, Playwright chromium, builds `nurtuo`, starts `atmoz/sftp@sha256:c5fb9e4b…` on a random loopback port with a random password masked by `::add-mask::`, writes `known_hosts` with `ssh-keyscan`, exports `NURTUO_SECRET_SFTP_PASSWORD`) | a test that drives Run in the canvas against a control plane |

### Drift found while designing

| Path | What the doc says | What the code does | Handling |
|---|---|---|---|
| Brief for this spec; [ARCHITECTURE.md](../ARCHITECTURE.md) §6.3 ("test mode" as a trigger of the engine) | test run "through the in-process engine in the control plane" | the control plane is Node (`core/control-plane/package.json`, `src/*.ts`); the engine is Go under `core/runtime/internal/`, importable only from `core/runtime/` | design against the code: the control plane spawns `nurtuo flow run` (D7, Q7) |
| [canvas-editor](implemented/2026-11-21-canvas-editor.md) | `@nurtuo/client` dependency "is not imported"; canvas "does not talk to the control plane" | same on `main` (`core/web/src`, no `fetch`) | none; this spec adds the first network calls |
| [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md) | per-workspace secret scoping is a non-goal; secrets are resolved by the runtime | same; the control plane has no secret code | this spec adds a control-plane store (D3) without changing the runtime providers |
| [workspace-isolation](2026-11-22-workspace-isolation.md) | path-scoped aliases `/v1/workspaces/{ws}/...` are "optional later" (phase 3) | path-scoped routes exist only for members/tokens/audit | new resources start path-scoped (D8); consistent with its direction, no conflict |
| [control-plane-api-and-data](implemented/2026-11-21-control-plane-api-and-data.md) gap 2 | no body limit | confirmed in `createHttpServer` (`api.ts`) | Phase 2 adds a limit (D2) |
| `docs/specs/2026-11-23-nurtuoctl-run.md` (PR #50) | `--json` envelope `{status, documents}` | matches `writeOutput` on PR #51 | extended additively (D16) |

### Architecture

```
core/web (browser)                         core/control-plane (Node)                         nurtuo (Go child, per run)
 Run dialog ──POST /v1/workspaces/{ws}/test-runs──> auth hook (session, CSRF, membership,
   flow JSON + input.properties                      test_run:execute)
                                                    testrun/routes.ts
                                                      validate body, limits, D11 policy
                                                      load connections(ws) + secrets(ws)
                                                      write tmp dir: flow.json, <id>.connection.json
                                                      spawn ──────────────────────────────────> nurtuo flow run tmp/flow.json --json
                                                        env: NURTUO_SECRET_<NAME>=…, HOME=tmp      --connection tmp/<id>.connection.json …
                                                                                                   --set k=v … --timeout 30s
                                                      read stdout ≤ limit, stderr  <────────────  engine.New + connhost.Builtins + Manager
                                                      kill on timeout / client abort               (same code as the CLI)
                                                      rm -rf tmp
 results panel <── 200 {runId, status, steps[], documents[], error?}
```

The engine path is identical to the CLI: same flags, same validation, same host-key rules, same
secret resolution and redaction. Only the entrypoint differs:

| | `nurtuo flow run` (PR #51) | canvas Run (this spec) |
|---|---|---|
| Caller | a developer in a terminal | control-plane `TestRunner`, one child per request |
| Flow source | file | inline JSON from `FlowStore`, written to a 0600 temp file |
| Connections | `--connection` files on disk | workspace connections, written to temp files for the run |
| Secrets | env / `--secrets-dir` of the user | workspace `SecretStore`, passed as `NURTUO_SECRET_*` in a minimal env (D12) |
| Host key | `--known-hosts`, inline, `~/.ssh/known_hosts`, `--insecure-host-key` | inline `fingerprint`/`knownHosts` from the connection only; `HOME` is the temp dir so the `~/.ssh` fallback finds nothing; `--insecure-host-key` is never passed |
| Output | raw bytes or `--json` | always `--json`, mapped to the API response |

### Phase 1: per-step status from the CLI (D16)

In `core/runtime/cmd/nurtuo/flowrun.go` (after PR #51 merges), `writeOutput` adds
`"steps": [{"id", "kind", "in", "out", "failed", "dropped"}]`, built from `res.Trace` and aggregated
by `StepID` in first-seen order (a step visited twice by two branches sums its counters). The JSON
error object on stderr gains the same `steps` when `eng.Run` was reached (failed documents, stop,
timeout, cancel). Errors before the engine (usage, validation, `Prepare`) carry no `steps`. Purely
additive; raw (non-`--json`) output is unchanged. The `nurtuo flow run` spec gets a changelog line in
the implementation PR (its file is on PR #50; this spec does not edit it).

### Phase 2: connections and secrets in the control plane

New module `core/control-plane/src/connections/` **[DEFAULT, pending Łukasz: D3, D6, D8]**:

- `ConnectionRepository` with `MemoryConnectionRepository` and `PgConnectionRepository`. Every
  method takes `workspaceId` first (the rule the workspace-isolation spec proposes for all
  repositories). Postgres migration `0300_workspace_connections.sql` **[DEFAULT: number chosen to stay
  clear of `0100` auth and `0200` workspace isolation]**:
  `connections(workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE, id text NOT NULL, connector text NOT NULL, settings jsonb NOT NULL, created_at, updated_at, PRIMARY KEY (workspace_id, id))`.
  Keyed by `workspace.id`, not slug (slugs can change via `PATCH /v1/workspaces/{ws}`).
- `SecretStore` interface `{ put(ws, name, value), has(ws, name), list(ws), delete(ws, name), reveal(ws, name) }`;
  `MemorySecretStore` only in this slice (a `Map<workspaceId, Map<name, Buffer>>`). `reveal` is
  called only by `TestRunner`. Values are never serialised into responses, logs or audit details.
- Validation on `PUT .../connections/{id}`, mirroring the Go rules so the user gets the error at save
  time (the child re-validates anyway): `id` matches `^[a-z][a-z0-9_-]*$`; `connector` is a
  registered connector (`sftp` only in this slice, D11); fields marked `writeOnly` /
  `x-nurtuo-secret` in `connectors/sftp/schemas/connection.json` must be `secret://<name>`
  references (literal -> 400 `literal_secret`); `hostKeyPolicy: insecure` -> 400
  `insecure_host_key_not_allowed`; `known_hosts` without non-empty `knownHosts`, or `fingerprint`
  without `hostKeyFingerprint` -> 400 `host_key_required`. The schema JSON is read from the repo at
  build time **[DEFAULT]**; a missing secret is allowed at save (shown as `missing`).
- Secret names from the canvas are derived as `<connectionId>-<field>` (for example
  `rebex-demo-password`); `PUT .../secrets/{name}` refuses a name whose `EnvVar` form equals that of
  another secret in the workspace (409 `secret_name_collision`, D12).
- Request body limit in `createHttpServer`: 1 MiB, 413 `payload_too_large` before JSON parsing
  **[DEFAULT, pending D2]**. This applies to every route; no existing route needs more (revisions
  are flow documents). Open: whether bundles posted as revision `content` can exceed it (Gaps #9).
- Routes are auth-module endpoints so they receive `EndpointCtx.workspace`: `createAuthHook` gains
  an option `extraEndpoints?: Record<string, Endpoint>` merged into the `endpoints` lookup
  (`auth/middleware.ts`), and `main.ts` passes the connection, secret and test-run endpoints. When
  `auth: "disabled"` these routes are not reachable (the legacy `route` returns 404), by design (D9).

### Phase 3: the test-run endpoint

New module `core/control-plane/src/testrun/` **[DEFAULT, pending D1, D2, D7, D10, D11, D12]**:

`POST /v1/workspaces/{ws}/test-runs`, request:

```json
{
  "flow": { "schema": "nurtuo.flow/v1", "id": "sftp-download", "steps": [ ... ], "edges": { ... } },
  "input": { "properties": { "path": "/readme.txt" } },
  "timeoutMs": 30000
}
```

`TestRunner.run(ws, request, signal)`:

1. Limits: body ≤ 1 MiB (Phase 2), `timeoutMs` in 1000..120000 (default 30000), at most 64 input
   properties, keys `^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`, values ≤ 4 KiB. Concurrency semaphore 2 per
   workspace, 4 per instance -> 429 `too_many_runs` with `retry-after: 5`.
2. Validate `flow` with `validateFlow` from `@nurtuo/flow-validate` (the same library as the canvas);
   errors -> 400 `invalid_flow` with the diagnostics. D11: every `connector` step must be `sftp`
   with `GET` or `LIST` -> otherwise 400 `not_allowed_in_test_run` naming the step.
3. Resolve each referenced connection id in workspace `ws` (`ConnectionRepository.get(ws, id)`);
   missing -> 404 `connection_not_found` naming the id (the same for an id of another workspace).
   Collect the `secret://` names; a name not in `SecretStore` for `ws` -> 409 `secret_missing` naming
   the secret and connection, never a value.
4. Egress check (D11): resolve `settings.host` (`dns.lookup`, all addresses); refuse loopback,
   RFC 1918, RFC 4193, link-local and `169.254.169.254` unless `NURTUO_TEST_RUN_ALLOW_PRIVATE=1` ->
   400 `host_not_allowed`. Best effort: the child resolves again (DNS rebinding, see Gaps).
5. Create `mkdtemp(<os tmp>/nurtuo-testrun-)` (0700); write `flow.json` and one
   `<id>.connection.json` per connection (0600, non-secret content only).
6. Spawn `NURTUO_FLOW_RUN_BIN` with argv `flow run <tmp>/flow.json --json --timeout <timeoutMs>ms`
   plus `--connection` per file and `--set k=v` per input property (argv array, no shell). The
   environment is built from scratch: `PATH`, `HOME=<tmp>`, `TMPDIR=<tmp>`, and
   `NURTUO_SECRET_<EnvVar(name)>=<value>` per referenced secret. Nothing else is inherited.
7. Read stdout up to 1.5 MiB (base64 of 1 MiB plus envelope) and stderr up to 64 KiB; beyond the
   stdout limit kill the child and report `output_too_large`. Kill (SIGTERM, then SIGKILL after 5 s)
   when `timeoutMs + 5 s` passes or the HTTP client disconnects (the `nurtuo` side closes
   connections on SIGTERM, PR #51 `signal.NotifyContext`).
8. Map: exit 0 -> `succeeded` with `documents` and `steps`; exit 1 or 2 -> `failed` with
   `error {code, message}` (from the CLI's JSON stderr, passed through `redact()` with the run's
   secret values as defence in depth) and `steps` when present; exit 3 -> `stopped` with
   `stop.status`. Step status per step id of the request flow: `ok` (in trace, `failed == 0`),
   `error` (`failed > 0`), `not_run` (absent from the trace). When the error happened before the
   engine and names a connection (the `Prepare` message names it), the connector steps using that
   connection are `error`, the rest `not_run`.
9. Always `rm -rf <tmp>` in `finally`; release the semaphore.

Response 200 (also for failed and stopped runs: a failed run is a result, not an API error):

```json
{
  "runId": "8f0c…",
  "status": "succeeded",
  "durationMs": 812,
  "steps": [
    { "id": "start-1", "kind": "start", "status": "ok", "in": 1, "out": 1, "failed": 0, "dropped": 0 },
    { "id": "connector-1", "kind": "connector", "status": "ok", "in": 1, "out": 1, "failed": 0, "dropped": 0 },
    { "id": "returndocuments-1", "kind": "returnDocuments", "status": "ok", "in": 1, "out": 0, "failed": 0, "dropped": 0 }
  ],
  "documents": [
    { "id": "connector-1.out", "properties": { "path": "/readme.txt", "filename": "readme.txt" }, "size": 379, "payload": "V2VsY29tZS4uLg==" }
  ]
}
```

Failed: `"status": "failed", "error": { "code": "execution", "message": "connection \"rebex-demo\": host key mismatch: got SHA256:…, want SHA256:…" }`.
Other API errors use the existing `{ "error": { "code", "message" } }` shape; 503
`test_run_unavailable` when `NURTUO_FLOW_RUN_BIN` is unset or not executable.

Binary location: `NURTUO_FLOW_RUN_BIN` (absolute path). `nurtuo standalone` sets it to
`os.Executable()` in `controlPlaneEnv` (`core/runtime/cmd/nurtuo/standalone.go`), so `make dev`
works without configuration **[DEFAULT]**. Audit: the endpoint returns
`audit: { after: { runId, status } }` so the audit `afterHash` never covers output data.

### Phase 4: canvas

New files in `core/web/src/` **[DEFAULT, pending D3, D4, D9, D15]**:

- `api/controlPlane.ts`: `fetch` wrapper with `credentials: "same-origin"`, `X-Nurtuo-Csrf` on
  mutations (token from the login response, kept in memory only), base path
  `/v1/workspaces/{ws}`; typed with `@nurtuo/client` types regenerated from `openapi.yaml`
  (`sdk/ts/client` `gen` script). `vite.config.ts` gains `server.proxy: { "/v1": NURTUO_CP_URL ?? "http://127.0.0.1:8080" }`.
- `ui/LoginBar.tsx`: email/password login, logout, workspace picker (`GET /v1/workspaces`). Offline
  (no control plane) the canvas works exactly as today; Run and Connections are disabled with a
  hint.
- `ui/ConnectionsPanel.tsx`: list (`GET .../connections`), SFTP form generated with `fieldsOf` over
  `connectors/sftp/schemas/connection.json` (`writeOnly` fields render as `type="password"`, never
  pre-filled; a "set"/"missing" badge from the list response), save = `PUT .../secrets/<id>-password`
  (only when the field was typed into) then `PUT .../connections/<id>` with
  `password: "secret://<id>-password"`. Host-key section: "Fetch host key" (Phase 5) or paste a
  fingerprint or `known_hosts` content; no insecure choice.
- `ConfigDialog.tsx`: for `kind: connector` the `connection` field becomes a select of workspace
  connections whose `connector` matches (free text when offline).
- `model/runState.ts`: `RunState { status, steps: Record<stepId, StepRun>, documents, error, flowHash }`;
  cleared when `FlowStore` publishes a different `flow` (any edit invalidates the colours).
- `App.tsx`: **Run** button in the toolbar, disabled while there are error diagnostics, no
  workspace is selected or a run is in flight; Run dialog with "Input properties" rows (pre-filled
  `path` for SFTP `GET`) and timeout; results section in the side panel: status, duration, per-step
  list (click selects the step), documents (UTF-8 text preview up to 64 KiB in a `<pre>` rendered by
  React, otherwise "binary, N bytes"; "Download" via `Blob`), error message.
- `Canvas.tsx`: step nodes get `data-run-status="ok|error|not_run|stopped"` and the classes
  `run-ok` (green), `run-error` (red), `run-not-run` (grey), with a text label as well as colour
  (accessibility, canvas-editor gap 3).

The flow JSON can never contain the password: `ConnectorConfig` has only `connection` (an id), and
the connection form writes passwords only to the secrets endpoint. "Save flow" output is unchanged.

### Phase 5: TOFU host-key scan (D4)

`POST /v1/workspaces/{ws}/connections/host-key-scan` `{ "host", "port" }` (permission
`connection:write`, same D11 egress check) spawns a new Go subcommand
`nurtuo flow host-key <host> [--port N] [--timeout 10s] --json` **[DEFAULT: name pending]**, which
dials with an `ssh.ClientConfig` whose `HostKeyCallback` records `key.Type()` and
`ssh.FingerprintSHA256(key)` and then aborts the handshake (no authentication is attempted, no
credentials are involved), printing `{ "keys": [{ "type", "fingerprintSha256" }] }`. It lives in
`core/runtime/cmd/nurtuo` next to `flowrun.go`. The UI shows the fingerprint with the text
"Compare this with the fingerprint published by the server's administrator" and a "Trust this key"
button; only then is `hostKeyPolicy: fingerprint` + `hostKeyFingerprint` saved. Without Phase 5 the
paste path from Phase 4 is complete.

### Phase 6: Playwright e2e in CI

See [Tests](#tests). Changes `.github/workflows/ci.yml` (the `web-e2e` job from PR #53), which is
"Ask First" in `AGENTS.md` (PR #49).

### Failure modes and what the user sees

| Failure | API | Canvas |
|---|---|---|
| not logged in / session expired | 401 | login bar asks to sign in; Run disabled |
| no `test_run:execute` (viewer) | 403 `forbidden` | Run disabled with "your role cannot run flows" |
| connection id unknown in this workspace | 404 `connection_not_found` | error names the step and the id |
| secret not set (for example after a control-plane restart, D3 memory backend) | 409 `secret_missing` | "password for rebex-demo is not set; open Connections" |
| wrong password | 200 `failed`, code `execution` | connector step red; message from the CLI (`Redactor`-scrubbed) |
| host key changed / wrong fingerprint | 200 `failed`, message `host key mismatch: got …, want …` | connector step red; hint "the server's key changed; verify before trusting the new key" (no one-click replace) |
| remote file missing | 200 `failed`, path named | connector step red |
| timeout | 200 `failed`, code `timeout` | steps that ran are coloured, the rest grey |
| output too large | 200 `failed`, code `output_too_large` | "output larger than 1 MiB; use `nurtuo flow run`" |
| too many concurrent runs | 429 | "another run is in progress, retry" |
| binary missing | 503 `test_run_unavailable` | "test runs are not configured on this server" |
| control plane unreachable | network error | canvas keeps working offline; Run disabled |

### Rollback

Each phase is additive. Phase 1 adds JSON fields. Phases 2-5 add routes, a table, permissions and a
role-seed migration; rollback = remove the routes from `main.ts` (data in `connections` is inert);
the table can be dropped by a later migration. Phase 4 UI is inert without a control plane. No
existing behaviour changes except the 1 MiB request body limit (Phase 2), which is a single
constant.

### Alternatives considered

- **Deploy to a runtime and trigger** (`run_now` via the gateway): blocked by #56 in standalone,
  needs a bundle, signing, desired state and a registered runtime; seconds to minutes; deferred (D7).
- **Long-running Go test-run service**: a new process to supervise, a new internal protocol and
  port; more moving parts than a per-run child. Revisit if per-run start-up cost (tens of ms) matters.
- **Run the engine in the browser (WASM)**: SFTP needs TCP; browsers cannot open it.
- **Secrets in browser storage, sent with each run**: credentials in `localStorage` are readable by
  any script on the origin; rejected.
- **Connections as components** (`kind: connection`): components are global until #55, so a
  connection would leak across workspaces; rejected for this slice.

## Contracts

All new contracts are **experimental** until this spec is implemented and a release note says
otherwise.

| Contract | Change | Stability |
|---|---|---|
| `GET /v1/workspaces/{ws}/connections` | new; `{ items: [{ id, connector, settings, secrets: { <field>: "set" \| "missing" }, updatedAt }] }`, secret fields shown only as `secret://<name>` | experimental |
| `GET\|PUT\|DELETE /v1/workspaces/{ws}/connections/{id}` | new; PUT body `{ connector, settings }`; 400 `literal_secret`, `insecure_host_key_not_allowed`, `host_key_required`, `not_allowed_in_test_run` | experimental |
| `GET /v1/workspaces/{ws}/secrets` | new; names and `updatedAt` only | experimental |
| `PUT\|DELETE /v1/workspaces/{ws}/secrets/{name}` | new; PUT body `{ value }`, 204, value never returned; 409 `secret_name_collision` | experimental |
| `POST /v1/workspaces/{ws}/connections/host-key-scan` | new (Phase 5) | experimental |
| `POST /v1/workspaces/{ws}/test-runs` | new; request and response as in Design | experimental |
| Request body limit | all routes: 413 `payload_too_large` above 1 MiB | new behaviour |
| Permissions | new resources `connection` (`read`, `write`, `delete`), `secret` (`write`, `delete`), `test_run` (`execute`) in `auth/permissions.ts`; role seed migration `0104_auth_seed_connection_permissions.sql` | experimental |
| Environment | `NURTUO_FLOW_RUN_BIN`, `NURTUO_TEST_RUN_ALLOW_PRIVATE` (control plane) | experimental |
| `nurtuo flow run --json` | additive `steps` in the envelope and the JSON error (D16) | experimental (as PR #51) |
| `nurtuo flow host-key` | new subcommand (Phase 5) | experimental |
| `nurtuo.flow/v1`, `nurtuo.connection/v1`, `secret://` | unchanged | — |

`AGENTS.md` (PR #49) lists `core/control-plane/openapi.yaml` and CLI flags as "Ask First"; approval
of this spec is that request.

## Compatibility

- REST: only new paths and new permissions; existing clients are unaffected (the
  [backward-compatibility contract](2026-11-22-backward-compatibility-contract.md) treats `/v1/` paths
  as STABLE and response fields as ADDITIVE-ONLY; nothing existing changes). The 1 MiB body limit
  is the one behaviour change: a client posting a larger revision today would get 413 (none known;
  see Gaps #9).
- Roles: built-in roles gain permissions; `canGrant` is role-agnostic, so an editor can still grant
  editor; the `0104` seed keeps `permissions.ts` and SQL in sync (existing drift test).
- `nurtuo flow run --json`: additive keys; consumers must ignore unknown fields.
- Canvas: offline behaviour unchanged; "Save flow" output unchanged.
- Downgrade: the `connections` table and permissions remain; an older control plane ignores them.

## Security

- **Secrets.** Write-only API; values only in `MemorySecretStore` and in the child's environment for
  the duration of a run; never in flow JSON, connection JSON, responses, logs, audit details or temp
  files. The child environment is built from scratch (D12), so control-plane secrets
  (`NURTUO_AUTH_SECRET`, `DATABASE_URL`, bootstrap password) never reach it. `Redactor` in the child
  plus `redact()` on the captured stderr in the control plane.
- **Host keys.** Verified on every run; `insecure` cannot be stored or requested; the CLI's
  `~/.ssh/known_hosts` fallback cannot pick up the server account's file (`HOME=<tmp>`); TOFU stores
  nothing without explicit confirmation; a mismatch fails the run.
- **Authorization.** Path-scoped routes (`workspace: "path"`, membership check, 404 for foreign
  workspaces), permissions of D6, `userOnly` for secrets and runs, CSRF for cookie mutations
  (existing middleware). Routes are absent under `NURTUO_INSECURE_NO_AUTH=1`.
- **Workspace isolation (D8).** New rows carry `workspace_id`; every repository/store call takes
  it; ids from another workspace answer exactly like missing ones; the per-run temp dir contains
  only that run's files; no caching of connections or secrets across runs.
- **Egress / SSRF (D11).** `sftp` only, `GET`/`LIST` only, private ranges refused by default; the
  host-key scan uses the same check. Remaining risk: DNS rebinding (Gaps #3).
- **Input limits.** Body 1 MiB, properties 64 × 4 KiB, timeout ≤ 120 s, output ≤ 1 MiB,
  concurrency 2/4; argv passed as an array (no shell).
- **Output rendering.** Previews are React text nodes; downloads use `Blob`; no `innerHTML`.
- **Audit.** `test_run.execute`, `connection.*`, `secret.*` are audited with hashes of non-secret
  summaries only.

## Step plan

Phases are independently shippable stories; each step leaves the system working and names its
verification. Prerequisites: PR #51 merged (Phases 1, 3, 5); PR #53 merged (Phase 6). Phase 2 has no
prerequisite.

### Phase 1: per-step status in `nurtuo flow run --json` (Go, `core/runtime`)

1. Add `steps` (aggregated `res.Trace`) to the `--json` success envelope. Verify:
   `TestFlowRunJSONSteps`; existing `TestFlowRunJSONEnvelope` still green.
2. Add `steps` to the JSON error object when the engine ran (failed documents, stop, timeout).
   Verify: `TestFlowRunJSONErrorSteps`, `TestFlowRunJSONNoStepsBeforeEngine`.
3. `docs/cli.md` note and a changelog line in the `nurtuo flow run` spec. Verify:
   `node scripts/check-docs-links.mjs`.

### Phase 2: workspace connections and secrets API (control plane)

4. Permissions `connection:*`, `secret:*`, `test_run:execute` in `auth/permissions.ts` and
   `migrations/0104_auth_seed_connection_permissions.sql`. Verify: the drift test
   "migration 0103 seed matches permissions.ts" in `auth/crypto.test.ts` extended to read `0103` + `0104`,
   `pg-auth.test.ts` migration list, `rbac.test.ts` role × route matrix extended.
5. Request body limit in `createHttpServer`. Verify: `api.test.ts` "body over 1 MiB is 413 before
   parsing".
6. `ConnectionRepository` (memory + Pg) with migration `0300_workspace_connections.sql`. Verify:
   `connections/repository.test.ts` (memory), `connections/pg-repository.test.ts` (`DATABASE_URL`-gated).
7. `MemorySecretStore` and the `extraEndpoints` seam in `createAuthHook`. Verify:
   `connections/secrets.test.ts`, `auth/authn.test.ts` case "extra endpoints receive the resolved workspace".
8. Connection and secret routes, `openapi.yaml`, `POLICIES`. Verify: `policy.test.ts` (every
   operation has a policy, 401/403 documented), `connections/routes.test.ts`, `connections/isolation.test.ts`.
9. `@nurtuo/client` types regenerated (`pnpm --filter @nurtuo/client run gen`). Verify: `gen:check`.

### Phase 3: test-run endpoint (control plane)

10. `TestRunner` with a fake binary fixture: temp dir, argv, minimal env, limits, timeout/kill,
    mapping. Verify: `testrun/runner.test.ts`.
11. Route `POST /v1/workspaces/{ws}/test-runs`, D11 policy, egress check, concurrency. Verify:
    `testrun/routes.test.ts`, `testrun/egress.test.ts`, `testrun/isolation.test.ts`.
12. `NURTUO_FLOW_RUN_BIN` from `nurtuo standalone` (`controlPlaneEnv`). Verify:
    `TestControlPlaneEnvFlowRunBin` in `core/runtime/cmd/nurtuo/standalone_test.go`.
13. Real-binary integration, gated. Verify: `testrun/runner.real.test.ts` with `NURTUO_FLOW_RUN_BIN`
    and `NURTUO_TEST_SFTP_*`.

### Phase 4: canvas connections, Run and results (paste host key)

14. `api/controlPlane.ts`, Vite proxy, `LoginBar`. Verify: `src/api/controlPlane.test.ts` (Vitest),
    `e2e/canvas-run-ui.spec.ts` "login and workspace picker" (mocked API).
15. `ConnectionsPanel` with SFTP form, secret write, paste host key; connection select in
    `ConfigDialog`. Verify: `canvas-run-ui.spec.ts` "create connection never sends the password to
    the connection endpoint".
16. Run button, Run dialog, `runState.ts`, results panel, node colouring, invalidation on edit.
    Verify: `src/model/runState.test.ts`, `canvas-run-ui.spec.ts` run cases; existing
    `canvas.spec.ts` unchanged and green.

### Phase 5: TOFU host-key scan

17. `nurtuo flow host-key`. Verify: `TestFlowHostKeyScan` (in-process SSH server).
18. Scan route and "Fetch host key / Trust this key" UI. Verify: `testrun/hostkey.test.ts`,
    `canvas-run-ui.spec.ts` "TOFU stores nothing until Trust".

### Phase 6: Playwright e2e in CI

19. `core/web/e2e/canvas-run.spec.ts` and a second Playwright `webServer` for the control plane,
    gated by `NURTUO_CANVAS_RUN_E2E=1`. Verify: skips locally without the variable; passes with it.
20. Extend the `web-e2e` job (PR #53) to build the control plane, start it with a random bootstrap
    password and `NURTUO_FLOW_RUN_BIN`, `NURTUO_TEST_RUN_ALLOW_PRIVATE=1`, and set
    `NURTUO_CANVAS_RUN_E2E=1`. Ask First (CI change). Verify: the job is green; the log shows no
    password (masked).

## Acceptance criteria

- WHEN an editor runs `Start -> sftp GET -> Return Documents` with input property `path` pointing
  at an existing file on a reachable server whose fingerprint is trusted THEN the response is 200
  `succeeded`, `documents[0].payload` decodes to the file bytes, and all three steps have status
  `ok`; the canvas shows the text and three green nodes.
- WHEN the SFTP password is entered in the connection form THEN it is sent only in
  `PUT /v1/workspaces/{ws}/secrets/{name}`; the connection is stored with `secret://{name}`; no
  response, "Save flow" download, audit row or log line contains the password.
- WHEN `PUT .../connections/{id}` contains a literal password, `hostKeyPolicy: insecure`, or no host
  key material THEN it is rejected with 400 and the respective code, and nothing is stored.
- WHEN the server's host key does not match the stored fingerprint THEN the run is `failed`, the
  connector step is `error`, and the message shows both fingerprints; the stored fingerprint is not
  changed.
- WHEN "Fetch host key" is used THEN the fingerprint is displayed and nothing is stored until
  "Trust this key" is pressed.
- WHEN a user of workspace A sends a connection id, secret name or test run naming a connection
  that exists only in workspace B THEN the answer is the same as for a non-existent id (404) and
  nothing of B is read or returned.
- WHEN a viewer or deployer calls `POST .../test-runs` or `PUT .../secrets/{name}` THEN 403; WHEN an
  API token calls them THEN 403 (`userOnly`).
- WHEN `NURTUO_INSECURE_NO_AUTH=1` THEN the new routes answer 404.
- WHEN the flow has a non-`sftp` connector or an operation other than `GET`/`LIST` THEN 400
  `not_allowed_in_test_run` and no process is spawned.
- WHEN the host resolves to a loopback/private/link-local address and
  `NURTUO_TEST_RUN_ALLOW_PRIVATE` is not `1` THEN 400 `host_not_allowed` and no process is spawned.
- WHEN a run exceeds its timeout THEN the child is killed, the temp dir is removed, the response is
  `failed` with code `timeout` and the steps that ran keep their status.
- WHEN the output exceeds 1 MiB THEN the run is `failed` with `output_too_large` and the child is
  killed.
- WHEN a run ends (any outcome) THEN its temp dir no longer exists and the child has exited.
- WHEN a third concurrent run starts in one workspace THEN 429.
- WHEN the flow is edited after a run THEN all run colours and results are cleared.
- WHEN the canvas has no control plane THEN it behaves as today and Run is disabled.
- WHEN `nurtuo flow run --json` succeeds THEN the envelope has `steps` with one entry per executed
  step id; raw output is unchanged.
- WHEN the control plane restarts THEN connections remain and secrets are reported `missing`
  (memory backend), and a run returns 409 `secret_missing`.

## Tests

Tests ship in the same PR as the code of each phase. Names are proposed; none exist yet.
Integration and e2e tests create their own fixtures (temp dirs, generated keys, containers, random
passwords) and clean them up. No password is committed anywhere.

### Go (`core/runtime`) — `cd core/runtime && GOWORK=off go test -race ./cmd/nurtuo/...`

| Test | File | Covers |
|---|---|---|
| `TestFlowRunJSONSteps` | `cmd/nurtuo/flowrun_test.go` | stub connector; `steps` ids/kinds/counters; a step visited twice is aggregated |
| `TestFlowRunJSONErrorSteps` | same | failed document and stop: JSON error carries `steps` with `failed` counts |
| `TestFlowRunJSONNoStepsBeforeEngine` | same | validation / `Prepare` errors have no `steps` |
| `TestFlowHostKeyScan` | `cmd/nurtuo/flowhostkey_test.go` | in-process SSH server (pattern of `connectors/sftp/sftp_test.go` `startServer`, copied): reports type and SHA-256; never authenticates; timeout on a silent port |
| `TestControlPlaneEnvFlowRunBin` | `cmd/nurtuo/standalone_test.go` | `controlPlaneEnv` contains `NURTUO_FLOW_RUN_BIN=<os.Executable()>` |

### Control plane (`core/control-plane`) — `pnpm --filter @nurtuo/control-plane run build && pnpm --filter @nurtuo/control-plane run test`

All handler tests drive `createHandler` in-process with `MemoryRepository`, `MemoryAuthRepository`
(`auth/test-util.ts`) and two workspaces populated.

| Test | File | Covers |
|---|---|---|
| connection CRUD, validation codes | `src/connections/routes.test.ts` | `literal_secret`, `insecure_host_key_not_allowed`, `host_key_required`, list shows `set`/`missing` |
| secrets write-only | `src/connections/secrets.test.ts` | PUT 204; GET lists names only; no route returns a value; `secret_name_collision` (`a-b` vs `a_b`); audit detail contains no value |
| isolation | `src/connections/isolation.test.ts`, `src/testrun/isolation.test.ts` | A cannot list/get/put/delete B's connections or secrets (404), cannot run with B's connection id (404), test-run with A's connection never reveals B's secrets (fake binary records env) |
| permissions | `src/auth/rbac.test.ts` (extended matrix) | viewer/deployer/runtime denied; tokens denied on `userOnly` routes |
| policy/openapi | `src/auth/policy.test.ts` (existing) | every new operation has a policy and documents 401/403 |
| no-auth mode | `src/api.test.ts` | new routes 404 with `auth: "disabled"`; body > 1 MiB is 413 |
| runner | `src/testrun/runner.test.ts` | fake binary `testdata/fake-nurtuo.mjs` (a Node script selected by an env var that echoes argv/env, prints canned envelopes, sleeps, floods stdout, exits 1/2/3): argv array, minimal env (no `NURTUO_AUTH_SECRET`), `HOME=<tmp>`, timeout kill, client abort kill, output limit, temp dir removed in every case, status mapping, `redact()` of echoed secret |
| egress and policy | `src/testrun/egress.test.ts` | loopback, 10/8, 172.16/12, 192.168/16, fc00::/7, 169.254/16 refused; allowed with `NURTUO_TEST_RUN_ALLOW_PRIVATE=1`; non-`sftp` connector and `PUT` operation refused before spawn |
| concurrency | `src/testrun/routes.test.ts` | third concurrent run in a workspace is 429; semaphore released on error |
| host-key scan | `src/testrun/hostkey.test.ts` | fake binary; egress check applies; nothing persisted |
| real binary | `src/testrun/runner.real.test.ts` | skipped unless `NURTUO_FLOW_RUN_BIN` and `NURTUO_TEST_SFTP_HOST/PORT/USER/PASSWORD/FINGERPRINT` are set (the `web-e2e` job sets them); GET returns the file; wrong password fails without leaking |
| Pg repository | `src/connections/pg-repository.test.ts` | skipped unless `DATABASE_URL`; migration `0300` and workspace-keyed queries |

### Canvas (`core/web`)

Unit — `pnpm --filter @nurtuo/web run test` (Vitest, node environment):

| Test | File | Covers |
|---|---|---|
| step status mapping and invalidation | `src/model/runState.test.ts` | `ok`/`error`/`not_run`/`stopped`; cleared when the flow changes; aggregated by step id |
| client | `src/api/controlPlane.test.ts` | CSRF header on mutations only, workspace path, error mapping; password never in the connection body |
| exported flow has no secret | `src/model/model.test.ts` (extended) | `serializeFlow` output of a flow with a connector contains only the connection id |

UI with a mocked API — `pnpm --filter @nurtuo/web run test:e2e` (runs in `web-e2e` without any
backend; `page.route("**/v1/**", …)` serves fixtures):

`core/web/e2e/canvas-run-ui.spec.ts`: login and workspace picker; offline canvas unchanged and Run
disabled; create SFTP connection — assert via `page.on("request")` that the password appears only in
the secrets PUT; run success colours three nodes `data-run-status="ok"` and shows the text preview;
run failure colours the connector `error` and shows the message; edit clears colours; binary output
shows "binary, N bytes"; TOFU stores nothing until "Trust"; no console errors.

Full e2e — `core/web/e2e/canvas-run.spec.ts`, gated by `NURTUO_CANVAS_RUN_E2E=1`:

- Fixtures from the job environment: `NURTUO_E2E_SFTP_HOST`, `_PORT`, `_USER`, `_PASSWORD` (random,
  masked), `_FINGERPRINT` (from `ssh-keyscan -t ed25519 … | ssh-keygen -lf -`), `_EXPECT` (file
  content); control plane started by Playwright `webServer` with a random `NURTUO_AUTH_SECRET` and
  bootstrap user/password generated in the job; in-memory repository, so no cleanup beyond process
  exit; the test deletes its connection and secret at the end anyway.
- Steps: log in; create connection `e2e-sftp` with the password and pasted fingerprint; build
  `Start -> Connector -> Return Documents` with the palette (helpers as in `flow-run-export.spec.ts`);
  pick `e2e-sftp`; Run with `path=/downloads/version.txt`; expect status `succeeded`, preview equals
  `_EXPECT`, three nodes `ok`.
- Negative: wrong fingerprint -> connector `error`, message contains "host key mismatch"; wrong
  password (second secret) -> `error`, page text does not contain either password.
- Leak checks: "Save flow" download does not contain the password; all captured response bodies do
  not contain it.
- Run locally: start `atmoz/sftp` as in PR #53, export the variables, then
  `NURTUO_CANVAS_RUN_E2E=1 pnpm --filter @nurtuo/web run test:e2e -- canvas-run.spec.ts`.
- `flow-run-export.spec.ts` (PR #51/#53) stays as is: it covers the CLI path; this file covers the
  control-plane path with the same container.

Manual smoke (not in CI, D13): `core/web/e2e/canvas-run-rebex.spec.ts`, gated by
`NURTUO_E2E_REBEX=1` and `NURTUO_E2E_REBEX_PASSWORD` (read from the environment, never committed),
runs the motivating example against `test.rebex.net` (`/readme.txt` **[to verify]**), fingerprint
via the TOFU flow compared against `NURTUO_E2E_REBEX_FINGERPRINT`.

### CI (`web-e2e`, PR #53, Phase 6)

Extend the job, not a new one: after "Start SFTP server…", add steps that build
`@nurtuo/control-plane`, compute the ed25519 fingerprint, generate the bootstrap password with
`openssl rand -hex 16` and `::add-mask::` it, and export `NURTUO_CANVAS_RUN_E2E=1`,
`NURTUO_TEST_RUN_ALLOW_PRIVATE=1`, `NURTUO_FLOW_RUN_BIN` (already exported by PR #53) and the
`NURTUO_E2E_SFTP_*` variables. Traces are uploaded on failure (existing step). Playwright traces can
contain typed form values; the e2e password is random per job and masked, and traces are kept 5 days
(PR #53), so no reusable secret is exposed.

### Docs

`node scripts/check-docs-links.mjs`; `docs/auth.md` (new permissions), `docs/security.md` (test-run
egress, secrets in the control plane), `docs/getting-started.md` (Run in the canvas).

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | An editor points a connection at `10.0.0.5:5432` or `169.254.169.254:80`; the control plane connects from inside the network (SSRF, port probing via error messages and timing) | high | security | D11: `sftp` only, private ranges refused by default; host-key scan uses the same check; document `NURTUO_TEST_RUN_ALLOW_PRIVATE` as dangerous |
| 2 | A flow with `sftp DELETE` run against production credentials deletes remote files | high | safety | D11: only `GET`/`LIST`; mutating operations need a later decision (`--read-only` in the CLI was an improvement idea in the `nurtuo flow run` spec) |
| 3 | DNS rebinding: `evil.example` resolves to a public IP at check time and `127.0.0.1` when the child dials | medium | security | follow-up: pass the checked IP to the child (`host` replaced by the address, SNI not relevant for SSH) or an egress proxy; document as best effort |
| 4 | The control plane restarts; every secret in `MemorySecretStore` is gone; runs fail with `secret_missing` until users re-enter passwords | medium | UX | D3 accepts this for the slice; clear 409 message; persistent encrypted store in a follow-up spec |
| 5 | Several control-plane replicas: the secret is set on replica 1, the run hits replica 2 and fails with `secret_missing` | medium | operations | memory backend documented as single-replica; persistent backend before HA |
| 6 | Secrets are in the child's environment; another process of the same OS user can read `/proc/<pid>/environ` during the run | low | security | same user as the control plane, which holds the values anyway; runs are short; D12 alternative documented |
| 7 | A hostile or buggy flow produces 4 GiB of output; Node buffers it | medium | reliability | stdout cap 1.5 MiB then kill; `docs.Spool` disk use in the child bounded by `TMPDIR=<tmp>`, removed after |
| 8 | The child hangs on a stalled SFTP server (SFTP gap 1: `timeoutMs` covers only connect) | medium | reliability | `--timeout` in the child plus SIGTERM/SIGKILL from the control plane; PR #51 made transfers cancellable |
| 9 | The new 1 MiB body limit rejects an existing client that posts a large revision (a bundle as `content`) | medium | compatibility | check bundle sizes in `nurtuoctl push` before Phase 2; make the limit per route if needed (D2) |
| 10 | The `Secure` session cookie is not stored when the canvas is served over plain `http://` on a non-localhost address (for example a LAN IP in `make dev`) | medium | UX | document `127.0.0.1`/`localhost` or TLS; Chromium treats loopback as secure (to verify in Phase 4) |
| 11 | A user trusts a TOFU fingerprint without comparing it, on a network with a MITM; credentials are then sent to the attacker | medium | security | explicit wording, show the fingerprint prominently, never auto-trust; paste path for out-of-band fingerprints |
| 12 | The server rotates its host key; every run fails and users ask for an "ignore" switch | low | UX | message explains rotation; replacing the key is the same TOFU/paste flow with the old one shown; no insecure switch |
| 13 | Workspace creation is ungated until #54; an attacker creates a workspace and becomes owner there | low (for this feature) | authz | new data is workspace-keyed, so they see only their own empty workspace; #54 remains required for the rest of the control plane |
| 14 | The TS-side connection validation drifts from `connections.Parse`/`CheckLiterals` | low | correctness | the child re-validates every run (exit 2 -> 400-like `failed` with the CLI message); shared fixture test runs both validators |
| 15 | `Result.Trace` counts documents, not causes: a failed document's error cannot always be attributed to the step that failed it | low | UX | step with `failed > 0` is red; message from the CLI; per-step error messages are an engine follow-up |
| 16 | Starting a Go process per run costs tens of milliseconds and some memory; a burst of runs from many users exhausts the host | low | performance | concurrency 2/4 (D2); revisit a long-running service (Alternatives) if needed |
| 17 | PR #51 or #53 changes or is not merged | medium | delivery | Phases 1, 3, 5, 6 wait; Phase 2 and the mocked-API part of Phase 4 can ship independently |

## Improvement opportunities

- Persistent encrypted workspace secret store; secret rotation propagating to deployments.
- Run a saved component revision; run on a deployed runtime (`run_now`) after #56.
- Async runs with progress events per step; run history with retention.
- Per-step document inspector (input/output documents of each step).
- A generic connection form for every connector from its `schemas/connection.json`.
- `nurtuoctl test-run` using the same endpoint.

## References

- [ARCHITECTURE.md](../ARCHITECTURE.md) §6.3 and §10 (Epic 10), [getting-started.md](../getting-started.md), [auth.md](../auth.md), [security.md](../security.md), [connectors/sftp.md](../connectors/sftp.md)
- Specs: [canvas-editor](implemented/2026-11-21-canvas-editor.md), [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md), [connector-sftp](implemented/2026-11-21-connector-sftp.md), [control-plane-api-and-data](implemented/2026-11-21-control-plane-api-and-data.md), [engine-interpreter](implemented/2026-11-21-engine-interpreter.md), [flow-format-v1-and-validator](implemented/2026-11-21-flow-format-v1-and-validator.md), [auth-rbac-audit](implemented/2026-11-21-auth-rbac-audit.md), [workspace-isolation](2026-11-22-workspace-isolation.md), [backward-compatibility-contract](2026-11-22-backward-compatibility-contract.md); `docs/specs/2026-11-23-nurtuoctl-run.md` (PR #50)
- PRs: https://github.com/nurtuo/nurtuo/pull/49 (AGENTS.md), https://github.com/nurtuo/nurtuo/pull/50, https://github.com/nurtuo/nurtuo/pull/51, https://github.com/nurtuo/nurtuo/pull/53
- Open Mercato: `integration-secret-credential-editing` (write-only credential fields), `tenant-ownership-and-module-acl-authorization`.

## Changelog

- 2026-11-23: created as draft
