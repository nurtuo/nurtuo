<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0003: Cycles in a flow are validation errors

Date: 2026-11-21. Status: accepted (M1 engine interprets a DAG).

**Context.** The engine is a set-at-a-time interpreter: a set of documents enters a step and the
step's outputs are walked recursively (`engine/interp.go`). Loops (for example "retry until ok" or
"page through results") would need loop state, termination bounds and a different execution model
for sets. None of the M1 use cases needs them: retries have `tryCatch` with `retries`, repetition
over many items is expressed by documents in a set, and reuse is `processCall`.

**Decision.** The validator reports `flow.cycle` as an error for every step that can reach itself
through edges (this includes self-loops). A flow with a cycle never reaches the engine. Nesting
through `processCall` is bounded separately: a flow may not call itself directly
(`processCall.self_reference`, error) and the engine refuses calls deeper than 16 levels
(`processCall.depth`). Mutual recursion between two flows is not detectable by the single-flow
validator and is caught at run time by the depth limit.

**Consequences.**

- The interpreter needs no cycle guard and no iteration limit inside a flow, and execution order
  and per-step traces are deterministic.
- Authors cannot model loops in one flow; they use `tryCatch.retries`, `processCall`, or schedule
  re-runs. If loops are added later, this is a contract change: `flow.cycle` becomes
  conditional, which affects the TS and Go validators and the conformance corpus together.
- The error is reported on every step in the cycle, not just one, so the canvas can highlight the
  whole loop.
- Unreachable steps are only a warning (`step.unreachable`); a cycle is always an error.

Corpus files: `sdk/ts/flow-schema/testdata/conformance/invalid/cycle.json`, `self-loop.json`,
`processcall-self.json`.
