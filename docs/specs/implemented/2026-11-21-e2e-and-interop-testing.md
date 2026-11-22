<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Runtime end-to-end and cross-language interop tests

Status: implemented. Date: 2026-11-21.

## TLDR

`core/runtime/internal/e2e` drives the whole runtime chain in one Go process (fake gateway with real
mTLS → `runtime.RunWith` → supervisor → deployer → HTTP listener → engine → HTTP connector →
`httptest` backend). `core/runtime/internal/interop` runs the real Go runtime against the real
TypeScript gateway in a Node child process. The interop tests skip when `core/control-plane/dist` is
missing, which is the case in CI, so cross-language compatibility is not checked automatically.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/35 (squash commit `411cf2e`), https://github.com/nurtuo/nurtuo/pull/39 (`e22ffab`, flaky TypeScript tests) |
| ADRs | — (exercises [ADR 0006](../../adr/0006-bundle-format-and-domain-separated-signature.md), [ADR 0009](../../adr/0009-outbound-only-runtime-gateway.md), [ADR 0010](../../adr/0010-connections-and-secrets.md)) |
| Code | `core/runtime/internal/e2e/{harness,chain,realwiring}_test.go`, `core/runtime/internal/interop/{harness,interop}_test.go`, `core/runtime/internal/interop/testdata/run-gateway.mjs`, `core/control-plane/testdata/certs` (TEST-ONLY keys) |
| User docs | [interop.md](../../interop.md), [ARCHITECTURE.md §9](../../ARCHITECTURE.md) |

## Problem and goals

Unit tests per package cannot show that the gateway protocol, bundle verification, connection
secrets and listeners work together, or that the Go runtime and the TypeScript control plane agree
on the wire.

- Goals: one-process end-to-end tests with no external services; a cross-language test of the
  gateway protocol, identity rules and signed-bundle path; deterministic waits instead of fixed
  sleeps.
- Non-goals: PostgreSQL-backed control plane, authentication on the REST API, installers and
  Windows services (epic 9 "e2e on Windows without Docker" is not covered), canvas e2e (see
  [canvas-editor](2026-11-21-canvas-editor.md)).

## Design (as built)

### `internal/e2e`

`harness_test.go` generates a CA and certificates at test time, starts a Go gRPC
`RuntimeGatewayService` fake over real TCP + mTLS, and runs `internal/runtime.RunWith`.

| Test | Scenario |
|---|---|
| `TestChain` (subtests `json-bundle`, `signed-bundle`) | desired state → deployment `running` → request through listener, engine and HTTP connector to the `httptest` backend; backend 500 surfaces as listener 500; `pause` command gives 503 and resume 200; an invalid flow reports an error status while the healthy deployment keeps serving; removing every deployment stops the listener (`waitRefused`, polled every 20 ms for 5 s) |
| `TestSignedBundleRejections` | in `trusted_keys` mode: a valid signature from an untrusted key, content that does not match the digest, tampered content re-addressed under its own hash, and a plain JSON bundle are all refused |
| `TestRealWiringHTTPConnector` | production wiring (no injected engine): the bundle carries a flow and a connection component; the bearer token comes from the env secret provider |
| `TestRealWiringTestModeWarnAndEncryptedFileSecrets` | `secrets.provider=encrypted-file` and `connection_test=warn` (a rejected token still deploys) |

### `internal/interop`

`requireNode` skips unless `node` is on `PATH` **and** `core/control-plane/dist/index.js` exists.
`startControlPlane` runs `testdata/run-gateway.mjs`, which imports the built control plane
(`MemoryRepository`, `Gateway`, `createGatewayServer`, `createHttpServer` with `auth: "disabled"`),
listens on random ports and prints `{"restPort","gatewayPort"}`. Certificates come from
`core/control-plane/testdata/certs`. The test steers the control plane through its REST API and
waits with `eventually(t, what, d, fn)`.

| Test | Scenario |
|---|---|
| `TestRuntimeAgainstTSGateway` | real Go runtime ↔ real TS gateway over mTLS; deploy through REST, runtime reports status, plain `<digest>.json` bundles |
| `TestWrongIdentityRejected` | a certificate for `rt-2` claiming `rt-1` gets `PermissionDenied`; the runtime keeps retrying and does not exit |
| `TestSignedBundleArtifactPath` | a signed bundle stored under its sha256 in `bundle_dir`, referenced by the revision digest, verified (digest + Ed25519) before running; stands in for `FetchArtifact` |
| `TestCertificateIdentities` | URI SAN `spiffe://nurtuo/runtime/<id>` wins over a decoy CN; a foreign-CA certificate is refused at TLS |

## Contracts

None public. The tests pin the gateway behaviour described in [interop.md](../../interop.md).

## Security

- TEST-ONLY private keys live in `core/control-plane/testdata/certs` (PR-STACK.md); the e2e harness
  generates its own keys per run.
- `run-gateway.mjs` disables REST authentication; auth is tested in the control plane package only.

## Testing

| Test | What it covers | How to run | Notes (skipped, env-gated) |
|---|---|---|---|
| `internal/e2e` (4 tests) | see above | `cd core/runtime && GOWORK=off go test -race ./internal/e2e/...` | no external services |
| `internal/interop` (4 tests) | see above | `pnpm turbo run build --filter=@nurtuo/control-plane...` then `cd core/runtime && GOWORK=off go test -race -v ./internal/interop/...` | skipped without `node` or without the control plane `dist` |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The CI `go` job does not build the control plane, so all four interop tests take the `t.Skip` branch (`ok …/internal/interop 1.027s` in the `main` run of `3d3d506`). A change to `core/control-plane/src/gateway.ts` or `proto/nurtuo/runtime/v1/gateway.proto` that breaks the Go runtime passes CI. | high | CI | Interop job that builds `@nurtuo/control-plane` first and sets `NURTUO_REQUIRE_INTEROP=1`, turning the skip into a failure ([quality-gates](../2026-11-22-quality-gates.md)). |
| 2 | The interop launcher uses `MemoryRepository` and `auth: "disabled"`. Outbox publishing, desired-state generations and reconnect against the PostgreSQL repository are never exercised end to end; a SQL bug in the outbox query only shows up in a real deployment. | medium | coverage | Second launcher mode with embedded PostgreSQL (already used by control plane repository tests). |
| 3 | No test installs anything: `nurtuo standalone`, `nurtuo service install` and the deb/rpm packages are not exercised, and the epic 9 target "e2e on Windows without Docker (<10 min)" has no job. | high | M1 scope | [m1-acceptance-review](../2026-11-22-m1-acceptance-review.md); a matrix job on `ubuntu-latest` and `windows-latest` that runs `standalone`, deploys GetEmployee via the API and calls it. |
| 4 | No end-to-end test covers "deploy via MCP": the MCP server is tested against an in-memory store and the control plane store test does not involve a runtime. | medium | coverage | Extend the interop test: `ControlPlaneFlowStore` + `deploy` with `confirm` → runtime reports `running`. |
| 5 | `TestWrongIdentityRejected` and `TestCertificateIdentities` assert the absence of an event after fixed `time.Sleep(500ms)` / `time.Sleep(300ms)`. On a slow runner the negative check can pass before the event would have happened, so a regression may go unnoticed. | low | test strength | Wait on a positive signal from the gateway (rejected-connection counter or log line) before asserting. |
| 6 | Interop and e2e tests start child processes and listeners; nothing collects the Node child's stderr on failure except the in-memory buffer, so a CI failure shows little context. | low | debuggability | Print `cp.stderr` in `t.Cleanup` on failure. |
| 7 | ARCHITECTURE.md §9 lists contract-suite cases (large payload streaming, crash behaviour), chaos tests and a 1 GB Map test; none exists (`contracttest.go` skips them with TODO). | medium | testing strategy | [quality-gates](../2026-11-22-quality-gates.md). |

## Improvement opportunities

- Property-based tests for bundle verification (random byte flips must always fail verification).
- Mutation testing on `internal/gateway` and `core/deploy/bundle` to measure how strong these tests are.

## References

- Open Mercato: `integration-testing-automation`, `agentic-property-based-testing`,
  `stryker-mutation-testing-ci-gate`.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
