<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: MCP server (`nurtuo-mcp`) and AI evals

Status: implemented. Date: 2026-11-21.

## TLDR

`@nurtuo/mcp` exposes flows to AI agents over MCP (stdio or stateless Streamable HTTP): catalog,
build (`apply_ops` with the shared CommandBus), validate, diff and, only with `--allow-deploy`, a
two-step `deploy`. Every tool goes through one wrapper that enforces size limits, a timeout and
secret redaction. `@nurtuo/ai` grades agents against scenarios by reading the stored flow. The
deploy confirmation is a flag the agent sets itself, and the shipped binary has no `dry_run`
backend.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/33 (squash commit `c534511`) |
| ADRs | — |
| Code | `core/mcp/src/{server,tooling,guardrails,errors,store,control-plane-store,control-plane-client,http,stdio,cli,catalog,assist,resources,prompts,view,runtime}.ts`, `core/mcp/scripts/gen-connectors.mjs`, `core/ai/src/evals/*.ts`, `core/control-plane/src/mcp-store.test.ts` |
| User docs | [mcp.md](../../mcp.md) |

## Problem and goals

Agents should be able to build integration flows with the same operations and validator as the
canvas, without being able to leak credentials or change running systems on their own.

- Goals: tool parity with the canvas CommandBus; deterministic, structured errors; bounded inputs,
  outputs and time; secrets never stored in flows and never returned to the model; deploy off by
  default; evals that catch regressions in tools, validator or schema.
- Non-goals: hosting an LLM; per-user authorization inside the MCP server (it acts with one control
  plane token); executing flows (dry runs are delegated to a `DryRunner`).

## Design (as built)

### Server (`createServer(deps)` in `server.ts`)

`ServerDeps`: `store: FlowStore`, optional `runtime: DryRunner`, optional `deployer: Deployer`,
`allowDeploy`, `limits`. Tool groups (exported constants):

- `MCP_TOOLS_V1`: `search`, `get_flow`, `apply_ops`, `validate`, `dry_run`.
- `MCP_TOOLS_V2`: `list_step_kinds`, `get_step_schema`, `list_connectors`, `suggest_next_steps`,
  `explain_diagnostic`, `diff_flows`.
- `MCP_TOOLS_DEPLOY`: `list_runtimes`, `deployment_status` (registered when a `deployer` exists),
  `deploy` (registered only when `allowDeploy === true`).

`apply_ops` validates arguments with a zod discriminated union of the seven ops (`addStep`,
`removeStep`, `updateConfig`, `connect`, `disconnect`, `setLabel`, `renameStep`), refuses literal
secrets (`findLiteralSecrets`), applies them atomically with `applyOps` from `@nurtuo/flow-schema`,
enforces `maxSteps`, saves with optimistic concurrency (`saveFlow(next, base.revision)`, mapped to
`revision_conflict`), and returns diagnostics plus the semantic view. Incomplete flows are stored.

`deploy` refuses invalid flows, resolves the runtime, and returns a plan with
`requiresConfirmation: true` unless the call has `confirm: true`.

Resources (`resources.ts`): `nurtuo://flows/{flowId}`, `nurtuo://flows/{flowId}/summary`,
`nurtuo://catalog/step-kinds`, `nurtuo://catalog/connectors`. Prompts (`prompts.ts`):
`build-integration`, `fix-flow`, `explain-flow`.

The connector catalog `src/connectors.generated.ts` is generated from `connectors/*/connector.yaml`
by `scripts/gen-connectors.mjs` during `build`; a unit test asserts it is in sync.

### Guardrails (`tooling.ts`, `guardrails.ts`)

`makeRegistrar` wraps every handler: `checkInputSize` (256 KiB), `withTimeout` (30 s), error mapping
to `{error, code, …}` (codes in `errors.ts`), deep `redact()` of the result, then the output size
check (512 KiB). `maxOps` 200, `maxSteps` 500. Redaction masks values under keys matching
`SECRET_KEY` (password, secret, token, api_key, authorization, private_key, credentials, cookie, …)
unless the key ends in a descriptor suffix (`_name`, `_ref`, `_id`, …), and string values matching
PEM private keys, `Bearer`/`Basic` credentials, URL user-info passwords, `sk-`/`ghp_`/`xox?-`
tokens and AWS access key ids. `${secret:name}`, `{{secret.name}}`, `secret://…` and `$ENV`
references are kept. Tool annotations set `readOnlyHint`, `destructiveHint`, `idempotentHint`,
`openWorldHint`.

### Stores

- `InMemoryFlowStore` and `InMemoryDeployer` (one fake runtime `local`), the default.
- `ControlPlaneFlowStore` (`control-plane-store.ts`) maps a flow to a component of kind `flow`
  whose name is the flow id; each save is a new immutable revision. It re-checks the revision
  number after writing to detect a concurrent writer. HTTP through its own `ControlPlaneClient`.

### Transports and CLI

- `stdio.ts`: protocol on stdout, logs on stderr.
- `http.ts`: stateless Streamable HTTP, one server and transport per POST at `/mcp`, `GET /healthz`,
  1 MiB body cap, `405` for non-POST, DNS-rebinding guard (non-loopback `Origin` rejected when bound
  to loopback), optional bearer token compared with `timingSafeEqual`.
- `cli.ts` (`nurtuo-mcp`): `--store`, `--url`/`NURTUO_URL`, `--token`/`NURTUO_TOKEN`,
  `--transport`, `--port` (3333), `--host` (127.0.0.1), `--auth-token`/`NURTUO_MCP_TOKEN` (required
  for a non-loopback host), `--allow-deploy`, `--max-ops`, `--timeout-ms`. Usage errors exit 2.

### Evals (`core/ai/src/evals`)

`EvalSession` connects an MCP client in-process to a fresh server and store and records calls.
`Scenario` holds a goal and expectations (validates, step kinds, connector operations, start
config, edges, a start-to-end path, tool-call budget, no secrets). `runScenario(scenario, agent)`
grades the flow read from the store. Scenarios: `getEmployee`, `httpMapSftp`, `repairFlow`, each
with a reference op list in `SCENARIOS`. Agents: `ScriptedAgent` (replays op lists),
`SuggestionAgent` (follows `suggest_next_steps` only), `ToolLoopAgent(model, maxTurns = 25)` for a
real `ChatModel`.

## Contracts

| Contract | Stability |
|---|---|
| Tool names, input schemas and error codes (mcp.md) | experimental; agents and `.mcp.json` files depend on them |
| Resource URIs `nurtuo://flows/…`, `nurtuo://catalog/…` | experimental |
| `nurtuo-mcp` flags and env vars | experimental |
| `createServer(ServerDeps)`, `FlowStore`, `Deployer`, `DryRunner` | internal (package is `private`) |
| `evals` exports (`Scenario`, `runScenario`, `grade`, `ChatModel`, `ToolLoopAgent`) | experimental |

## Security

- Deploy is off by default: without `--allow-deploy` the `deploy` tool is not registered
  (`tools-v2.test.ts`, `transports.test.ts`).
- Literal credentials in ops are refused; outputs, resources and errors are redacted before they
  reach the agent; internal errors are returned as messages without stack traces.
- The HTTP transport binds to loopback by default and requires a bearer token otherwise; it does not
  terminate TLS.
- The MCP server holds one control plane token. Every agent connected to it acts as that token's
  user; the control plane audit log cannot tell which human or agent made a change.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `core/mcp/src/server.test.ts` (4) | tool list without deployer, GetEmployee build + validate + dry run (fake runner), atomic `apply_ops`, errors | `pnpm turbo run test --filter=@nurtuo/mcp` | |
| `core/mcp/src/tools-v2.test.ts` (13) | catalog tools, suggestions converge, explain, diff, guardrails, redaction, timeout, output limit, deploy gating and confirm, resources, prompts | same | |
| `core/mcp/src/guardrails.test.ts` (5) | `isSecretKey`, `redact`, `findLiteralSecrets`, `withTimeout`, catalog in sync with `connector.yaml` | same | |
| `core/mcp/src/control-plane-store.test.ts` (4) | bearer token, error mapping, revision races, flow id rules | same | fake `fetch` |
| `core/mcp/src/transports.test.ts` (6) | CLI parsing, deploy wiring, HTTP auth and origin, body limits, stdio bin end to end, usage exit 2 | same | |
| `core/control-plane/src/mcp-store.test.ts` | `ControlPlaneFlowStore` against the real control plane handler with `MemoryRepository` | `pnpm turbo run test --filter=@nurtuo/control-plane` | |
| `core/ai/src/evals/evals.test.ts` (8) | reference solutions, scenario structure, grader negatives, repair, suggestion agent, secrets, `ToolLoopAgent` with a stub model | `pnpm turbo run test --filter=@nurtuo/ai` | no real LLM in CI |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | With `--allow-deploy`, `confirm: true` is only an argument. An agent can call `deploy` with `confirm: true` on the first call, and a flow description or step label written by another user (returned by `get_flow`) can instruct it to do so. Nothing on the server proves a human approved. | high | MCP, authorization | Make the first call return a one-time `planId` bound to flow, revision and runtime; require it on the second call and, for production environments, require approval in the control plane (MCP elicitation or a pending deployment approved by a human). |
| 2 | `nurtuo-mcp` never wires a `DryRunner`: `depsFromConfig` passes no `runtime`, so `dry_run` always answers `unavailable` outside tests. Epic 11 ("AI agent builds GetEmployee and passes validation and dry run") is met only with the fake runner in `server.test.ts`. | high | MCP, M1 scope | Implement a `DryRunner` over the runtime (or the control plane test-mode endpoint) and add it to `depsFromConfig`; track in [m1-acceptance-review](../2026-11-22-m1-acceptance-review.md). |
| 3 | All MCP clients share the server's control plane token. In HTTP mode with `--auth-token`, every holder of that one bearer token can write every flow the control plane token can write, and the audit log shows only the token owner. | medium | MCP, audit | Pass the caller's own control plane token through (OAuth for MCP or a per-request header) and cap tool scopes by the caller's role (Open Mercato `ai-key-user-acl-ceiling`). |
| 4 | `withTimeout` rejects after 30 s but does not cancel the handler. A slow control plane write in `apply_ops` completes after the agent got `timeout`; the agent retries and gets `revision_conflict`, or a duplicate revision is created. | medium | MCP | Pass an `AbortSignal` into store calls and abort the request; report "may have been applied" on timeout. |
| 5 | Redaction is key- and pattern-based. A config field `"pwd": "hunter2"` or `"dsn": "Server=db;User Id=sa;Password=x"` (no URL syntax) is neither refused by `apply_ops` nor redacted in outputs. | medium | secrets | Add common aliases (`pwd`, `dsn`, `connection_string`, `client_secret` variants) and the `Password=` key-value form; long term, have flow-schema mark secret fields so redaction is schema-driven. |
| 6 | The HTTP transport has no TLS. Bound to `0.0.0.0` with `--auth-token`, the token and all flows travel in clear text unless an operator adds a reverse proxy. | medium | transport | Document "TLS terminates at the proxy" in mcp.md, or add `--tls-cert/--tls-key`. |
| 7 | MCP tool calls are not logged anywhere except stderr lines on errors. After an agent changes a flow there is no record of which tool calls produced which revision. | medium | audit | Log tool name, arguments digest, result code and resulting revision per call; with the control-plane store, send an audit header. |
| 8 | Evals run only scripted and stub agents in CI. A prompt or tool-description change that confuses real models (for example renaming `after` in `addStep`) passes CI. | low | AI quality | Nightly job running `ToolLoopAgent` with a real model on `SCENARIOS`, tracking pass rate and tool calls (mcp.md already recommends this). |
| 9 | `McpServer` is created with `version: "0.1.0"` while `package.json` says `0.0.0`; clients cannot tell server builds apart. | low | MCP | Read the version from `package.json` or inject it at build time. |
| 10 | `@nurtuo/mcp` has its own `ControlPlaneClient` instead of `@nurtuo/client` (no retries, separate error mapping). | low | duplication | See [ts-client-and-cli](2026-11-21-ts-client-and-cli.md) gap 4. |

## Improvement opportunities

- Expose `list_connections` (names only) so agents stop inventing connection names.
- Per-tool rate limits for agents that loop.
- Return `structuredContent` alongside the JSON text so clients do not have to parse strings.

## References

- Open Mercato: `mcp-tool-exposure-parity-and-audit`, `issue-3805-direct-tool-mutation-approval`,
  `ai-agents-agentic-loop-controls`, `ai-key-user-acl-ceiling`.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
