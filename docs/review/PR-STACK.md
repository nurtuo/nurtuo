# Review guide: the PR stack

Nurtuo's first milestone (M1) is delivered as a stack of draft PRs. Each PR is based on the previous
one (`pr/01-foundation` is based on `main`) and adds one reviewable area. The last PR
(`pr/19-review-guide`) has a tree identical to the verified milestone branch. Review in order;
merge in order (or squash the stack onto `main` once the last PR is approved).

Commands below run from the repository root. Go modules are tested with `GOWORK=off` from inside the
module directory; TypeScript with `pnpm turbo run ... --filter=<package>` after `pnpm install`.

Intermediate branches were **not** individually built or tested. Only the last branch was built
and tested as a whole (see "Verification"). Known breakage in the middle of the stack is listed
per PR.

## The stack

| # | Branch | Contains | Review / test |
|---|---|---|---|
| 01 | `pr/01-foundation` | `.gitattributes`, `.gitignore`, toolchain pins (`.mise.toml`), Makefile, CI workflow, `SECURITY.md`, docs link checker | read CI; `node scripts/check-docs-links.mjs` |
| 02 | `pr/02-contracts-v1` | `proto/` connector.v1 and gateway.v1, generated Go and TS code, `sdk/go` connector contract test suite, minimal connector example, `go.work` base | `cd sdk/go && GOWORK=off go test ./...` |
| 03 | `pr/03-flow-v1` | flow.v1 JSON schema, Go and TS types, CommandBus, validators, shared conformance corpus, reference example flows, `docs/flow-validation.md` (script step included) | `cd sdk/go && GOWORK=off go test ./flow/...`; `pnpm turbo run test --filter=@nurtuo/flow-schema --filter=@nurtuo/flow-validate` |
| 04 | `pr/04-bundles` | `core/deploy/bundle`: signed bundle pack/sign/verify, ADR 0006, `go.work` complete | `cd core/deploy/bundle && GOWORK=off go test -race ./...` |
| 05 | `pr/05-engine-core` | `core/runtime/internal/engine`: document spool, interpreter, maps (JSONata), step runners, script runner and wazero WASM sandbox, runtime `go.mod` | `cd core/runtime && GOWORK=off go test -race ./internal/engine/...` |
| 06 | `pr/06-queues-listeners` | QueueBackend, memory and NATS JetStream backends, DLQ advisory, listeners (HTTP), perf harness | `cd core/runtime && GOWORK=off go test -race ./internal/queue/... ./internal/listener/...`; perf: `NURTUO_PERF=1`, see `docs/perf.md` |
| 07 | `pr/07-runtime-client` | gateway client, supervisor, runtime config/deployer, connections and secrets (ADR 0010), `cmd/nurtuo` (run, service, standalone, secret) | `cd core/runtime && GOWORK=off go test -race ./internal/... ./cmd/...` (needs the connectors module, so full build from PR 13) |
| 08 | `pr/08-control-plane` | control plane: migrations, repositories, REST API, OpenAPI, gateway server, TEST-ONLY mTLS fixtures | `pnpm turbo run typecheck test --filter=@nurtuo/control-plane` (imports `src/auth`, so it type-checks only from PR 09) |
| 09 | `pr/09-auth-rbac` | authentication, RBAC, audit log (`src/auth`, migrations 0100+) | same as 08 |
| 10 | `pr/10-connector-http` | `connectors` module deps, HTTP connector with OpenAPI import | `cd connectors && GOWORK=off go test -race ./http/...` |
| 11 | `pr/11-connector-sftp` | SFTP connector | `... ./sftp/...` |
| 12 | `pr/12-connector-postgres` | PostgreSQL connector; the **last commit** makes `QUERY` run in a READ ONLY transaction | `... ./postgres/...`; live test needs `NURTUO_TEST_POSTGRES_URL` |
| 13 | `pr/13-connector-nats` | NATS connector | `... ./nats/...` |
| 14 | `pr/14-bridges` | bridge launcher (`connectors/bridge`), Python bridge SDK, JVM and .NET bridge SDKs | `... ./bridge/...` (Python tests need `python`); JVM and .NET are **unverified** |
| 15 | `pr/15-ts-clients-tools` | `sdk/ts/client`, `cli`, `core/mcp`, `core/ai` evals, control plane MCP store test | `pnpm turbo run typecheck test --filter=@nurtuo/client --filter=@nurtuo/cli --filter=@nurtuo/mcp --filter=@nurtuo/ai`  |
| 16 | `pr/16-canvas` | `core/web` canvas editor and Playwright e2e, `pnpm-lock.yaml` | `pnpm turbo run typecheck build test --filter=@nurtuo/web`; e2e: `pnpm --filter @nurtuo/web test:e2e` |
| 17 | `pr/17-e2e-interop` | runtime e2e tests and TS interop tests | `cd core/runtime && GOWORK=off go test -race ./internal/e2e/... ./internal/interop/...` |
| 18 | `pr/18-docs` | remaining docs: README, ARCHITECTURE, concepts, getting started, security, ADR 0001 | `node scripts/check-docs-links.mjs` |
| 19 | `pr/19-review-guide` | this file | read |

Commits inside a PR are grouped by sub-area (implementation and its tests together). Review fixes
made during development are folded into the area commits; they are not separate commits.

## Verification (whole tree, last branch)

- `go vet`, `golangci-lint run`, `go test -race -count=1 ./...` in `connectors` (`GOWORK=off`).
- `node scripts/check-docs-links.mjs`, `bash scripts/check-boundaries.sh`.
- `pnpm install --frozen-lockfile`, `pnpm turbo run typecheck build test --force`, and `go vet` +
  `go test -count=1 ./...` per Go module. The exact results are in the description of the last PR.

## Known gaps and unverified items

- **Java (JVM bridge) and .NET bridge**: written and reviewed but never built or run here. Treat as unverified.
- **Real PostgreSQL**: connector and control plane repositories were exercised against fakes; the control plane repository and standalone
  mode use embedded PostgreSQL in their tests where available. The live `QUERY` read-only test
  (`NURTUO_TEST_POSTGRES_URL`) was not run against a real server.
- **Docker**: compose files, container images and Helm charts were not exercised.
- **CI jobs never run**: `.github/workflows/ci.yml` has not executed on GitHub Actions. Expect
  first-run fixes (tool versions, caching, the perf and Playwright jobs).
- **Script modules are not deliverable**: the script step runs inline WASM; shipping script
  modules in bundles is not implemented.
- **HTTP listener** routes use exact path match only (no path parameters or prefixes).
- **Bundle `FetchArtifact`** is not implemented.
- **Auth workspace partitioning is missing**: roles and tokens are not yet partitioned per workspace.
- **`QUERY` read-only** is a database transaction setting, a guard against accidental writes, not a
  security boundary: it does not limit reads, and write operations are unchanged. Use a dedicated
  least-privilege database account (see `docs/connectors/postgres.md`).
- **TEST-ONLY private keys** are committed under `core/control-plane/testdata/certs`. They are
  public, labelled in the README next to them, and must never be used outside tests.
- **Intermediate branches** may not build (examples: the control plane imports `src/auth` before PR 09;
  `core/runtime` needs the connectors module from PR 10 to PR 13 for its full build).
