<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: M1 acceptance review

Status: draft. Date: 2026-11-22.

## TLDR

ARCHITECTURE.md says "The M1 acceptance criteria in the reference (Part V §1) are the definition of
done", but `boomi-architecture-reference.md` is not in the repository, so nobody can check whether
M1 is done. This spec reconstructs testable criteria from the eleven M1 epics in ARCHITECTURE.md
§11 (plus §7.2 and §9 where an epic points there), records a first status for each against `main`
at `3d3d506`, and defines how the review is completed and kept.

## Status and links

| | |
|---|---|
| Pull requests | evidence: https://github.com/nurtuo/nurtuo/pull/19 to https://github.com/nurtuo/nurtuo/pull/37, https://github.com/nurtuo/nurtuo/pull/15, https://github.com/nurtuo/nurtuo/pull/16, https://github.com/nurtuo/nurtuo/pull/39 |
| ADRs | — |
| Code | whole repository |
| User docs | [ARCHITECTURE.md §10 and §11](../ARCHITECTURE.md), [PR stack review guide](../review/PR-STACK.md), [flow-validation.md](../flow-validation.md) (also notes the reference document is missing) |

## Problem and goals

The version policy depends on these criteria ("`v0.x` until the M1 acceptance criteria pass,
`v1.0.0` after", ARCHITECTURE.md §10). Without them there is no rule for when to cut 1.0, and the
gap list of each as-built spec has no common yardstick.

- Goals:
  1. A criteria list in the repository that does not depend on the missing reference document.
  2. Every criterion is a check someone can run (a command, a test name or a CI job).
  3. A status per criterion (met, partial, not met, to verify) with evidence, refreshed when M1
     work lands.
- Non-goals: recovering the exact text of the reference Part V §1; criteria for M2–M4.

## Design (proposed)

### Method

1. Ask the authors for `boomi-architecture-reference.md` Part V §1. If it can be published (it is
   a proposal document), add the relevant part under `docs/reference/` with its license header; if
   not, the list below becomes the authority and ARCHITECTURE.md §11 links to this spec instead of
   "reference Part V §1".
2. Derive one or more criteria from each epic's "Scope" column. Where the scope names a number (p99
   targets, "<10 min"), that number is the criterion.
3. For each criterion, name the evidence: test, CI job, command, or the as-built spec section that
   documents it. "Code exists" is not evidence; a passing test or a reproducible command is.
4. Record the status in the table below. A criterion is **met** only if its evidence runs in CI or
   is a documented manual command that a reviewer ran on the stated commit.
5. When every row is met (or explicitly moved to M2 with an ADR), M1 is accepted and `v1.0.0`
   may be tagged.

### Criteria and first status (`main` at `3d3d506`)

| # | Epic | Criterion | Evidence | Status |
|---|---|---|---|---|
| 1.1 | Repo foundation | CI green on `main` for all jobs | `ci` run 44958510575 on `3d3d506` success; `bridge-dotnet` is `continue-on-error` and failing | partial |
| 1.2 | Repo foundation | License scans with both allow-lists, boundary check | `licenses` job | met |
| 1.3 | Repo foundation | CLA required | `license/cla` status on pull requests | met (not required: no branch protection) |
| 1.4 | Repo foundation | Branch protection on `main` with required checks | `gh api repos/nurtuo/nurtuo/branches/main/protection` returns 404 | not met |
| 1.5 | Repo foundation | Release dry-run `make snapshot` | `release-snapshot` workflow | met |
| 2.1 | Contracts v1 | `connector.proto`, `gateway.proto` final, Go/TS codegen | `proto` job (`buf lint`, `format`, `breaking`) | met; open #41 is a breaking rename before 1.0 |
| 2.2 | Contracts v1 | Contract-test suite runs against every native connector | `sdk/go/connector/contracttest` | partial: `ExecuteLargePayload`, `CrashBehaviour`, `Listen` are `t.Skip` |
| 3.1 | Flow format | `flow.v1` schema, TS types, Go structs, CommandBus, validator, conformance corpus; TS and Go agree | `@nurtuo/flow-schema`, `@nurtuo/flow-validate` tests; `sdk/go/flow` tests | met (rules derived without the reference Part III §5, see flow-validation.md) |
| 4.1 | Engine core | Set-at-a-time interpreter, spill to disk, the listed steps, per-document status, Try/Catch, Process Call | `core/runtime/internal/engine` tests | to verify (engine spec owner) |
| 4.2 | Engine core | Every step kind accepted by the schema can run | `queue` steps fail with `no_runner` (flow-reference.md) | not met |
| 5.1 | Queues and listeners | Queue component with retention, retries, DLQ, ordering, concurrency; DLQ replay | `internal/queue/{memory,natsjs}` tests | to verify |
| 5.2 | Queues and listeners | Queue and HTTP listeners; pause/resume | `TestChain` (pause 503, resume 200) | partial: HTTP listener exact path match only; queue listener to verify |
| 5.3 | Queues and listeners | Benchmark harness with p99 targets in CI (pickup < 5 ms, overhead < 10 ms) | `perf` job `TestPerfBudget` | partial: runs per pull request; nightly `bench` job and the `test/bench` benchmarks (`b.Skip`) missing |
| 6.1 | Maps and scripts | JSON/XML profiles, maps compiled to JSONata/CEL | `internal/engine/maps`, `internal/engine/cel.go` tests | to verify |
| 6.2 | Maps and scripts | WASM script step | `internal/engine/script/wasm` tests, [scripts.md](../scripts.md) | partial: modules cannot be delivered in bundles |
| 7.1 | Native connectors | HTTP (+ OpenAPI import), SFTP, PostgreSQL, NATS pass the contract suite | `connectors/*` tests | partial: suite incomplete (2.2); NATS `Listen` not implemented; live PostgreSQL test gated by `NURTUO_TEST_POSTGRES_URL` and never run |
| 8.1 | Control plane and gateway | Postgres schema, REST API, signed bundles, deploy, outbound-only mTLS gateway, supervisor reconcile | control plane tests, `internal/e2e` | partial: Go↔TS interop tests skip in CI; `FetchArtifact` not implemented |
| 8.2 | Control plane and gateway | Telemetry to Postgres (standalone) | `core/runtime/internal/telemetry` is a package comment only | not met ([observability](2026-11-22-observability-logging-telemetry-health.md)) |
| 9.1 | Install and operations | `nurtuo standalone` with embedded Postgres and supervised control plane | `core/runtime/cmd/nurtuo/standalone*.go` tests | partial: the control plane executable is not packaged ([release-signing-and-installers](2026-11-22-release-signing-and-installers.md)) |
| 9.2 | Install and operations | `nurtuo service install` on Windows, Linux, macOS | `service_test.go`; operations.md marks Windows Event Log "not verified" | to verify |
| 9.3 | Install and operations | Packages | deb/rpm in snapshot only; no MSI/pkg | partial |
| 9.4 | Install and operations | e2e on Windows without Docker in under 10 minutes | no job | not met |
| 10.1 | Canvas | Palette, drop, move/snap, marquee, connect with validation, undo/redo, config dialogs from JSON Schema | `core/web/e2e/canvas.spec.ts` (not in CI) | partial |
| 10.2 | Canvas | Elbow routing, optimistic save, test mode with per-step colours | — | not met ([canvas-editor](implemented/2026-11-21-canvas-editor.md)) |
| 11.1 | MCP server v1 | `search`, `get_flow`, `apply_ops`, `validate`, `dry_run` | `core/mcp` tests | met for the tool surface |
| 11.2 | MCP server v1 | An AI agent builds GetEmployee and passes validation and dry run | `core/ai` evals (scripted agents), `server.test.ts` (fake runner) | partial: `nurtuo-mcp` has no `DryRunner`; no real-model run recorded |
| X.1 | Testing strategy (§9) | E2E: standalone install → deploy GetEmployee via API and via MCP → call it | — | not met |
| X.2 | Testing strategy (§9) | Performance: 1 GB file through Map with bounded memory; canvas 250 steps at 60 fps | — | not met |

Totals (28 rows): 6 met, 11 partial, 7 not met, 4 to verify.

### Keeping it current

- This spec moves to `implemented/` when every row is met or deferred by ADR.
- Each pull request that changes a row's status updates the row and the changelog.
- The "to verify" rows are assigned to the owners of the matching as-built specs (engine,
  queues, maps, runtime CLI) to confirm with a named test.

## Contracts

None. The criteria list becomes the release gate for `v1.0.0` (ARCHITECTURE.md §10).

## Compatibility

ARCHITECTURE.md §11's last sentence changes from "reference (Part V §1)" to a link to this spec.

## Security

Criteria with security impact (1.4 branch protection, 8.1 mTLS identity, 11.2 deploy gating) are not
waived by deferral without an ADR.

## Testing (acceptance)

| Check | Acceptance criterion |
|---|---|
| Review completeness | every row has a status other than "to verify" and names a test, job or command |
| Evidence reproducibility | a second reviewer reruns the evidence for all "met" rows on the same commit and gets the same result |
| Docs | ARCHITECTURE.md §11 no longer references a file that is not in the repository; `node scripts/check-docs-links.mjs` passes |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The reconstructed list misses a criterion that the reference Part V §1 contains (for example a specific connector or a security requirement); `v1.0.0` ships without it. | high | scope | Step 1 of the method: obtain the reference; until then label the list "reconstructed". |
| 2 | "To verify" rows are marked met on the basis of code existing, without a test. | medium | process | Rule 4 of the method; reviewer reruns evidence. |
| 3 | The list grows into a second backlog and is never "done". | medium | process | Only criteria derived from §11 scope text; new ideas go to M2 specs. |

## Improvement opportunities

- A GitHub milestone `M1 Core product` with one issue per "not met" row, linked here.

## References

- Open Mercato: no direct equivalent (process spec).
- As-built specs: [repo-foundation-and-ci](implemented/2026-11-21-repo-foundation-and-ci.md),
  [canvas-editor](implemented/2026-11-21-canvas-editor.md),
  [mcp-server-and-ai-evals](implemented/2026-11-21-mcp-server-and-ai-evals.md),
  [e2e-and-interop-testing](implemented/2026-11-21-e2e-and-interop-testing.md); the runtime,
  control plane and connector specs are written in parallel.

## Changelog

- 2026-11-22: created as draft with a first status at `3d3d506`.
