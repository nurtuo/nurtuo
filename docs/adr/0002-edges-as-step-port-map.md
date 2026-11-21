<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0002: Edges are a map keyed by "stepId:port"

Date: 2026-11-21. Status: accepted (decision as implemented in `flow.v1`).

**Context.** A flow is a graph of steps. Steps have different numbers of outputs (`decision` has two,
`branch` up to 25, `route` has one per route id, terminal steps have none). The canvas, the MCP
`apply_ops` tool and the engine all edit and read the same document, and AI agents need to address
connections by name. A list of `{from, port, to}` objects allows two edges from one port, which the
engine cannot execute, and needs extra rules to reject.

**Decision.** `edges` is a JSON object from `"<stepId>:<port>"` to the target step id. The key
pattern is `^[a-z][a-z0-9_-]*:[A-Za-z0-9_-]+$` (`flow.v1.schema.json`). Because object keys are
unique, a port has at most one outgoing edge by construction; fan-out is expressed with a `branch`
step. Ports are named by the step kind (`default`, `true`/`false`, `try`/`catch`, `1`..`paths`,
route ids). Every port of every step must have an edge (`step.port_unconnected`), so every path ends
in a terminal step. Layout (canvas positions) is not part of the flow document.

**Consequences.**

- A single flow document is compact and diffs cleanly; the CommandBus ops `connect` and
  `disconnect` take `(from, port, to)` and `(from, port)`.
- "Two edges from one port" cannot be represented, so no validation rule is needed for it. Fan-in
  is not restricted: many ports may point at the same target (merge).
- Duplicate JSON object keys would collapse to the last value in some parsers; documents that rely
  on it are not meaningful, and the bundle packer rejects duplicate keys outright (ADR 0006).
- Renaming a step rewrites every edge key and value (`renameStep`), and route ids double as port
  names, hence the reserved id `default`.
- The port set depends on the step config (`branch.paths`, `route.routes`), so changing the config
  can leave edges pointing at ports that no longer exist (`edge.port_invalid`); the validator, not
  the schema, reports that.
