<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Repository foundation and CI

Status: implemented. Date: 2026-11-21.

## TLDR

One monorepo with a pinned toolchain (`.mise.toml`), a `Makefile` front door and one GitHub Actions
workflow (`.github/workflows/ci.yml`) with seven jobs: `go`, `perf`, `ts`, `bridge-python`,
`bridge-dotnet`, `proto`, `licenses`. CI first ran on GitHub only after the M1 stack was merged;
it is green on `main` at `3d3d506`, but `bridge-dotnet` is non-blocking, TypeScript lint is a stub
and `main` has no branch protection.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/19 (squash commit `678663a`), https://github.com/nurtuo/nurtuo/pull/15 (`c69f869`), https://github.com/nurtuo/nurtuo/pull/39 (`e22ffab`) |
| ADRs | [ADR 0001](../../adr/0001-monorepo-license-split.md) |
| Code | `.mise.toml`, `Makefile`, `.github/workflows/ci.yml`, `.github/dependabot.yml`, `.github/PULL_REQUEST_TEMPLATE.md`, `.github/ISSUE_TEMPLATE/`, `scripts/check-docs-links.mjs`, `scripts/gen-flow-reference.mjs`, `scripts/check-example-operations.mjs`, `package.json`, `turbo.json`, `pnpm-workspace.yaml`, `go.work` |
| User docs | [ARCHITECTURE.md §7.2 and §8](../../ARCHITECTURE.md), [getting-started.md](../../getting-started.md), [PR stack review guide](../../review/PR-STACK.md) |

## Problem and goals

Contributors and CI need one reproducible way to build, test and check a repository that mixes
Go modules, a pnpm/turbo TypeScript workspace, protobuf contracts, Python/JVM/.NET bridges and two
licenses.

- Goals: pinned tool versions; one command per task (`make bootstrap|gen|build|test|lint|docs|boundaries|snapshot`);
  a CI workflow that runs every language's tests on every pull request; documentation that cannot
  silently rot (link check, generated flow reference).
- Non-goals: release publishing (see [release-and-packaging](2026-11-21-release-and-packaging.md)),
  license policy itself (see [license-boundaries](2026-11-21-license-boundaries.md)), performance
  targets (owned by the `performance-budget` spec).

## Design (as built)

### Toolchain

`.mise.toml` pins `go = "1.27"`, `node = "24"`, `pnpm = "10"`, `protoc-gen-go 1.36.12`,
`protoc-gen-go-grpc 1.6.2`. `buf`, `golangci-lint`, `go-licenses` and `goreleaser` are `"latest"`.
The root `package.json` pins `packageManager: pnpm@10.34.6` and `engines.node >=22`. `make tools`
installs the two protobuf plugins with the same versions as `.mise.toml`.

### Makefile targets

| Target | Runs |
|---|---|
| `bootstrap` | `mise install`, `pnpm install`, `go work sync` |
| `gen` | `buf lint`, `buf generate` |
| `build` | `CGO_ENABLED=0 go build github.com/nurtuo/nurtuo/...`, `pnpm turbo run build` |
| `test` | `go test -race github.com/nurtuo/nurtuo/...`, `pnpm turbo run test` |
| `lint` | `golangci-lint run ./core/runtime/... ./sdk/go/... ./connectors/...`, `pnpm turbo run lint typecheck`, `buf lint` |
| `bench` | `go test -run '^$' -bench . -benchtime 10000x ./core/runtime/test/bench/...` (both benchmarks `b.Skip("TODO(M1): ...")`) |
| `docs` | `node scripts/gen-flow-reference.mjs --validate --check`, `node scripts/check-docs-links.mjs` |
| `boundaries` | `./scripts/check-boundaries.sh` |
| `dev` | `pnpm turbo run dev` plus `go run ./core/runtime/cmd/nurtuo standalone` (README: does not work end to end because `nurtuo run` is not implemented) |
| `snapshot` | `goreleaser release --snapshot --clean --skip=sign` |

### Turbo pipeline

`turbo.json`: `build` depends on `^build` and caches `dist/**`; `typecheck` depends on `^build`;
`test` depends on the package's own `build` (tests run from `dist/**/*.test.js` with `node --test`,
except `@nurtuo/web`, which runs `vitest run`); `lint` has no dependencies.

### CI workflow `ci.yml`

Triggers: `push` to `main` and every `pull_request`. `permissions: contents: read`. A
`concurrency` group `ci-${{ github.ref }}` with `cancel-in-progress: true`, so a newer push to
`main` cancels the older run (this is why most `main` runs of the M1 squash merges show
`cancelled`).

| Job | Steps |
|---|---|
| `go` | `go vet` and `go test -race` on `github.com/nurtuo/nurtuo/...` (workspace mode), cross-compile `./core/runtime/cmd/nurtuo` for 6 targets with `CGO_ENABLED=0`, `golangci-lint-action@v9` |
| `perf` | `NURTUO_PERF=1 go test -p 1 -count=1 -v -run TestPerfBudget ./internal/bench` in `core/runtime` |
| `ts` | `pnpm install --frozen-lockfile`, `pnpm turbo run lint typecheck build test`, then `gen-flow-reference.mjs --validate --check`, `check-docs-links.mjs`, `check-example-operations.mjs` |
| `bridge-python` | `pip install -e "bridges/python[dev]"`, `gen_proto.py --check`, `ruff`, `mypy`, `pytest`, then the Go launcher test `-run 'Python'` in `connectors/bridge` with `NURTUO_TEST_PYTHON=python` |
| `bridge-dotnet` | `dotnet test bridges/dotnet/Nurtuo.Bridge.Tests`, `continue-on-error: true` |
| `proto` | `bufbuild/buf-action@v1`: lint, format, `breaking` against `main` on pull requests, `pr_comment: false` |
| `licenses` | see [license-boundaries](2026-11-21-license-boundaries.md) |

The `setup-go` cache key uses the three module sums (`core/runtime/go.sum`, `sdk/go/go.sum`,
`connectors/go.sum`) because there is no root `go.sum` (fixed in #15). The CLA is enforced by the
CLA Assistant app as the `license/cla` status (it reports on pull requests, for example #42), not
by a workflow job.

### Docs checks

`scripts/check-docs-links.mjs` walks every `*.md` outside `node_modules`, `.git`, `dist`, `.turbo`
and `.claude`, and fails on a relative link whose file or `#fragment` (GitHub slug) does not
exist. External URLs are not fetched. `scripts/gen-flow-reference.mjs --check` fails if
`docs/flow-reference.md` is stale; `--validate` validates its examples.
`scripts/check-example-operations.mjs` checks that example flows only use connector operations
that the connectors declare.

### Dependabot

`.github/dependabot.yml`: weekly updates for `gomod` (`/core/runtime`, `/sdk/go`, `/connectors`),
`npm` (`/`) and `github-actions` (`/`).

### Flaky-test fixes (#39)

Two tests failed intermittently on the first CI runs:
`core/control-plane/src/auth/authn.test.ts` split the API token `nrt_<prefix>_<secret>` on `_`,
although the base64url secret may contain `_`; and `cli/src/e2e.test.ts` reported `deploy --wait`
status from a fixed `setTimeout(…, 200)` that raced the CLI on slow runners. Both now parse or poll
deterministically.

## Contracts

| Contract | Stability |
|---|---|
| Make target names listed above | stable (referenced by README, CONTRIBUTING and the PR template) |
| CI job names `go`, `ts`, `proto`, `licenses`, `perf`, `bridge-python`, `bridge-dotnet`, `snapshot` | experimental: ARCHITECTURE.md §7.2 names `go`, `ts`, `proto`, `licenses`, `cla` as the intended required checks, none is required today |
| `scripts/check-docs-links.mjs` exit code (0 OK, 1 problems) | stable |

## Security

- Workflow token is `contents: read`; no secrets are used in `ci.yml`.
- `ci.yml` references actions by major tag (`actions/checkout@v7`, `actions/setup-go@v7`,
  `pnpm/action-setup@v6`, `bufbuild/buf-action@v1`, `anchore/sbom-action@v0`, …), while
  `release-snapshot.yml` pins commit SHAs. See gap 4.
- `pull_request` (not `pull_request_target`) is used, so forks never get a write token.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `scripts/check-docs-links.mjs` | relative Markdown links and anchors | `node scripts/check-docs-links.mjs` | |
| `scripts/gen-flow-reference.mjs --validate --check` | generated `docs/flow-reference.md` is current | `make docs` | |
| `scripts/check-example-operations.mjs` | example flows use declared connector operations | `node scripts/check-example-operations.mjs` | |
| CI workflow itself | — | push a branch | first executed on GitHub Actions after the stack merged (PR-STACK.md "CI jobs never run") |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | `main` has no branch protection (`gh api repos/nurtuo/nurtuo/branches/main/protection` returns 404). A pull request with a red `go` or `licenses` job, or a direct push, can land on `main`; the M1 stack was squash-merged while `ts` and `bridge-dotnet` were failing (run 44957022444 for #35). | high | repo settings | Protect `main`: required checks `go`, `ts`, `proto`, `licenses`, `license/cla`, one review, linear history, as ARCHITECTURE.md §7.2 states. |
| 2 | The interop tests in `core/runtime/internal/interop` call `t.Skip` when `core/control-plane/dist/index.js` is missing. The `go` job never builds the control plane, so all four tests skip on every CI run (the job log on `main` shows `ok …/internal/interop 1.027s`). A breaking change in `core/control-plane/src/gateway.ts` passes CI. | high | CI, interop | Build `@nurtuo/control-plane` in the `go` job (or a dedicated `interop` job) and set an env var that turns the skip into a failure. Tracked in [quality-gates](../2026-11-22-quality-gates.md). |
| 3 | `bridge-dotnet` is `continue-on-error: true` and has failed on every run so far (CS0542 on the proto message `Descriptor`; with the rename in open #41 it compiles but a test fails with a socket error). A regression in the .NET bridge SDK is invisible. | medium | bridges, CI | Land #41, fix the socket test, then drop `continue-on-error`. |
| 4 | `ci.yml` uses mutable major tags for third-party actions. A compromised or retagged `anchore/sbom-action@v0` runs with the checkout of every pull request. | medium | supply chain | Pin every action to a commit SHA with a version comment, as `release-snapshot.yml` already does; Dependabot keeps them current. |
| 5 | `.mise.toml` says "Pinned toolchain" but `buf`, `golangci-lint`, `go-licenses` and `goreleaser` are `latest`; CI pins different versions (`go-licenses@v2.0.1`, goreleaser `v2.20.3`). A new golangci-lint release adds a linter finding and `make lint` fails locally while CI passes, or the reverse. | low | toolchain | Pin exact versions in `.mise.toml` and read them from there in CI. |
| 6 | `pnpm turbo run lint` succeeds without linting: every TypeScript package's `lint` script is `echo 'lint: TODO (eslint)'`. | medium | TypeScript | [quality-gates](../2026-11-22-quality-gates.md). |
| 7 | No CI step regenerates `proto/` code (`TODO` in the `proto` job), `@nurtuo/client` types (`gen:check`) or the MCP connector catalog (`sync:check`). An edit to `core/control-plane/openapi.yaml` without `pnpm --filter @nurtuo/client gen` leaves the published client types stale. | medium | codegen | [quality-gates](../2026-11-22-quality-gates.md). |
| 8 | ARCHITECTURE.md §7.2 lists `cla`, `bench` (nightly), `e2e` and `release` jobs and a stale-codegen check that do not exist. Readers assume coverage that is absent. | low | docs | Update §7.2 to the jobs above, marking planned ones. |
| 9 | `make bench` reports success although both benchmarks in `core/runtime/test/bench` are `b.Skip("TODO(M1)…")`; the real latency budget is `TestPerfBudget` in `internal/bench`. | low | perf | Point `make bench` at `internal/bench` with `NURTUO_PERF=1`, or remove the stubs. |
| 10 | Intermediate branches of the M1 stack were never built; TypeScript was effectively untested until the lockfile landed in PR 16 (`pnpm install --frozen-lockfile` cannot run without it). `git bisect` across #19–#34 hits unbuildable commits. | low | history | Accept for M1; new work lands as self-contained pull requests that pass CI on their own. |

## Improvement opportunities

- Path filters so a docs-only change does not run the 5-minute `go` job.
- Cache the turbo cache (`.turbo`) between runs; the `ts` job is fast today (34 s on #42) but will grow.
- A `make ci` target that runs the same commands as the workflow, for local reproduction.

## References

- Open Mercato specs with a similar scope: `foundation`, `ci-cd-performance`,
  `phased-integration-ci`, `harness-validation-gate`.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
