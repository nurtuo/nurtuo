<!-- SPDX-License-Identifier: Apache-2.0 -->
# Nurtuo MCP server

`nurtuo-mcp` lets an AI agent (Claude Desktop, Claude Code, any MCP client) discover connectors and
step kinds, build flows through the same CommandBus ops as the canvas, validate them with the shared
validator, dry-run them, and, only if you allow it, deploy them. Package: `core/mcp` (`@nurtuo/mcp`).

## Running it

```
nurtuo-mcp [--store memory|control-plane] [--url URL] [--token TOKEN]
           [--transport stdio|http] [--port 3333] [--host 127.0.0.1] [--auth-token TOKEN]
           [--allow-deploy] [--max-ops 200] [--timeout-ms 30000]
```

| Option | Meaning |
|---|---|
| `--store memory` | Flows live in the process (lost on exit). Good for trying things out. Default when no `--url`. |
| `--store control-plane --url ... --token ...` | Flows are components of kind `flow` in the control plane; every save is an immutable revision. Env: `NURTUO_URL`, `NURTUO_TOKEN`. |
| `--transport stdio` | Default. stdout carries the protocol; logs go to stderr. |
| `--transport http` | Streamable HTTP at `http://host:port/mcp` (stateless, JSON responses), `GET /healthz`. Binds to loopback; a non-loopback `--host` requires `--auth-token` (clients send `Authorization: Bearer ...`). Env: `NURTUO_MCP_TOKEN`. |
| `--allow-deploy` | Registers the `deploy` tool. Without it, agents can read and write flows but never deploy. |

Build and run from the repo:

```
pnpm turbo run build --filter=@nurtuo/mcp
node core/mcp/dist/stdio.js --store memory
```

### Claude Desktop

`claude_desktop_config.json` (Settings, Developer, Edit Config):

```json
{
  "mcpServers": {
    "nurtuo": {
      "command": "node",
      "args": ["/path/to/nurtuo/core/mcp/dist/stdio.js", "--store", "control-plane", "--url", "http://localhost:8080"],
      "env": { "NURTUO_TOKEN": "<control plane token>" }
    }
  }
}
```

Try it without a control plane by dropping the `--store`/`--url` arguments (in-memory store).

### Claude Code

```
claude mcp add nurtuo -e NURTUO_TOKEN=<token> -- node /path/to/nurtuo/core/mcp/dist/stdio.js --store control-plane --url http://localhost:8080
```

or a project-scoped `.mcp.json`:

```json
{
  "mcpServers": {
    "nurtuo": {
      "command": "node",
      "args": ["core/mcp/dist/stdio.js", "--store", "control-plane", "--url", "http://localhost:8080"],
      "env": { "NURTUO_TOKEN": "${NURTUO_TOKEN}" }
    }
  }
}
```

Remote (HTTP) server:

```
nurtuo-mcp --transport http --port 3333 --store control-plane --url http://cp:8080 --token "$NURTUO_TOKEN"
claude mcp add --transport http nurtuo http://127.0.0.1:3333/mcp
```

With `--auth-token`, add `--header "Authorization: Bearer <token>"` to `claude mcp add`.

## Tools

| Tool | Purpose |
|---|---|
| `search` | Find flows and components by text. |
| `get_flow` | Semantic view of a flow: steps, ports, where each port leads. |
| `apply_ops` | Atomic CommandBus ops (`addStep`, `removeStep`, `updateConfig`, `connect`, `disconnect`, `setLabel`, `renameStep`). `create` makes a missing flow. Returns diagnostics; an incomplete flow is stored. |
| `validate` | Shared validator diagnostics. |
| `dry_run` | Run a valid flow with sample input via the runtime (needs a runtime to be configured). |
| `list_step_kinds`, `get_step_schema` | Step kinds, ports, per-kind config JSON Schema (taken from `flow.v1.schema.json`) and an example step. |
| `list_connectors` | The four native connectors (http, nats, postgres, sftp): operations (the `operation` value for a connector step), connection fields, options. |
| `suggest_next_steps` | Open ports with candidate steps and ready-to-send `addStep` ops, plus fixes for current errors. Heuristic; example configs are placeholders. |
| `explain_diagnostic` | What a validator code means and how to fix it; with `flowId` and `stepId` it adds concrete ops. |
| `diff_flows` | Structural diff of two flows or two revisions. |
| `list_runtimes`, `deployment_status` | Available when a deployer is configured (control-plane store, or the memory store's fake deployer). Read only. |
| `deploy` | Only with `--allow-deploy`. Refuses invalid flows. The first call returns the plan; deployment happens only when called again with `confirm: true`, which the agent must set only after the human approved. |

Resources: `nurtuo://flows/{flowId}` (flow JSON), `nurtuo://flows/{flowId}/summary` (text),
`nurtuo://catalog/step-kinds`, `nurtuo://catalog/connectors`.

Prompts: `build-integration` (goal, source, target, flowId: the guided workflow), `fix-flow`, `explain-flow`.

### Typical agent session

1. `list_connectors`, `list_step_kinds` (or the `build-integration` prompt).
2. `apply_ops` with `create`, in a few batches.
3. Read `diagnostics`; use `suggest_next_steps` / `explain_diagnostic` until `valid` is true.
4. `dry_run`, then summarise. Deploy only on request, via `deploy`, with human approval.

## Guardrails

| Guardrail | Default | Result |
|---|---|---|
| Ops per `apply_ops` | 200 | `limit_exceeded` (`limit: "maxOps"`) |
| Steps per flow | 500 | `limit_exceeded` (`maxSteps`) |
| Tool input size | 256 KiB | `limit_exceeded` (`maxInputBytes`) |
| Tool output size | 512 KiB | `limit_exceeded` (`maxOutputBytes`) |
| Per-call timeout | 30 s | `timeout` |
| Literal credentials in ops (secret-looking keys or values) | refused | `secret_in_input` with the offending paths |
| Secrets in outputs (flows, diffs, resources, backend data) | redacted | values become `[REDACTED]`; `${secret:name}` style references are kept |
| Deploy | off | tool not registered without `--allow-deploy`; needs `confirm` |

Library embedders override limits via `createServer({ store, limits: { maxOps: 50 } })`. The HTTP
transport additionally caps request bodies (1 MiB) and rejects browser origins that are not loopback.

Errors are structured: `{ "error": "<message>", "code": "<code>", ... }`. Codes: `not_found`,
`invalid_input`, `limit_exceeded`, `secret_in_input`, `revision_conflict`, `timeout`,
`deploy_disabled`, `invalid_flow`, `unavailable`, `unauthorized`, `backend_error`, `internal`.
`apply_ops` op failures keep `failedOpIndex` and the CommandBus code (e.g. `step_not_found`).
Arguments that fail schema validation are rejected by the MCP SDK before the tool runs.

## Control-plane store

Mapping: flow id = component name (`^[a-z0-9][a-z0-9._-]*$`), kind `flow`; each `apply_ops` creates an
immutable revision whose number becomes `flow.revision`. `saveFlow(flow, expected)` compares
`expected` with the latest revision, and again with the number the server assigned, so a concurrent
writer produces `revision_conflict` instead of a silent overwrite (re-read with `get_flow` and
re-apply). Deploys call `POST /v1/deployments` with the revision number.

## Evals (`core/ai`)

`@nurtuo/ai` ships a deterministic eval layer (`evals`):

* `EvalSession`: an MCP client connected in-process to a fresh server and store; records every call.
* `Scenario`: a goal plus expectations on the final flow (validates, step kinds, connector operations,
  start config, edges, a start-to-end path, tool-call budget, no secrets).
* `runScenario(scenario, agent)`: runs the agent, then grades the flow read from the store, not the
  agent's claims.
* Scenarios: GetEmployee, HTTP to Map to SFTP, and repair of an invalid flow. `SCENARIOS` pairs each
  with a reference op list. Scripted agents replay those lists in CI, so any change that breaks the
  tools, the validator or the schema shows up as a failing scenario.

### Plugging an LLM in

Implement `ChatModel` for your provider and hand it to `ToolLoopAgent`; scenarios, session and grader
stay the same:

```ts
import { evals } from "@nurtuo/ai";

const model: evals.ChatModel = {
  name: "my-model",
  // messages: user prompt (the rendered build-integration prompt), assistant turns, tool results.
  // tools: MCP tool specs; `inputSchema` is JSON Schema, pass it as your provider's tool schema.
  async next(messages, tools) {
    const reply = await provider.chat(toProviderMessages(messages), tools.map(toProviderTool));
    return { text: reply.text, toolCalls: reply.toolCalls?.map((c) => ({ id: c.id, name: c.name, args: c.input })) };
  },
};

for (const { scenario } of evals.SCENARIOS) {
  const result = await evals.runScenario(scenario, new evals.ToolLoopAgent(model, 25));
  console.log(evals.formatReport([result]));
}
```

Run LLM evals outside the default `test` task (cost, flakiness): sample several runs per scenario and
track the pass rate and tool calls per scenario over time. `grade()` is exported for custom checks.
