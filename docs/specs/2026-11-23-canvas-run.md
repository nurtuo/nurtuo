<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: canvas Run (test-run a flow from the canvas against a real connection)

Status: in progress (implementation: [#87](https://github.com/nurtuo/nurtuo/issues/87), see [Implementation Status](#implementation-status)). Date: 2026-11-23. Approved by Łukasz on 2026-11-23 18:30 (Europe/Warsaw): decisions D1–D16 accepted as recommended.

Extends: [canvas-editor](implemented/2026-11-21-canvas-editor.md) (test mode, Epic 10),
[connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md),
[control-plane-api-and-data](implemented/2026-11-21-control-plane-api-and-data.md),
[nurtuo flow run](2026-11-23-nurtuoctl-run.md) (approved; PR #50, merged as `d5adeeb`).
Related: [workspace-isolation](2026-11-22-workspace-isolation.md) (draft).

> **Approved.** All blocking questions are resolved: Łukasz accepted D1–D16 exactly as recommended on
> 2026-11-23 (see [Decisions](#decisions)). Items still open are labelled **non-blocking** where they
> occur (they are verifications and follow-ups, not product decisions).

The approved operation limits and credential binding decisions in
[PR #92](https://github.com/nurtuo/nurtuo/pull/92) supersede this spec where they overlap, including
the original global request body limit and separate credential writes.

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
| Pull requests | spec: https://github.com/nurtuo/nurtuo/pull/68; implementation PRs follow. Depends on https://github.com/nurtuo/nurtuo/pull/51 (`nurtuo flow run`, open) and https://github.com/nurtuo/nurtuo/pull/53 (`web-e2e` job, open); its spec https://github.com/nurtuo/nurtuo/pull/50 is merged (`d5adeeb`); `AGENTS.md` is https://github.com/nurtuo/nurtuo/pull/49 (open) |
| ADRs | [ADR 0010](../adr/0010-connections-and-secrets.md) (connections and secrets), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) (per-document status), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (gateway; not used here) |
| Issues | https://github.com/nurtuo/nurtuo/issues/54, https://github.com/nurtuo/nurtuo/issues/55 (workspace isolation), https://github.com/nurtuo/nurtuo/issues/56 (standalone handshake, bypassed) |
| Code (existing) | `core/web/src/ui/{App,Canvas,ConfigDialog}.tsx`, `core/web/src/model/*.ts`, `core/control-plane/src/{api,main}.ts`, `core/control-plane/src/auth/{middleware,policy,permissions,endpoints}.ts`, `core/control-plane/openapi.yaml`, `core/runtime/internal/engine/interp.go`, `core/runtime/cmd/nurtuo/standalone.go`, `connectors/sftp/{config,sftp}.go`, `core/runtime/internal/secrets/env.go`; on PR #51: `core/runtime/cmd/nurtuo/{flow,flowrun}.go`, `core/web/e2e/flow-run-export.spec.ts` |
| User docs | [concepts.md](../concepts.md), [connectors/sftp.md](../connectors/sftp.md), [auth.md](../auth.md), [security.md](../security.md) |

## Goal

Who: an integration developer working in the canvas. Today the canvas (`core/web`) is a local file
editor with no network calls; to see a flow work the user must "Save flow", write a connection file,
export a secret into the environment and run `nurtuo flow run` in a terminal (PR #51), or deploy to a
runtime, which in standalone does not work yet (#56). Proposed: a **Run** button that executes the
current canvas flow against a connection stored in the user's workspace and shows the output and a
per-step status in the canvas within seconds. Why: it closes the "test mode with per-step colours"
item of M1 Epic 10 ([ARCHITECTURE.md](../ARCHITECTURE.md) §11, canvas-editor gap 2) with the engine
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

## Decisions

Date: 2026-11-23. Approved by Łukasz (2026-11-23 18:30, Europe/Warsaw): **all recommendations D1–D16
accepted exactly as recommended.** No blocking question remains open. Where checking the code showed
that the recommendation's wording named something that does not exist, the wording is corrected in the
"Applied as" column without changing the decision; those corrections are explicit.

| ID | Question | Decision (as recommended, accepted) | Applied as |
|---|---|---|---|
| D1 | Sync or async test run? | **Synchronous** `POST .../test-runs` returning the full result, bounded by the D2 timeout; the response carries a `runId` so job + poll can be added later without breaking clients | as decided |
| D2 | Timeouts and size limits? | **For test-run execution: timeout 30 s default, client may ask up to 120 s; request body ≤ 1 MiB (413); returned payload ≤ 1 MiB total decoded and ≤ 20 documents (else the run fails with `output_too_large`); 2 concurrent runs per workspace, 4 per instance (429)** | as decided; also bounds the child's spool, see Phase 3 step 7 |
| D3 | Where are secrets stored in M1, and how does the canvas refer to them? | **Workspace-scoped control-plane secret store behind a `SecretStore` interface, in-memory backend only in this slice** (values live in the control-plane process, never on disk, lost on restart; a persistent encrypted backend is a separate spec). The canvas and connections refer to secrets only as `secret://<name>`; the password is never in the flow JSON, the connection JSON, a response, a log or the audit log | as decided |
| D4 | How is the SFTP host key accepted in the UI? | **TOFU (server scans, user confirms the fingerprint) plus paste of a fingerprint or `known_hosts` content, never an insecure option.** Nothing is stored until the user presses "Trust this key"; the trusted key is saved as `hostKeyPolicy: fingerprint`. `hostKeyPolicy: insecure` is rejected by the API (400) and there is no UI for it. A later mismatch is a hard error, never a re-prompt that overwrites | as decided; the scan reports the key the connector actually negotiates (Phase 5) |
| D5 | Is run history persisted? | **Not persisted.** The result lives in canvas memory and is cleared on any edit, reload or logout; the server keeps only the audit row (action `test_run.execute`, status, no payload) | as decided |
| D6 | Who may create connections, set secrets and press Run? | **New permissions `connection:read`, `connection:write`, `connection:delete`, `secret:write`, `secret:delete`, `test_run:execute`. `connection:read` for owner, admin, editor, viewer and deployer; every other new permission for owner, admin and editor. `viewer` and `deployer` read connections only; `runtime` gets none. In this slice the secret and test-run routes are `userOnly` (no API tokens).** Secret values are never readable by anyone (no `secret:read` of values; listing returns names only) | the role list is explicit (the recommendation's phrase "all roles that have `component:read`" would have included `runtime`, which D6 itself excludes; F8) |
| D7 | Execution path? | **The control plane runs the engine via the `nurtuo` binary as a child (`nurtuo flow run --json`), no deploy.** Same engine and connector registry as PR #51, one process per run, no bundle, no gateway, so #56 does not block it. **Explicitly deferred:** test runs on a deployed runtime through the gateway (after #56), test runs of saved component revisions, async jobs, MCP `test_run` tool | as decided |
| D8 | Workspace isolation for this slice? | **Required now:** all new routes are path-scoped `/v1/workspaces/{ws}/...` (the membership check in `resolveWorkspace` already isolates them); new data (connections, secrets) is keyed by `workspace.id` from day one; a run resolves connections and secrets only from the URL's workspace; per-run temp dir and minimal child environment; a foreign id is 404. **Can wait:** #55 and #54, because the run takes the flow inline and never reads a component. **Condition:** if D10 later allows running a saved component, #55 must land first | #54 being open is accepted for this slice; because any session user can create a workspace and become owner, the host-key scan shares the run semaphore (Phase 5) |
| D9 | API shape and auth; how the canvas talks to the control plane | **Path-scoped routes + session cookie + CSRF, canvas same-origin through a proxy.** Routes under `/v1/workspaces/{ws}/` implemented as auth-module endpoints that receive the resolved `workspace`. Canvas: minimal login (`POST /v1/auth/login`), workspace picker (`GET /v1/workspaces`), `X-Nurtuo-Csrf` from the login response (restored from `GET /v1/auth/me` after a reload); dev and Playwright use a Vite `server.proxy` for `/v1`. **The new routes are not served when `NURTUO_INSECURE_NO_AUTH=1`: 404** | the legacy `route()` answers 405 for unknown `/v1/workspaces/...` paths today (F2); to keep the decided 404 the implementation adds an explicit 404 for them (Phase 2) |
| D10 | How does the canvas ship the flow; can unsaved state run? | **Inline flow JSON only.** The body carries the current `nurtuo.flow/v1` document from `FlowStore` (no layout); the server validates it again. No component id in this slice | as decided |
| D11 | Egress and side effects of a server-side run (SSRF) | **`sftp` only, read-only operations, private/loopback/link-local addresses refused unless the operator allows them.** Other connectors or operations are 400 `not_allowed_in_test_run`. Hosts that resolve to loopback, private, link-local or metadata ranges are refused unless `NURTUO_TEST_RUN_ALLOW_PRIVATE=1` (CI sets it for the container) | the recommendation named the operations "`GET` and `LIST`"; SFTP has no `LIST` (directory listing is `QUERY`; F1). The decided intent, read-only download and listing, is applied as **`GET` and `QUERY`**; nothing mutating is allowed |
| D12 | How are secret values handed to the child process? | **`NURTUO_SECRET_<NAME>` in a minimal, non-inherited environment**, built from scratch (no `DATABASE_URL`, `NURTUO_AUTH_SECRET`, bootstrap password etc.), `HOME` pointed at the per-run temp dir; secret names whose `EnvVar` form collides in a workspace are refused at write time | the variable name is exactly `secrets.EnvVar(name)`, which already includes the `NURTUO_SECRET_` prefix (F3) |
| D13 | Which SFTP server does the Playwright test use? | **`atmoz/sftp` container in CI** (pinned digest, random password per job, host key from the scan); **`test.rebex.net` only as a manual smoke test gated by `NURTUO_E2E_REBEX=1`, never in CI** | CI derives the fingerprint of the key the connector negotiates (Tests, CI) |
| D14 | Split into more than one spec? | **One spec with six independently shippable phases**; the persistent secret backend (D3), runtime-deployed test runs (D7) and run history (D5) are separate future specs | as decided |
| D15 | How does the user give the SFTP path? | **"Input properties" in the Run dialog**, sent as the initial document's properties (same as `--set`); pre-filled with `path=` when the flow has an SFTP `GET` step; a `setProperties` step keeps working | as decided |
| D16 | Per-step status from the engine: extend `nurtuo flow run --json`? | **Add an additive `steps` array (from `engine.Result.Trace`) to the `--json` envelope and to the JSON error object**, as a small follow-up to PR #51's experimental contract | as decided and nothing more: no further CLI fields are added; the control plane maps the CLI's existing error shapes (timeout, stop, `Prepare` failures) by code and message, with tests (F6, F7) |

## Open questions

None blocking: Q1–Q16 are answered by D1–D16. Remaining items are non-blocking and are tracked where
they occur: confirm Rebex's file path for the manual smoke test (Tests), confirm that Chromium keeps
the `Secure` cookie on `127.0.0.1` (Gaps #10), and a structured `connection`/`stop` field and a
`--max-output-bytes` flag in the CLI as later additive follow-ups (Gaps #7, Improvement opportunities).

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
- Serving the canvas outside `vite dev` (`make dev`, Playwright): the same-origin session cookie relies on the Vite proxy; a production topology (static hosting behind the control plane or a reverse proxy) needs its own spec.
- Control-plane hosts running Windows: the temp-dir modes (0700/0600) and the from-scratch child environment are specified for POSIX; on Windows `POST .../test-runs` answers 503 `test_run_unavailable`.
- Any change to `nurtuo.flow/v1`, `nurtuo.connection/v1` (fields), the SFTP connector or `nurtuo run`.
- Workspace isolation of existing tables (#55) and the workspace-creation gate (#54).
- MCP tools for test runs.

## Design

### What exists today (code on `main` at `d5adeeb`, plus the named PR branches)

| Area | Built | Missing for this feature |
|---|---|---|
| Canvas `core/web` | `App.tsx` toolbar (Undo, Redo, Load JSON, Save flow, Save layout), palette from `STEP_CATALOG` (labels "Start", "Connector", "Return Documents"), `Canvas.tsx` custom `step` node with diagnostics severity styling, `ConfigDialog.tsx` generated from `configSchemaOf(kind)` via `fieldsOf` (`formSchema.ts`); the connector `connection` field is free text. `FlowStore` (`flowStore.ts`) publishes `FlowState` and runs `validateFlow` on every change. No network calls; `@nurtuo/client` is a dependency but not imported | any control-plane client, login, workspace, connections UI, Run button, results panel, run status on nodes. "Test mode with per-step colours" is listed as not built in the canvas-editor spec |
| Control plane `core/control-plane` (Node, TypeScript) | REST routes in `api.ts` `route(repo, req)` for components, revisions, runtimes, deployments; auth hook `createAuthHook` (`auth/middleware.ts`): match policy (fail closed) -> authenticate (cookie or bearer) -> CSRF for cookie mutations -> resolve workspace (`path`/`context`/`none`) -> `authorize(permission, {workspaceId})` -> handler -> audit (hashes only, `beforeHash`/`afterHash`). Auth-module handlers in `auth/endpoints.ts` (`endpoints: Record<string, Endpoint>`, keyed by policy action) receive `EndpointCtx.workspace`; legacy `api.ts` routes do not. `policy.test.ts` fails when an `openapi.yaml` operation has no policy. Permissions in `auth/permissions.ts` mirrored by `migrations/0103_auth_seed_roles.sql` | **no connection resource, no secret store, no run endpoint** (`openapi.yaml` paths: components, runtimes, deployments, auth, scim, roles, workspaces/members/tokens/audit); no request body limit (`createHttpServer` concatenates all chunks, control-plane gap 2); no CORS (none needed with D9) |
| Engine `core/runtime/internal/engine` (Go, `internal`) | `engine.New()`, `(*Engine).Run(ctx, *flow.Flow, *docs.Set, *Process) (*Result, error)`; `Result{Returned, Failed, Stop, Trace}`; `StepTrace{StepID, Kind, In, Out, Failed, Dropped}` appended per step visit (`interp.go`), returned also on error/cancel | nothing outside the engine reads `Trace`; `returnDocuments` sets `Out` to the number of returned documents |
| `nurtuo flow run` (PR #51, `core/runtime/cmd/nurtuo/flowrun.go`) | `flowRun(args, stdout, stderr, reg)`: `loadFlow` (`flow.Parse`, which runs `ValidateJSON` itself), `loadConnections`, `crossCheck`, `applyHostKeys` (`--known-hosts` > inline `knownHosts` > `~/.ssh/known_hosts`; file-level `insecure` without `--insecure-host-key` = exit 2), `checkLiterals`, `chainProvider{secrets.NewEnv()}` (plus `secrets.NewDir(...)` only with `--secrets-dir`), `connhost.NewManager(reg, &connections.Resolver{...})`, `Prepare`/`Commit`, `eng.Registry.RegisterConnectorLookup(mgr)`, one injected document with `--set` properties, `--timeout`, `writeOutput` `--json` envelope `{status: 200, documents: [{id, properties, payload(base64)}]}`, JSON errors on stderr `{error: {code, message}}`, exit codes 0/1/2/3, `Redactor` on all stderr | per-step status in the envelope (D16); a timeout is code `execution` with message `timed out after <d>`; a Stop is code `stopped` with message `flow stopped with status N[: msg]`; host-key and password failures are raised by `connhost.Manager.Prepare` (strict connection test) before the engine runs; not merged |
| Connections and secrets (runtime) | `nurtuo.connection/v1` (`connections.Parse`), `secret://<name>` (name `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}`), providers `env` (`secrets.EnvVar(name)` returns `NURTUO_SECRET_` + the upper-cased name with other characters mapped to `_`), `dir`, `encrypted-file`; `CheckLiterals`; `Redactor` | per-workspace secret scoping (a documented non-goal of that spec); nothing in the control plane |
| SFTP connector `connectors/sftp` | settings `host`, `port`, `user`, `password`, `privateKey`, `passphrase`, `hostKeyPolicy` (`known_hosts` default, `fingerprint`, `insecure`), `knownHosts` (content), `hostKeyFingerprint` (compared with `ssh.FingerprintSHA256`), `rootPath`, `maxListEntries`, `timeoutMs`; operations `QUERY` (list a directory), `GET`, `CREATE`, `UPSERT`, `DELETE`, `EXECUTE`/`RENAME` (the engine upper-cases the operation and maps any other name to `EXECUTE`); `GET` emits `<input document id>.out` (`flow-run.out` under `nurtuo flow run`) whose properties are the input properties plus `path`, `filename`; the fingerprint callback's error is `host key mismatch: got <fp>, want <fp>` (`config.go`) | a way to read a server's host key without trusting it (needed for TOFU, D4) |
| Standalone `core/runtime/cmd/nurtuo/standalone.go` | the Go binary supervises the Node control plane as a child (`--control-plane-cmd node`, `controlPlaneEnv(dsn, creds, port, gatewayAddr)`) | it does not tell the control plane where the `nurtuo` binary is |
| Playwright | `core/web/e2e/canvas.spec.ts` (10 tests, Vite on `127.0.0.1:5199`, not in CI on `main`); on PR #51/#53: `core/web/e2e/flow-run-export.spec.ts` builds the SFTP flow, exports it and, when `NURTUO_FLOW_RUN_BIN` is set, runs it with the binary; PR #53 adds the `web-e2e` job (Node 24, Playwright chromium, builds `nurtuo`, starts `atmoz/sftp@sha256:c5fb9e4b…` on a random loopback port with a random password masked by `::add-mask::`, writes `known_hosts` with `ssh-keyscan`, exports `NURTUO_SECRET_SFTP_PASSWORD`) | a test that drives Run in the canvas against a control plane |

### Drift found while designing

| Path | What the doc says | What the code does | Handling |
|---|---|---|---|
| Brief for this spec; [ARCHITECTURE.md](../ARCHITECTURE.md) §6.3 ("test mode" as a trigger of the engine) | test run "through the in-process engine in the control plane" | the control plane is Node (`core/control-plane/package.json`, `src/*.ts`); the engine is Go under `core/runtime/internal/`, importable only from `core/runtime/` | design against the code: the control plane spawns `nurtuo flow run` (D7) |
| [canvas-editor](implemented/2026-11-21-canvas-editor.md) | `@nurtuo/client` dependency "is not imported"; canvas "does not talk to the control plane" | same on `main` (`core/web/src`, no `fetch`) | none; this spec adds the first network calls |
| [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md) | per-workspace secret scoping is a non-goal; secrets are resolved by the runtime | same; the control plane has no secret code | this spec adds a control-plane store (D3) without changing the runtime providers |
| [workspace-isolation](2026-11-22-workspace-isolation.md) | path-scoped aliases `/v1/workspaces/{ws}/...` are "optional later" (phase 3) | path-scoped routes exist only for members/tokens/audit | new resources start path-scoped (D8); consistent with its direction, no conflict |
| [control-plane-api-and-data](implemented/2026-11-21-control-plane-api-and-data.md) gap 2 | no body limit | confirmed in `createHttpServer` (`api.ts`) | Phase 2 adds a limit (D2) |
| [nurtuo flow run](2026-11-23-nurtuoctl-run.md) (PR #50, merged) | `--json` envelope `{status, documents}` | matches `writeOutput` on PR #51 | extended additively (D16) |

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
                                                        env: <EnvVar(name)>=…, HOME=tmp      --connection tmp/<id>.connection.json …
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
| Secrets | env / `--secrets-dir` of the user | workspace `SecretStore`, passed as `secrets.EnvVar(name)` variables in a minimal env (D12) |
| Host key | `--known-hosts`, inline, `~/.ssh/known_hosts`, `--insecure-host-key` | inline `fingerprint`/`knownHosts` from the connection only; `HOME` is the temp dir so the `~/.ssh` fallback finds nothing; `--insecure-host-key` is never passed |
| Output | raw bytes or `--json` | always `--json`, mapped to the API response |

### Phase 1: per-step status from the CLI (D16)

In `core/runtime/cmd/nurtuo/flowrun.go` (after PR #51 merges), `writeOutput` adds
`"steps": [{"id", "kind", "in", "out", "failed", "dropped"}]`, built from `res.Trace` and aggregated
by `StepID` in first-seen order (a step visited twice by two branches sums its counters). The JSON
error object on stderr gains the same `steps` when `eng.Run` was reached (failed documents, stop,
timeout, cancel). Errors before the engine (usage, validation, `Prepare`: host-key mismatch, wrong password, missing
secret) carry no `steps`. Purely additive; raw (non-`--json`) output is unchanged. No other CLI field
is added (D16). The `nurtuo flow run` spec
([2026-11-23-nurtuoctl-run.md](2026-11-23-nurtuoctl-run.md), on `main`) gets a changelog line in the
implementation PR; this spec does not edit it.

### Phase 2: connections and secrets in the control plane

New module `core/control-plane/src/connections/` (D3, D6, D8):

- `ConnectionRepository` with `MemoryConnectionRepository` and `PgConnectionRepository`. Every
  method takes `workspaceId` first (the rule the workspace-isolation spec proposes for all
  repositories). Postgres migration `0300_workspace_connections.sql` (number chosen to stay
  clear of `0100` auth and `0200` workspace isolation):
  `connections(workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE, id text NOT NULL, connector text NOT NULL, settings jsonb NOT NULL, created_at, updated_at, PRIMARY KEY (workspace_id, id))`.
  Keyed by `workspace.id`, not slug (today `PATCH /v1/workspaces/{ws}` changes only `name`, so slugs are stable, but the id is the durable key).
- `SecretStore` interface `{ put(ws, name, value), has(ws, name), list(ws), delete(ws, name), reveal(ws, name) }`;
  `MemorySecretStore` only in this slice (a `Map<workspaceId, Map<name, Buffer>>`). `reveal` is
  called only by `TestRunner`. The `workspace.delete` handler purges the workspace's connections
  (memory repository; Pg cascades) and secrets, so nothing outlives the workspace (tested). Values are never serialised into responses, logs or audit details.
- Validation on `PUT .../connections/{id}`, mirroring the Go rules so the user gets the error at save
  time (the child re-validates anyway): `id` matches `^[a-z][a-z0-9_-]*$`; `connector` is a
  registered connector (`sftp` only in this slice, D11); fields marked `writeOnly` /
  `x-nurtuo-secret` in `connectors/sftp/schemas/connection.json` (password, privateKey, passphrase) must be `secret://<name>`
  references (literal -> 400 `literal_secret`); `hostKeyPolicy: insecure` -> 400
  `insecure_host_key_not_allowed`; `known_hosts` without non-empty `knownHosts`, or `fingerprint`
  without `hostKeyFingerprint` -> 400 `host_key_required`. The schema JSON is copied into `dist` by the control-plane build
  (`@nurtuo/control-plane` ships only `dist`, `migrations`, `openapi.yaml`), with a test that the copy
  equals `connectors/sftp/schemas/connection.json`; a missing secret is allowed at save (shown as `missing`).
- Secret names from the canvas are derived as `<connectionId>-<field>` (for example
  `rebex-demo-password`); `PUT .../secrets/{name}` refuses a name whose `EnvVar` form equals that of
  another secret in the workspace (409 `secret_name_collision`, D12). `envVar(name)` is a TypeScript
  port of Go `secrets.EnvVar` **including** the `NURTUO_SECRET_` prefix, pinned by a shared fixture
  (`a-b`, `a_b`, `A.b`, digits) that is also run against the Go function.
- Request body limits follow the operation profiles in PR #92. Bounded operations return 413
  `payload_too_large` before JSON parsing when their limits are exceeded. Test runs have a 1 MiB
  request limit (D2), and revision uploads retain their existing defaults.
- Routes are auth-module endpoints so they receive `EndpointCtx.workspace`: `createAuthHook` gains
  an option `extraEndpoints?: Record<string, Endpoint>` merged into the `endpoints` lookup
  (`auth/middleware.ts`), and `main.ts` passes the connection, secret and test-run endpoints. When
  `auth: "disabled"` these routes are not reachable, by design (D9): the legacy `route()` today
  answers 405 `method_not_allowed` for any `/v1/workspaces/...` path, so `route()` gets an explicit
  404 `not_found` for paths under `/v1/workspaces/` (`api.test.ts` case). Both are fail-closed.
- Abort plumbing: `ApiRequest` and `EndpointCtx` gain `signal?: AbortSignal`, set in
  `createHttpServer` from the request/response `close` event, so `TestRunner` can kill the child when
  the client disconnects.
- `ROLE_DESCRIPTIONS.editor` is updated to mention connections, secrets and test runs.

### Phase 3: the test-run endpoint

New module `core/control-plane/src/testrun/` (D1, D2, D7, D10, D11, D12):

`POST /v1/workspaces/{ws}/test-runs`, request:

```json
{
  "flow": { "schema": "nurtuo.flow/v1", "id": "sftp-download", "steps": [ ... ], "edges": { ... } },
  "input": { "properties": { "path": "/readme.txt" } },
  "timeoutMs": 30000
}
```

`TestRunner.run(ws, request, signal)`:

1. Limits: test-run body ≤ 1 MiB, `timeoutMs` in 1000..120000 (default 30000), at most 64 input
   properties, keys `^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`, values ≤ 4 KiB. Concurrency semaphore 2 per
   workspace, 4 per instance -> 429 `too_many_runs` with `retry-after: 5`.
2. Validate `flow` with `validateFlow` from `@nurtuo/flow-validate` (the same library as the canvas);
   errors -> 400 `invalid_flow` with the diagnostics. D11: every `connector` step must be `sftp`
   with operation `GET` or `QUERY`, compared after `toUpperCase()` as the engine does (`get` works;
   `RENAME`, `DELETE`, `CREATE`, `UPSERT`, `UPDATE` and unknown names do not) -> otherwise 400
   `not_allowed_in_test_run` naming the step. Every connector step must name a `connection`
   explicitly -> otherwise 400 `connection_required` (the CLI's "only connection of that connector"
   default is not used, because the control plane writes only the referenced connections).
3. Resolve each referenced connection id in workspace `ws` (`ConnectionRepository.get(ws, id)`);
   missing -> 404 `connection_not_found` naming the id (the same for an id of another workspace).
   Collect the `secret://` names; a name not in `SecretStore` for `ws` -> 409 `secret_missing` naming
   the secret and connection, never a value.
4. Egress check (D11): resolve `settings.host` (`dns.lookup`, all addresses); refuse `0.0.0.0/8`,
   `127.0.0.0/8`, `::1`, RFC 1918, `100.64.0.0/10`, `169.254.0.0/16` (including `169.254.169.254`),
   `fc00::/7` (including `fd00:ec2::254`), `fe80::/10` and IPv4-mapped `::ffff:0:0/96` (checked
   against the mapped IPv4 address) unless `NURTUO_TEST_RUN_ALLOW_PRIVATE=1` ->
   400 `host_not_allowed`. Best effort: the child resolves again (DNS rebinding, see Gaps #3).
5. Create `mkdtemp(<os tmp>/nurtuo-testrun-)` (0700); write `flow.json` and one
   `<id>.connection.json` per connection (0600, non-secret content only).
6. Spawn `NURTUO_FLOW_RUN_BIN` with argv `flow run <tmp>/flow.json --json --timeout <timeoutMs>ms`
   plus `--connection` per file and `--set k=v` per input property (argv array, no shell). The
   environment is built from scratch: `PATH`, `HOME=<tmp>`, `TMPDIR=<tmp>`, and
   `<envVar(name)>=<value>` per referenced secret, where `envVar` already includes the
   `NURTUO_SECRET_` prefix (never prefix it twice). Nothing else is inherited.
7. Read stdout up to 1.5 MiB (base64 of 1 MiB plus envelope) and stderr up to 64 KiB; beyond the
   stdout limit kill the child and report `output_too_large`. `TMPDIR=<tmp>` only moves the child's
   spool, so the runner also polls the size of `<tmp>` (every 500 ms) and kills the child with
   `output_too_large` above 64 MiB; the residual memory risk is in Gaps #7. Kill (SIGTERM, then SIGKILL after 5 s)
   when `timeoutMs + 5 s` passes or the HTTP client disconnects (the `nurtuo` side closes
   connections on SIGTERM, PR #51 `signal.NotifyContext`).
8. Map the CLI result (D16 adds only `steps`; everything else comes from the CLI's existing shapes):
   exit 0 -> `succeeded` with `documents` and `steps`; exit 1 or 2 -> `failed` with
   `error {code, message}` (from the CLI's JSON stderr, passed through `redact()` with the run's
   secret values as defence in depth) and `steps` when present; exit 3 (code `stopped`, message
   `flow stopped with status N[: msg]`) -> `stopped` with `stop {status, message}` parsed from that
   message (`status` omitted if the parse fails). A timeout is classified by the control plane: the
   run is `failed` with code `timeout` when the child was killed at its deadline or the CLI error
   message starts with `timed out after` (the CLI reports it as code `execution`). Step status per step id of the request
   flow: `ok` (in trace, `failed == 0`), `error` (`failed > 0`), `stopped` (the Stop step that ended a
   stopped run), `not_run` (absent from the trace; also the steps after a Stop). Errors raised
   before the engine runs, host-key mismatch and wrong password among them, come from
   `connhost.Manager.Prepare` as `connection "<id>" (sftp): test failed: ssh connect: …` and carry no
   `steps`; the runner extracts `<id>` from the message, marks the connector steps using that
   connection `error` and all others `not_run` (tests with fixed messages).
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
    { "id": "returndocuments-1", "kind": "returnDocuments", "status": "ok", "in": 1, "out": 1, "failed": 0, "dropped": 0 }
  ],
  "documents": [
    { "id": "flow-run.out", "properties": { "path": "/readme.txt", "filename": "readme.txt" }, "size": 379, "payload": "V2VsY29tZS4uLg==" }
  ]
}
```

Failed (host key changed; raised before the engine, so `steps` is derived as above):
`"status": "failed", "error": { "code": "execution", "message": "connection \"rebex-demo\" (sftp): test failed: ssh connect: … host key mismatch: got SHA256:…, want SHA256:…" }`.
Other API errors use the existing `{ "error": { "code", "message" } }` shape; 503
`test_run_unavailable` when `NURTUO_FLOW_RUN_BIN` is unset or not executable.

Binary location: `NURTUO_FLOW_RUN_BIN` (absolute path). `nurtuo standalone` sets it to
`os.Executable()` in `controlPlaneEnv` (`core/runtime/cmd/nurtuo/standalone.go`), so `make dev`
works without configuration. Audit: the endpoint returns
`audit: { after: { runId, status } }` so the audit `afterHash` never covers output data.

### Phase 4: canvas

New files in `core/web/src/` (D3, D4, D9, D15). This slice targets `vite dev` only (see Non-goals):

- `api/controlPlane.ts`: `fetch` wrapper with `credentials: "same-origin"`, `X-Nurtuo-Csrf` on
  mutations (token from the login response or, after a reload with a valid session cookie, from `GET /v1/auth/me`, which returns `csrfToken`; kept in memory only), base path
  `/v1/workspaces/{ws}`; typed with `@nurtuo/client` types regenerated from `openapi.yaml`
  (`sdk/ts/client` `gen` script). `vite.config.ts` gains `server.proxy: { "/v1": NURTUO_CP_URL ?? "http://127.0.0.1:8080" }`.
- `ui/LoginBar.tsx`: email/password login, logout, workspace picker (`GET /v1/workspaces`). Offline
  (no control plane) the canvas works exactly as today; Run and Connections are disabled with a
  hint.
- `ui/ConnectionsPanel.tsx`: list (`GET .../connections`), SFTP form generated with `fieldsOf` over
  `connectors/sftp/schemas/connection.json`, with `Field` extended by `writeOnly`; the form offers password authentication only in this slice (`privateKey`/`passphrase` are not offered; no `oneOf` handling) and filters `insecure` out of the `hostKeyPolicy` choices (`writeOnly` fields render as `type="password"`, never
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
  `run-ok` (green), `run-error` (red), `run-stopped` (amber), `run-not-run` (grey), with a text label as well as colour
  (accessibility, canvas-editor gap 3).

The flow JSON can never contain the password: `ConnectorConfig` has only `connection` (an id), and
the connection form writes passwords only to the secrets endpoint. "Save flow" output is unchanged.

### Phase 5: TOFU host-key scan (D4)

`POST /v1/workspaces/{ws}/connections/host-key-scan` `{ "host", "port" }` (permission
`connection:write`, same D11 egress check, and the same per-workspace and per-instance semaphore as
test runs, because #54 is open and any session user can own a workspace) spawns a new Go subcommand
`nurtuo flow host-key <host> [--port N] [--timeout 10s] --json`, which
dials with the connector's own `ssh.ClientConfig` defaults (the SFTP connector sets no
`HostKeyAlgorithms`, so the negotiated key type is the one a real run will see), records `key.Type()` and
`ssh.FingerprintSHA256(key)` in `HostKeyCallback` and then aborts the handshake (no authentication is attempted, no
credentials are involved), printing `{ "keys": [{ "type", "fingerprintSha256" }] }` with the negotiated key
first and marked as the one to trust; keys of other types, if shown at all, are informational. It lives in
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
| wrong password | 200 `failed`, code `execution`, raised before the engine (no CLI `steps`) | connector step red; message from the CLI (`Redactor`-scrubbed) |
| host key changed / wrong fingerprint | 200 `failed`, code `execution`, message `connection "<id>" (sftp): test failed: ssh connect: … host key mismatch: got …, want …`, no `steps` from the CLI | connector step red; hint "the server's key changed; verify before trusting the new key" (no one-click replace) |
| remote file missing | 200 `failed`, path named | connector step red |
| timeout | 200 `failed`, code `timeout` (classified by the control plane, see Phase 3 step 8) | steps that ran are coloured, the rest grey |
| a connector step without `connection` | 400 `connection_required` | error names the step |
| output too large | 200 `failed`, code `output_too_large` | "output larger than 1 MiB; use `nurtuo flow run`" |
| too many concurrent runs | 429 | "another run is in progress, retry" |
| binary missing | 503 `test_run_unavailable` | "test runs are not configured on this server" |
| flow ends in a Stop step | 200 `stopped`, `stop {status, message}` | the Stop step is `stopped`, later steps grey |
| control plane unreachable | network error | canvas keeps working offline; Run disabled |

### Rollback

Each phase is additive. Phase 1 adds JSON fields. Phases 2-5 add routes, a table, permissions and a
role-seed migration (and the explicit 404 in `route()`); rollback = remove the routes from `main.ts` (data in `connections` is inert);
the table can be dropped by a later migration. Phase 4 UI is inert without a control plane. The limits on the new endpoints roll back with their routes.

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
otherwise. The [compatibility contract](2026-11-22-backward-compatibility-contract.md) classes
`nurtuo` subcommands/flags and control-plane environment variables as STABLE; the new ones below are
EXPERIMENTAL by exception (the `nurtuo flow` subtree is already experimental per the `nurtuo flow run`
spec) and are labelled so in `docs/cli.md` and `docs/operations.md`.

| Contract | Change | Stability |
|---|---|---|
| `GET /v1/workspaces/{ws}/connections` | new; `{ items: [{ id, connector, settings, secrets: { <field>: "set" \| "missing" }, updatedAt }] }`, secret fields shown only as `secret://<name>` | experimental |
| `GET\|PUT\|DELETE /v1/workspaces/{ws}/connections/{id}` | new; PUT body `{ connector, settings }`; 400 `literal_secret`, `insecure_host_key_not_allowed`, `host_key_required`, `not_allowed_in_test_run` | experimental |
| `GET /v1/workspaces/{ws}/secrets` | new; names and `updatedAt` only | experimental |
| `PUT\|DELETE /v1/workspaces/{ws}/secrets/{name}` | new; PUT body `{ value }`, 204, value never returned; 409 `secret_name_collision` | experimental |
| `POST /v1/workspaces/{ws}/connections/host-key-scan` | new (Phase 5) | experimental |
| `POST /v1/workspaces/{ws}/test-runs` | new; request and response as in Design | experimental |
| Request body limit | operation-specific profiles in PR #92; 413 `payload_too_large` above the selected limit; no new default ceiling on revision uploads | new behaviour |
| Permissions | new resources `connection` (`read`, `write`, `delete`), `secret` (`write`, `delete`), `test_run` (`execute`) in `auth/permissions.ts`; role seed migration `0104_auth_seed_connection_permissions.sql` | experimental |
| Environment | `NURTUO_FLOW_RUN_BIN`, `NURTUO_TEST_RUN_ALLOW_PRIVATE` (control plane) | experimental (by exception to STABLE env vars) |
| `nurtuo flow run --json` | additive `steps` in the envelope and the JSON error (D16) | experimental (as PR #51) |
| `nurtuo flow host-key` | new subcommand (Phase 5) | experimental |
| `nurtuo.flow/v1`, `nurtuo.connection/v1`, `secret://` | unchanged | — |

`AGENTS.md` (PR #49) lists `core/control-plane/openapi.yaml` and CLI flags as "Ask First"; approval
of this spec is that request.

## Compatibility

- REST: the [backward-compatibility contract](2026-11-22-backward-compatibility-contract.md)
  treats `/v1/` paths as STABLE and response fields as ADDITIVE-ONLY. Revision uploads and full
  readback retain their existing default behavior under the operation-specific profiles in PR #92.
- Roles: built-in roles gain permissions per D6 (`runtime` gains none). Editors lack `member:manage`,
  so `canGrant` is unchanged in practice (it matters only for owner and admin). The `0104` seed keeps
  `permissions.ts` and SQL in sync (drift test, with its permission regex widened for `test_run`).
- `nurtuo flow run --json`: additive keys; consumers must ignore unknown fields.
- Canvas: offline behaviour unchanged; "Save flow" output unchanged.
- Downgrade: the `connections` table and permissions remain; an older control plane ignores them.

- **D9 status code change (404 instead of 405).** Before this change the legacy `route()` answered
  405 `method_not_allowed` for any `/v1/workspaces/<id>/...` path when the auth module did not serve
  it, which is the case under `NURTUO_INSECURE_NO_AUTH=1`. Those paths now answer 404 `not_found`
  (the bare `/v1/workspaces` and every other resource keep their previous answers). A client that
  relied on 405 for these dev-only paths sees 404; with auth enabled nothing changes. Covered by
  `api.test.ts` "HTTP without auth returns 404 for workspace ID paths".

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
  (existing middleware). Routes answer 404 under `NURTUO_INSECURE_NO_AUTH=1`.
- **Workspace isolation (D8).** New rows carry `workspace_id`; every repository/store call takes
  it; ids from another workspace answer exactly like missing ones; the per-run temp dir contains
  only that run's files; no caching of connections or secrets across runs.
- **Egress / SSRF (D11).** `sftp` only, `GET`/`QUERY` only, private ranges refused by default (full
  list in Phase 3 step 4); the host-key scan uses the same check and the same semaphore (#54 is open).
  Remaining risk: DNS rebinding (Gaps #3).
- **Input limits.** Body 1 MiB, properties 64 × 4 KiB, timeout ≤ 120 s, output ≤ 1 MiB,
  concurrency 2/4; spool size cap 64 MiB; argv passed as an array (no shell).
- **Platform.** POSIX control-plane hosts only in this slice (Non-goals); on Windows test runs answer 503.
- **Output rendering.** Previews are React text nodes; downloads use `Blob`; no `innerHTML`.
- **Audit.** `test_run.execute`, `connection.*`, `secret.*` are audited with hashes of non-secret
  summaries only.

## Step plan

Phases are independently shippable stories; each step leaves the system working and names its
verification. Prerequisites: PR #51 merged (Phases 1, 3, 5); PR #53 merged (Phase 6). Phase 2 has no
prerequisite. PR #50 (the `nurtuo flow run` spec) is already on `main`.

### Phase 1: per-step status in `nurtuo flow run --json` (Go, `core/runtime`)

1. Add `steps` (aggregated `res.Trace`) to the `--json` success envelope. Verify:
   `TestFlowRunJSONSteps`; existing `TestFlowRunJSONEnvelope` still green.
2. Add `steps` to the JSON error object when the engine ran (failed documents, stop, timeout).
   Verify: `TestFlowRunJSONErrorSteps`, `TestFlowRunJSONNoStepsBeforeEngine`.
3. `docs/cli.md` note and a changelog line in the `nurtuo flow run` spec. Verify:
   `node scripts/check-docs-links.mjs`.

### Phase 2: workspace connections and secrets API (control plane)

4. Permissions `connection:*`, `secret:*`, `test_run:execute` in `auth/permissions.ts` and
   `migrations/0104_auth_seed_connection_permissions.sql`. Widen the permission regex in the drift test (`auth/crypto.test.ts` matches
   `[a-z]+` for the resource, which rejects `test_run`) and update `ROLE_DESCRIPTIONS.editor`. Verify: the drift test
   "migration 0103 seed matches permissions.ts" extended to read `0103` + `0104`,
   `pg-auth.test.ts` migration list, `rbac.test.ts` role × route matrix extended with an assertion that
   `runtime` has no `connection:*`, `secret:*` or `test_run:*` permission.
5. Request body limit and `signal` plumbing (`ApiRequest`, `EndpointCtx`) in `createHttpServer`;
   explicit 404 for `/v1/workspaces/...` in `route()`. Verify: `api.test.ts` checks each operation’s request limit
   before parsing, no-auth workspace 404s and signal cancellation on disconnect.
6. `ConnectionRepository` (memory + Pg) with migration `0300_workspace_connections.sql`. Verify:
   `connections/repository.test.ts` (memory), `connections/pg-repository.test.ts` (`DATABASE_URL`-gated).
7. `MemorySecretStore` and the `extraEndpoints` seam in `createAuthHook`. Workspace delete purges
   connections and secrets. Verify:
   `connections/secrets.test.ts`, `connections/workspace-delete.test.ts`, `auth/authn.test.ts` case "extra endpoints receive the resolved workspace".
8. Connection and secret routes, `openapi.yaml`, `POLICIES`. Verify: `policy.test.ts` (every
   operation has a policy, 401/403 documented), `connections/routes.test.ts`, `connections/isolation.test.ts`.
9. Copy `connectors/sftp/schemas/connection.json` into `dist` at build. `@nurtuo/client` types regenerated (`pnpm --filter @nurtuo/client run gen`) and a changeset added (`pnpm changeset`; the package is Apache-2.0 and consumed by users). Verify: `gen:check`, the schema-copy equality test, `pnpm changeset status`.

### Phase 3: test-run endpoint (control plane)

10. `TestRunner` with a fake binary fixture: temp dir, argv, minimal env, limits, timeout/kill,
    mapping. Verify: `testrun/runner.test.ts`.
11. Route `POST /v1/workspaces/{ws}/test-runs`, D11 policy, `connection_required`, egress check, concurrency. Verify:
    `testrun/routes.test.ts`, `testrun/egress.test.ts`, `testrun/isolation.test.ts`.
12. `NURTUO_FLOW_RUN_BIN` from `nurtuo standalone` (`controlPlaneEnv`). Verify:
    `TestControlPlaneEnvFlowRunBin` in `core/runtime/cmd/nurtuo/standalone_test.go`.
13. Real-binary integration, gated. Verify: `testrun/runner.real.test.ts` with `NURTUO_FLOW_RUN_BIN`
    and `NURTUO_TEST_SFTP_*`.

### Phase 4: canvas connections, Run and results (paste host key)

14. `api/controlPlane.ts` (CSRF restored from `/v1/auth/me` on start), Vite proxy, `LoginBar`. Verify: `src/api/controlPlane.test.ts` (Vitest),
    `e2e/canvas-run-ui.spec.ts` "login and workspace picker" (mocked API).
15. `ConnectionsPanel` with SFTP form, secret write, paste host key; connection select in
    `ConfigDialog`. Verify: `canvas-run-ui.spec.ts` "create connection never sends the password to
    the connection endpoint".
16. Run button, Run dialog, `runState.ts`, results panel, node colouring, invalidation on edit.
    Verify: `src/model/runState.test.ts`, `canvas-run-ui.spec.ts` run cases; existing
    `canvas.spec.ts` unchanged and green.

### Phase 5: TOFU host-key scan

17. `nurtuo flow host-key` (negotiated key first). Verify: `TestFlowHostKeyScan` (in-process SSH server; a server with several host keys reports the one the connector's defaults negotiate).
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
- WHEN the server's host key does not match the stored fingerprint THEN the run is `failed` (code
  `execution`, raised before the engine, so without CLI `steps`), the connector steps using that
  connection are `error` and the others `not_run`, and the message shows both fingerprints; the
  stored fingerprint is not changed. A wrong password behaves the same way.
- WHEN the flow ends in a Stop step THEN the run is `stopped` with `stop.status`, the Stop step is
  `stopped` and later steps `not_run`.
- WHEN "Fetch host key" is used THEN the fingerprint of the key the connector would negotiate is
  displayed and nothing is stored until "Trust this key" is pressed; a run against that server then
  succeeds.
- WHEN a user of workspace A sends a connection id, secret name or test run naming a connection
  that exists only in workspace B THEN the answer is the same as for a non-existent id (404) and
  nothing of B is read or returned.
- WHEN a viewer or deployer calls `POST .../test-runs` or `PUT .../secrets/{name}` THEN 403; WHEN an
  API token calls them THEN 403 (`userOnly`).
- WHEN `NURTUO_INSECURE_NO_AUTH=1` THEN the new routes answer 404 (not 405).
- WHEN the role is `runtime` THEN it holds no `connection:*`, `secret:*` or `test_run:*` permission.
- WHEN a workspace is deleted THEN its connections and secrets are gone.
- WHEN the flow has a non-`sftp` connector or an operation other than `GET`/`QUERY` (compared
  case-insensitively; `RENAME`, `DELETE`, `CREATE`, `UPSERT`, `UPDATE` are refused) THEN 400
  `not_allowed_in_test_run` and no process is spawned.
- WHEN a connector step has no `connection` THEN 400 `connection_required` and no process is spawned.
- WHEN the host resolves to a loopback/private/link-local address and
  `NURTUO_TEST_RUN_ALLOW_PRIVATE` is not `1` THEN 400 `host_not_allowed` and no process is spawned.
- WHEN a run exceeds its timeout THEN the child is killed, the temp dir is removed, the response is
  `failed` with code `timeout` (classified by the control plane) and the steps that ran keep their status.
- WHEN the output exceeds 1 MiB, or the child's temp dir exceeds 64 MiB, THEN the run is `failed`
  with `output_too_large` and the child is killed.
- WHEN the HTTP client disconnects during a run THEN the child is killed and the temp dir removed.
- WHEN a run ends (any outcome) THEN its temp dir no longer exists and the child has exited.
- WHEN a third concurrent run starts in one workspace THEN 429.
- WHEN the flow is edited after a run THEN all run colours and results are cleared.
- WHEN the canvas has no control plane THEN it behaves as today and Run is disabled.
- WHEN `nurtuo flow run --json` succeeds THEN the envelope has `steps` with one entry per executed
  step id; raw output is unchanged.
- WHEN the control plane restarts with `DATABASE_URL` set THEN connections remain and secrets are
  reported `missing` (memory backend), and a run returns 409 `secret_missing`; without `DATABASE_URL`
  (memory repository) connections are lost as well and the run returns 404 `connection_not_found`.
- WHEN `POST .../connections/host-key-scan` is called while the concurrency limit is reached THEN 429.

## Tests

Tests ship in the same PR as the code of each phase. Names are proposed; none exist yet.
Integration and e2e tests create their own fixtures (temp dirs, generated keys, containers, random
passwords) and clean them up. No password is committed anywhere.

### Go (`core/runtime`) — `cd core/runtime && GOWORK=off go test -race ./cmd/nurtuo/...`

| Test | File | Covers |
|---|---|---|
| `TestFlowRunJSONSteps` | `cmd/nurtuo/flowrun_test.go` | stub connector; `steps` ids/kinds/counters; a step visited twice is aggregated |
| `TestFlowRunJSONErrorSteps` | same | failed document and stop: JSON error carries `steps` with `failed` counts |
| `TestFlowRunJSONNoStepsBeforeEngine` | same | validation / `Prepare` errors (host-key mismatch, wrong password) have no `steps` |
| `TestFlowRunErrorMessages` | same | pins the shapes the control plane parses: timeout is code `execution` with `timed out after <d>`; Stop is code `stopped` with `flow stopped with status N: msg`; `Prepare` failure names `connection "<id>"` |
| `TestEnvVarFixture` | `internal/secrets/env_test.go` | shared fixture (`a-b`, `a_b`, `A.b`, digits) that the TypeScript `envVar` test also reads |
| `TestFlowHostKeyScan` | `cmd/nurtuo/flowhostkey_test.go` | in-process SSH server (pattern of `connectors/sftp/sftp_test.go` `startServer`, copied): reports type and SHA-256 of the key negotiated with the connector's defaults, also when the server has several host keys; never authenticates; timeout on a silent port |
| `TestControlPlaneEnvFlowRunBin` | `cmd/nurtuo/standalone_test.go` | `controlPlaneEnv` contains `NURTUO_FLOW_RUN_BIN=<os.Executable()>` |

### Control plane (`core/control-plane`) — `pnpm --filter @nurtuo/control-plane run build && pnpm --filter @nurtuo/control-plane run test`

All handler tests drive `createHandler` in-process with `MemoryRepository`, `MemoryAuthRepository`
(`auth/test-util.ts`) and two workspaces populated.

| Test | File | Covers |
|---|---|---|
| connection CRUD, validation codes | `src/connections/routes.test.ts` | `literal_secret`, `insecure_host_key_not_allowed`, `host_key_required`, list shows `set`/`missing` |
| secrets write-only | `src/connections/secrets.test.ts` | PUT 204; GET lists names only; no route returns a value; `secret_name_collision` (`a-b` vs `a_b`); audit detail contains no value |
| isolation | `src/connections/isolation.test.ts`, `src/testrun/isolation.test.ts` | A cannot list/get/put/delete B's connections or secrets (404), cannot run with B's connection id (404), test-run with A's connection never reveals B's secrets (fake binary records env) |
| permissions | `src/auth/rbac.test.ts` (extended matrix) | viewer/deployer/runtime denied; tokens denied on `userOnly` routes; `runtime` lacks every `connection:*`, `secret:*` and `test_run:*` permission; `connection:read` held by owner, admin, editor, viewer, deployer; drift test (regex widened) sees the `0104` rows |
| policy/openapi | `src/auth/policy.test.ts` (existing) | every new operation has a policy and documents 401/403 |
| no-auth mode, limits, abort | `src/api.test.ts` | new routes 404 (not 405) with `auth: "disabled"`; operation-specific body limits return 413 and revision routes retain existing behavior; disconnect before the response completes aborts `ApiRequest.signal` |
| workspace delete | `src/connections/workspace-delete.test.ts` | deleting a workspace purges its connections and secrets (memory) |
| envVar port | `src/connections/env-var.test.ts` | TS `envVar` equals the Go fixture, prefix present exactly once; `secret_name_collision` |
| schema copy | `src/connections/schema.test.ts` | the `dist` copy of `connection.json` equals `connectors/sftp/schemas/connection.json` |
| runner | `src/testrun/runner.test.ts` | fake binary `testdata/fake-nurtuo.mjs` (a Node script selected by an env var that echoes argv/env, prints canned envelopes, sleeps, floods stdout, exits 1/2/3): argv array, minimal env (no `NURTUO_AUTH_SECRET`), `HOME=<tmp>`, timeout kill and `timeout` classification (CLI message `timed out after` and own deadline), client abort kill (via `signal`), stdout limit, spool-size kill (`<tmp>` over 64 MiB), temp dir removed in every case, status mapping incl. `stopped` and Stop-step colouring, `Prepare` failure naming a connection (no `steps`, connector steps `error`), `redact()` of echoed secret |
| egress and policy | `src/testrun/egress.test.ts` | each of `0.0.0.0/8`, `127.0.0.0/8`, `::1`, `10/8`, `172.16/12`, `192.168/16`, `100.64/10`, `169.254/16` (incl. `169.254.169.254`), `fc00::/7` (incl. `fd00:ec2::254`), `fe80::/10`, `::ffff:127.0.0.1` refused; allowed with `NURTUO_TEST_RUN_ALLOW_PRIVATE=1`; non-`sftp` connector and `RENAME`, `DELETE`, `CREATE`, `UPSERT`, `UPDATE` refused before spawn; `GET`, `get`, `Get`, `QUERY`, `query` accepted; missing `connection` is 400 `connection_required` |
| concurrency | `src/testrun/routes.test.ts` | third concurrent run in a workspace is 429; semaphore released on error; host-key scans count against the same limit |
| host-key scan | `src/testrun/hostkey.test.ts` | fake binary; egress check applies; nothing persisted |
| real binary | `src/testrun/runner.real.test.ts` | skipped unless `NURTUO_FLOW_RUN_BIN` and `NURTUO_TEST_SFTP_HOST/PORT/USER/PASSWORD/FINGERPRINT` are set (the `web-e2e` job sets them); GET returns the file; wrong password fails without leaking |
| Pg repository | `src/connections/pg-repository.test.ts` | skipped unless `DATABASE_URL`; migration `0300`, workspace-keyed queries, cascade on workspace delete |

### Canvas (`core/web`)

Unit — `pnpm --filter @nurtuo/web run test` (Vitest, node environment):

| Test | File | Covers |
|---|---|---|
| step status mapping and invalidation | `src/model/runState.test.ts` | `ok`/`error`/`not_run`/`stopped` (Stop step `stopped`, later steps `not_run`); cleared when the flow changes; aggregated by step id |
| client | `src/api/controlPlane.test.ts` | CSRF header on mutations only, token restored from `/v1/auth/me` after reload, workspace path, error mapping; password never in the connection body |
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
  masked), `_FINGERPRINT` (from `nurtuo flow host-key`, i.e. the key the connector negotiates; the server has ED25519, RSA and ECDSA keys and the connector's defaults prefer ECDSA, so `ssh-keyscan -t ed25519` would not match), `_EXPECT` (file
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
runs the motivating example against `test.rebex.net` (the file path `/readme.txt` is a non-blocking item to confirm when the smoke test is first run), fingerprint
via the TOFU flow compared against `NURTUO_E2E_REBEX_FINGERPRINT`.

### CI (`web-e2e`, PR #53, Phase 6)

Extend the job, not a new one: after "Start SFTP server…", add steps that build
`@nurtuo/control-plane`, compute the fingerprint with `nurtuo flow host-key` (not `ssh-keyscan -t ed25519`), generate the bootstrap password with
`openssl rand -hex 16` and `::add-mask::` it, and export `NURTUO_CANVAS_RUN_E2E=1`,
`NURTUO_TEST_RUN_ALLOW_PRIVATE=1`, `NURTUO_FLOW_RUN_BIN` (already exported by PR #53) and the
`NURTUO_E2E_SFTP_*` variables. Traces are uploaded on failure (existing step). Playwright traces can
contain typed form values; the e2e password is random per job and masked, and traces are kept 5 days
(PR #53), so no reusable secret is exposed.

### Docs

`node scripts/check-docs-links.mjs`; `docs/auth.md` (new permissions), `docs/security.md` (test-run
egress, secrets in the control plane), `docs/getting-started.md` (Run in the canvas), `docs/cli.md` and `docs/operations.md` (new flag/subcommand/env vars labelled EXPERIMENTAL), `openapi.yaml` (including 413).

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | An editor points a connection at `10.0.0.5:5432` or `169.254.169.254:80`; the control plane connects from inside the network (SSRF, port probing via error messages and timing) | high | security | D11: `sftp` only, private ranges refused by default; host-key scan uses the same check; document `NURTUO_TEST_RUN_ALLOW_PRIVATE` as dangerous |
| 2 | A flow with `sftp DELETE` run against production credentials deletes remote files | high | safety | D11: only `GET`/`QUERY`; mutating operations need a later decision (`--read-only` in the CLI was an improvement idea in the `nurtuo flow run` spec) |
| 3 | DNS rebinding: `evil.example` resolves to a public IP at check time and `127.0.0.1` when the child dials | medium | security | follow-up: pass the checked IP to the child (`host` replaced by the address, SNI not relevant for SSH) or an egress proxy; document as best effort |
| 4 | The control plane restarts; every secret in `MemorySecretStore` is gone; runs fail with `secret_missing` until users re-enter passwords | medium | UX | D3 accepts this for the slice; clear 409 message; persistent encrypted store in a follow-up spec |
| 5 | Several control-plane replicas: the secret is set on replica 1, the run hits replica 2 and fails with `secret_missing` | medium | operations | memory backend documented as single-replica; persistent backend before HA |
| 6 | Secrets are in the child's environment; another process of the same OS user can read `/proc/<pid>/environ` during the run | low | security | same user as the control plane, which holds the values anyway; runs are short; D12 alternative documented |
| 7 | A hostile or buggy flow produces 4 GiB of output; Node buffers it | medium | reliability | stdout cap 1.5 MiB then kill. `TMPDIR=<tmp>` only moves the child's `docs.Spool` files and does not bound them, so the runner polls the size of `<tmp>` and kills the child above 64 MiB; the child's `writeOutput` loads each returned payload into memory (`p.Bytes()`) before the stdout cap applies, a residual memory risk until a later additive CLI limit (`--max-output-bytes`, not part of D16) exists; temp dir removed after |
| 8 | The child hangs on a stalled SFTP server (SFTP gap 1: `timeoutMs` covers only connect) | medium | reliability | `--timeout` in the child plus SIGTERM/SIGKILL from the control plane; PR #51 made transfers cancellable |
| 9 | A bounded operation profile is incorrectly applied to a revision upload | medium | compatibility | retain the revision profile without a default byte ceiling and test uploads above 1 MiB (PR #92) |
| 10 | The `Secure` session cookie is not stored when the canvas is served over plain `http://` on a non-localhost address (for example a LAN IP in `make dev`) | medium | UX | document `127.0.0.1`/`localhost` or TLS; Chromium treats loopback as secure (non-blocking: confirm in Phase 4) |
| 11 | A user trusts a TOFU fingerprint without comparing it, on a network with a MITM; credentials are then sent to the attacker | medium | security | explicit wording, show the fingerprint prominently, never auto-trust; paste path for out-of-band fingerprints |
| 12 | The server rotates its host key; every run fails and users ask for an "ignore" switch | low | UX | message explains rotation; replacing the key is the same TOFU/paste flow with the old one shown; no insecure switch |
| 13 | Workspace creation is ungated until #54; an attacker creates a workspace and becomes owner there | low (for this feature) | authz | new data is workspace-keyed, so they see only their own empty workspace; #54 remains required for the rest of the control plane |
| 14 | The TS-side connection validation drifts from `connections.Parse`/`CheckLiterals` | low | correctness | the child re-validates every run (exit 2 -> 400-like `failed` with the CLI message); shared fixture test runs both validators |
| 15 | `Result.Trace` counts documents, not causes: a failed document's error cannot always be attributed to the step that failed it | low | UX | step with `failed > 0` is red; message from the CLI; per-step error messages are an engine follow-up |
| 16 | Starting a Go process per run costs tens of milliseconds and some memory; a burst of runs from many users exhausts the host | low | performance | concurrency 2/4 (D2); revisit a long-running service (Alternatives) if needed |
| 17 | PR #51 or #53 changes or is not merged | medium | delivery | Phases 1, 3, 5, 6 wait; Phase 2 and the mocked-API part of Phase 4 can ship independently |
| 18 | #54 is open: any session user can create a workspace, becomes `owner` and may trigger outbound connections (runs and host-key scans) | medium | security | accepted by D8; scans share the run semaphore (Phase 5); the egress check applies to both; #54 should land before the feature is enabled on a shared instance |
| 19 | The Gaps #3 mitigation "replace `host` by the checked IP" would break `known_hosts` hostname matching (fingerprint pinning, the only mode stored by this slice, is unaffected) | low | security | apply it for `fingerprint` connections only |
| 20 | Parsing the CLI's error messages (timeout, stop status, `connection "<id>"`) is brittle if PR #51's wording changes | low | correctness | fixed-message tests in `runner.test.ts` and a Go test in Phase 1 that pins the messages; a structured field is an additive CLI follow-up |
| 21 | Windows control-plane hosts | low | portability | out of scope (Non-goals); 503 on win32 |

## Improvement opportunities

- Persistent encrypted workspace secret store; secret rotation propagating to deployments.
- Run a saved component revision; run on a deployed runtime (`run_now`) after #56.
- Async runs with progress events per step; run history with retention.
- Per-step document inspector (input/output documents of each step).
- Structured `stop`, `timeout` and `connection` fields in the `nurtuo flow run --json` error, and `--max-output-bytes` (additive CLI follow-ups).
- A production serving topology for the canvas; Windows support for test runs.
- A generic connection form for every connector from its `schemas/connection.json`.
- `nurtuoctl test-run` using the same endpoint.

## References

- [ARCHITECTURE.md](../ARCHITECTURE.md) §6.3 and §11 (epic table, row 10), [getting-started.md](../getting-started.md), [auth.md](../auth.md), [security.md](../security.md), [connectors/sftp.md](../connectors/sftp.md)
- Specs: [canvas-editor](implemented/2026-11-21-canvas-editor.md), [connections-and-secrets](implemented/2026-11-21-connections-and-secrets.md), [connector-sftp](implemented/2026-11-21-connector-sftp.md), [control-plane-api-and-data](implemented/2026-11-21-control-plane-api-and-data.md), [engine-interpreter](implemented/2026-11-21-engine-interpreter.md), [flow-format-v1-and-validator](implemented/2026-11-21-flow-format-v1-and-validator.md), [auth-rbac-audit](implemented/2026-11-21-auth-rbac-audit.md), [workspace-isolation](2026-11-22-workspace-isolation.md), [backward-compatibility-contract](2026-11-22-backward-compatibility-contract.md); [nurtuo flow run](2026-11-23-nurtuoctl-run.md) (PR #50)
- PRs: https://github.com/nurtuo/nurtuo/pull/49 (AGENTS.md), https://github.com/nurtuo/nurtuo/pull/50, https://github.com/nurtuo/nurtuo/pull/51, https://github.com/nurtuo/nurtuo/pull/53
- Open Mercato: `integration-secret-credential-editing` (write-only credential fields), `tenant-ownership-and-module-acl-authorization`.

## Implementation Status

[#99](https://github.com/nurtuo/nurtuo/pull/99) adds canvas sign-in, session restoration, workspace
selection and logout. It also adds the permission matrix and migration `0104`, the no-auth 404
behavior and the HTTP disconnect signal.

The remaining work from [#90](https://github.com/nurtuo/nurtuo/pull/90) is tracked in
[#100](https://github.com/nurtuo/nurtuo/pull/100) (connections and secrets),
[#101](https://github.com/nurtuo/nurtuo/pull/101) (runtime support) and
[#102](https://github.com/nurtuo/nurtuo/pull/102) (test-run API, canvas and end-to-end tests).

## Changelog

- 2026-11-23: created as draft
- 2026-11-23: approved; decisions D1–D16 recorded; pre-implement findings F1–F15 resolved
