<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0004: One JSON Schema, a small interpreter in TS and Go, and a shared conformance corpus

Date: 2026-11-21. Status: accepted.

**Context.** The product promise is that the canvas, the MCP server, the CLI and the Go engine
validate a flow identically. The canvas runs in a browser, the engine is Go. General-purpose JSON
Schema libraries (Ajv in TS, various in Go) differ in error paths, ordering and edge cases, so the
same invalid flow would produce different diagnostics in different places.

**Decision.**

- The single source of truth is `sdk/ts/flow-schema/schema/flow.v1.schema.json` (JSON Schema
  2020-12). A sync script (`pnpm --filter @nurtuo/flow-schema run sync`, also part of `build`)
  generates the TS module `src/schema.generated.ts` and copies the file to
  `sdk/go/flow/flow.v1.schema.json` for `go:embed`. `sync:check` and tests fail when a copy is stale.
- Both validators contain a deliberately small interpreter for the keyword subset the schema uses
  (`$ref` to `$defs`, `type`, `const`, `enum`, `minLength`, `pattern`, `minimum`, `maximum`,
  `minItems`, `items`, `required`, `properties`, `additionalProperties`, `propertyNames`, `allOf`,
  `if`/`then`). The files `sdk/ts/flow-validate/src/jsonschema.ts` and `sdk/go/flow/jsonschema.go`
  are twins and must be kept in lockstep. Ajv is used only in a TS test to cross-check the
  interpreter against a real implementation.
- Validation has two stages. Stage 1 is the schema (codes `schema.<keyword>`, JSON Pointer paths).
  If it reports errors, stage 2 (semantic rules: ports, cycles, duplicate ids, ...) does not run.
  Output is de-duplicated and sorted by (path, code, stepId).
- A shared corpus `sdk/ts/flow-schema/testdata/conformance/{valid,invalid}` holds
  `{description, flow, expect:[{severity, code, path, stepId?}]}`; messages are not compared. Both
  validators run every file and must produce exactly the expected set. Adding a rule means adding
  corpus files first.
- Typed structs and TS types are hand-written, not generated; the corpus and tests (kind list,
  version) keep them consistent.

**Consequences.**

- Identical diagnostics (codes and paths) across the browser, Node and Go; the validator has no
  runtime dependencies and runs in a browser.
- Cost: two interpreters to maintain. A new schema keyword requires implementing it twice and
  adding corpus cases; unsupported keywords are silently ignored, so schema authors must stay inside
  the subset (the tests catch schema changes that need an unimplemented keyword only through the
  corpus).
- The corpus is also the executable specification of the rules in
  [flow-validation.md](../flow-validation.md).
- `docs/flow-reference.md` is generated from the same schema file
  (`scripts/gen-flow-reference.mjs --check`), so the docs cannot drift from it either.
