<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0005: Set-at-a-time execution with per-document status

Date: 2026-11-21. Status: accepted (as implemented in `core/runtime/internal/engine`).

**Context.** Integration flows move many documents (rows, files, messages). Row-at-a-time
interpretation makes batching connectors (one SQL transaction, one SFTP session) awkward, and a
single bad document should not stop the others. Deterministic results are needed for snapshot tests.

**Decision.** Documents at a step form a set, and the step processes the set as a whole.
Each document has a status `ok`, `error` or `dropped`. Single-output steps return a set; `decision`
and `route` partition it across ports; `branch` runs its paths one after another over clones of the
set (payloads are reference counted, not copied); `tryCatch` collects documents that fail on the
`try` path, optionally re-runs them (`retries`) from their state at entry, then sends them down
`catch` with the error in `nurtuo.error.*` properties. Errored documents outside any `tryCatch` go
to the result's failed set. Connector steps send the whole OK set to one `Execute` stream and map
the per-document results back (`SUCCESS`, `APPLICATION_ERROR`, `FAILURE` fail or pass the
document individually; a stream-level error fails the whole set). Payloads are streams that spill
to disk above a threshold, so large documents do not live in memory.

**Consequences.**

- Batching connectors are a natural fit, partial failure is first-class, and execution traces
  (`StepTrace`: in, out, failed, dropped per step, in execution order) are deterministic, which the
  snapshot tests use.
- Per-document and per-set semantics must be kept in mind by step authors (`docs.Apply` is the
  helper for per-document steps).
- `branch` is sequential, not parallel: simple and deterministic, at the cost of latency for
  independent paths.
- The "contract" for connectors, `DocumentResult` per input document, comes directly from this
  model (see [connector-authoring.md](../connector-authoring.md)).
