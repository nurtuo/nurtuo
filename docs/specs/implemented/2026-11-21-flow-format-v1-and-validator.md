<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Flow format `nurtuo.flow/v1` and validator

Status: implemented (written as built). Date: 2026-11-21.

## TLDR

A flow is one JSON document (`nurtuo.flow/v1`): a list of typed steps and an `edges` map keyed by
`"stepId:port"`. One JSON Schema is the source of truth, and two small validators (TypeScript and
Go) must produce identical diagnostics, enforced by a shared conformance corpus. The validator
checks structure and graph rules only; it does not know what the runtime can execute, so a valid
flow can still fail at run time (see Gaps).

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/21 (squash commit `e0bf106`); schema extension for listener auth in review in https://github.com/nurtuo/nurtuo/pull/42 |
| ADRs | [ADR 0002](../../adr/0002-edges-as-step-port-map.md), [ADR 0003](../../adr/0003-cycles-are-errors.md), [ADR 0004](../../adr/0004-shared-schema-interpreter-and-corpus.md) |
| Issues | — |
| Code | `sdk/ts/flow-schema/`, `sdk/ts/flow-validate/`, `sdk/go/flow/`, `scripts/gen-flow-reference.mjs`, `examples/flows/` |
| User docs | [flow-reference.md](../../flow-reference.md), [flow-validation.md](../../flow-validation.md), [concepts.md](../../concepts.md) |

## Problem and goals

The canvas (browser), the MCP server and CLI (Node) and the engine (Go) all read and edit the same
flow. They must agree on what a valid flow is, down to the error code and JSON Pointer of each
problem, so that an AI agent or a user sees the same diagnostic everywhere.

- Goals: one versioned document format; one schema; byte-identical diagnostics (severity, code,
  path, stepId) in TS and Go; validation that never throws on hostile input; a command API
  (`CommandBus`) for safe edits; generated reference docs that cannot drift from the schema.
- Non-goals: checking that referenced connectors, connections, scripts or called flows exist;
  checking that the runtime has a runner for every step kind; expression (CEL, JSONata) syntax
  checking; loops inside a flow (ADR 0003); canvas layout (not part of the document).

## Design (as built)

**Document.** Top level `schema` (const `nurtuo.flow/v1`), `id`, `name`, optional `description`,
integer `revision`, `steps[]` and `edges{}`. A step is `{id, kind, label?, config}`. Fifteen kinds
(`flow.Kind*` constants in `sdk/go/flow/flow.go`, `STEP_KINDS` in `sdk/ts/flow-schema/src/types.ts`):
`start`, `connector`, `queue`, `map`, `script`, `message`, `setProperties`, `decision`, `branch`,
`route`, `tryCatch`, `processCall`, `notify`, `stop`, `returnDocuments`. Each kind has a
`$defs/<kind>Config` with `additionalProperties: false`.

**Ports and edges (ADR 0002).** Ports come from the kind and config: `flow.PortsOf` (Go) and
`portsOf` (`sdk/ts/flow-validate/src/index.ts`): `default` for single-output kinds, `true`/`false`
for `decision`, `try`/`catch` for `tryCatch`, `1`..`paths` for `branch` (2..25), each route id
then `default` for `route`, none for `stop` and `returnDocuments`. Because `edges` is an object,
one port has at most one outgoing edge by construction.

**Schema source and copies (ADR 0004).** `sdk/ts/flow-schema/schema/flow.v1.schema.json` (JSON
Schema 2020-12) is the source. `node sdk/ts/flow-schema/scripts/sync-schema.mjs` (package scripts
`sync`, `sync:check`, and part of `build`) writes `src/schema.generated.ts` and the Go copy
`sdk/go/flow/flow.v1.schema.json`, which is embedded with `go:embed` (`SchemaJSON()`).

**Validation, two stages.** `validateFlow(input)` (TS) and `flow.ValidateJSON(raw)` (Go):

1. Schema stage: a small interpreter for the keyword subset the schema uses
   (`sdk/ts/flow-validate/src/jsonschema.ts`, `sdk/go/flow/jsonschema.go`, kept in lockstep).
   Codes are `schema.<keyword>`; paths are JSON Pointers; `stepId` is filled when the path is
   under `/steps/<i>`.
2. Semantic stage, only if stage 1 found no errors (`semantic` in `sdk/go/flow/validate.go`):
   `step.duplicate_id`, `flow.start_count` (none or more than one), `edge.source_unknown`,
   `edge.port_invalid`, `edge.target_unknown`, `start.has_incoming`, `step.port_unconnected`,
   `route.reserved_id`, `route.duplicate_id`, `setProperties.duplicate_name` (per scope),
   `processCall.self_reference`, `flow.cycle` (every step that can reach itself) and the warning
   `step.unreachable`.

Output is de-duplicated and sorted by (path, code, stepId) (`finish`). `HasErrors`/`hasErrors`
ignore warnings. `flow.Parse(raw)` validates, then decodes into typed structs; it returns a nil
flow and an error if any diagnostic is an error. `Step.TypedConfig()` decodes `config` into the
kind's struct (`StartConfig`, `ConnectorConfig`, ...).

**Editing.** `applyOps(flow, ops)` and `CommandBus` (`sdk/ts/flow-schema/src/commands.ts`):
`addStep` (optionally splicing into an occupied port), `removeStep`, `updateConfig` (JSON merge
patch; `null` deletes a key), `connect`, `disconnect`, `renameStep` (rewrites edges), `setLabel`.
A batch is atomic and never mutates its input; `CommandBus` adds undo/redo and change
notification. The canvas and the MCP `apply_ops` tool use it.

**Reference docs.** `scripts/gen-flow-reference.mjs` generates `docs/flow-reference.md` from the
schema plus one example per kind in `examples/flows/reference/<kind>.flow.json`, with hand-written
"Runtime status" notes. `--check` fails when the file is stale; `--validate` also validates every
example with the built `@nurtuo/flow-validate`.

## Contracts

| Contract | Where | Stability |
|---|---|---|
| Document format `nurtuo.flow/v1` (fields, kinds, configs) | `flow.v1.schema.json` | stable: a breaking change needs `nurtuo.flow/v2` |
| Port names per kind | `PortsOf` / `portsOf`, [flow-validation.md](../../flow-validation.md#ports) | stable |
| Diagnostic codes and paths (`{severity, code, path, stepId}`) | conformance corpus | stable: tools and AI evals match on codes; messages are free text and not a contract |
| Go API `flow.Parse`, `ValidateJSON`, `HasErrors`, `PortsOf`, `SchemaJSON`, `Step.TypedConfig`, typed config structs | `sdk/go/flow` (Apache-2.0) | experimental (v0.x module) |
| TS API `validateFlow`, `hasErrors`, `portsOf`, `applyOps`, `CommandBus`, `flowSchema`, types | `@nurtuo/flow-validate`, `@nurtuo/flow-schema` (Apache-2.0) | experimental (0.0.0) |
| Corpus file format `{description, flow, expect[]}` | `sdk/ts/flow-schema/testdata/conformance/` | internal |

## Compatibility

The format is versioned by the `schema` constant. Additive, optional config fields (as PR #42 adds
`auth`, `tls`, `insecure` to `startConfig`) are compatible for readers that validate with the new
schema; an old validator rejects a document that uses them (`schema.additionalProperties`), so
the canvas, MCP server and runtime must be upgraded together. Any rule that makes a previously
valid flow invalid (for example making `flow.cycle` conditional, ADR 0003) is a contract change and
must update both validators and the corpus in the same pull request.

## Security

- The validators are pure functions with no I/O and no dependencies at run time; `validate.test.ts`
  ("validator never throws on hostile input") feeds non-objects, wrong types and deep junk.
- Size limits are not enforced by the validator itself; callers limit input (the MCP server caps
  steps per flow at 500, see [mcp.md](../../mcp.md); bundles cap size at 256 MiB).
- Duplicate JSON keys: `encoding/json` keeps the last value. The bundle packer rejects duplicate
  keys (ADR 0006), but `flow.Parse` on its own does not.
- Secrets: PR #42 (in review) adds `$defs/secretRef`, so listener secrets must be `secret://`
  references; a literal secret is a `schema.pattern` error.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestConformanceCorpus` in `sdk/go/flow/conformance_test.go` | every corpus file (9 valid, 48 invalid on `main`) gives exactly the expected diagnostics in Go | `cd sdk/go && GOWORK=off go test ./flow/...` | |
| `TestExamplesHaveNoErrors`, `TestReferenceExamples` (`reference_examples_test.go`) | `examples/flows/**` have no errors; one reference example per kind, no warnings | same | |
| `TestSchemaCopyInSync` | embedded Go schema equals the TS source | same | |
| `TestParseDecodesTypedFlow`, `TestParseRejectsInvalid`, `TestPortsOf` | `Parse` and port derivation | same | |
| `conformance.test.ts` (`sdk/ts/flow-validate/src`) | same corpus in TS; examples have no errors | `pnpm turbo run test --filter=@nurtuo/flow-validate` | runs on built `dist` |
| `validate.test.ts` | GetEmployee example, unknown edge target, `hasErrors`, `portsOf`, hostile input | same | |
| `schema.test.ts` (`sdk/ts/flow-schema/src`) | generated module and Go copy are byte-identical; kind list; Ajv 2020-12 agrees with the corpus on structural validity | `pnpm turbo run test --filter=@nurtuo/flow-schema` | Ajv is a dev dependency only |
| `commands.test.ts` | every `CommandBus` op, atomic batches, undo/redo | same | |
| `sync:check`, `gen-flow-reference.mjs --validate --check` | no stale schema copy or reference doc | `pnpm --filter @nurtuo/flow-schema run sync:check`; `node scripts/gen-flow-reference.mjs --validate --check` | the latter runs in the `ts` CI job |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | `startConfig.path` is an unconstrained string. A flow with `"path": "/a/{"` or `"/a/{id}/{id}"` passes both validators; at deploy the HTTP listener passes it to `http.ServeMux.HandleFunc`, which panics, and nothing recovers, so the runtime process exits. | high | schema, listener | add a `pattern` for `path` and a corpus case; validate the route in `NewHTTP`. Draft spec [http-listener-routing](../2026-11-22-http-listener-routing.md) |
| 2 | The schema accepts kinds and start types the runtime cannot execute: `queue` and `notify` steps fail every document with `no_runner`; `start.type` `schedule` and `manual` are never fired; `listener` with a non-`http` connector fails at deploy. The canvas shows such a flow as valid. | medium | validator, runtime | a runtime capability manifest and a warning code (for example `step.unsupported_by_runtime`). Draft spec [queue-step-runner](../2026-11-22-queue-step-runner.md) |
| 3 | The Go structs are hand-written and `json.Unmarshal` ignores unknown fields. If a new schema field (such as `auth` in PR #42) is added to the schema but not to `flow.StartConfig`, `Parse` succeeds and the engine silently runs without it; for `auth` that means an unauthenticated listener. The corpus compares diagnostics only, not decoding. | medium | `sdk/go/flow` | a test that decodes every valid corpus flow with `DisallowUnknownFields` into `TypedConfig()` and fails on a field the struct lacks |
| 4 | `processCall.flow` and `processCall.revision` are not checked against anything; a typo in the called flow id is only found when a document reaches the step (`processCall.resolve`), and the runtime ignores `revision` (see the [engine spec](2026-11-21-engine-interpreter.md)). | medium | validator, engine | cross-flow check in the control plane at deploy time |
| 5 | `uniqueItems` (used by `scriptConfig.capabilities`) is not in the interpreter subset, so `["clock","clock"]` is valid for nurtuo but invalid for Ajv. The Ajv cross-check passes only because no corpus file has a duplicate. Any other unsupported keyword added to the schema is silently ignored the same way. | low | validators | implement `uniqueItems` in both interpreters or drop it; a test that lists every keyword in the schema and fails on unsupported ones |
| 6 | The `setProperties` value is described as a CEL expression in the schema but rendered with the `{{operand}}` template mechanism; `decision`/`route` say CEL but the runtime uses `BasicEvaluator`. Authors write CEL that validates and then fails. | medium | schema docs, engine | fix descriptions or switch the runtime to CEL (see the [engine spec](2026-11-21-engine-interpreter.md), and [maps-and-expressions](2026-11-21-maps-and-expressions.md)) |
| 7 | The `lint` script of both packages is `echo 'lint: TODO (eslint)'`, so `pnpm turbo run lint` in CI checks nothing here. | low | tooling | quality-gates draft spec (other area) |

## Improvement opportunities

- Generate the Go structs and TS types from the schema, or at least test field parity both ways.
- Emit expression syntax diagnostics (CEL, JSONata) in the semantic stage so the canvas catches
  them before deploy.
- Publish the corpus as its own artifact so third-party editors can certify against it.

## References

- Open Mercato: `code-based-workflow-definitions` (definitions as code, versioned), and
  `subworkflow-explicit-ports-schema-builder` (explicit ports per node and a schema builder).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
