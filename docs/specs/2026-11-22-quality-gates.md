<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Quality gates

Status: draft. Date: 2026-11-22.

## TLDR

CI runs real tests, but several gates report success without checking anything: TypeScript `lint`
is `echo` in nine packages, generated code is never re-checked, interop and Playwright tests do not
run, three contract-suite cases are `t.Skip`, and `bridge-dotnet` cannot fail the build. This spec
turns each into a blocking check, in three phases, and adds rules learned from the flaky tests fixed
in #39.

## Status and links

| | |
|---|---|
| Pull requests | none yet; evidence from https://github.com/nurtuo/nurtuo/pull/19, https://github.com/nurtuo/nurtuo/pull/35, https://github.com/nurtuo/nurtuo/pull/39 |
| ADRs | — |
| Code (today) | `"lint": "echo 'lint: TODO (eslint)'"` in `sdk/ts/{client,flow-schema,flow-validate,proto}/package.json`, `cli/package.json`, `core/{control-plane,web,mcp,ai}/package.json`; `# TODO: regenerate and git diff --exit-code` in the `proto` job of `.github/workflows/ci.yml`; `continue-on-error: true` on `bridge-dotnet`; `t.Skip("TODO: …")` for `ExecuteLargePayload`, `CrashBehaviour`, `Listen` in `sdk/go/connector/contracttest/contracttest.go`; `requireNode` skip in `core/runtime/internal/interop/harness_test.go` |
| User docs | [repo-foundation-and-ci](implemented/2026-11-21-repo-foundation-and-ci.md), [ARCHITECTURE.md §9](../ARCHITECTURE.md) |

## Problem and goals

A green check should mean the thing it names was verified. Today a reviewer sees `ts` pass and
assumes lint ran, sees `go` pass and assumes the Go/TS gateway interop was tested.

- Goals: every listed gate either runs and can fail, or is removed from the job; generated files
  cannot drift; tests that need a service fail loudly in CI instead of skipping; flaky patterns are
  caught in review by rule.
- Non-goals: coverage percentages as a gate; style debates (formatter defaults are accepted).

## Design (proposed)

### Phase 1: make existing gates real (blocking)

| Gate | Change |
|---|---|
| TypeScript lint | One root `eslint.config.js` (flat config, `typescript-eslint` recommended-type-checked, `eslint-plugin-react-hooks` for `core/web`, `no-console` for `core/control-plane/src` and `core/mcp/src` except `cli.ts`/`stdio.ts`); every package's `lint` becomes `eslint .`. The existing `// eslint-disable-line @typescript-eslint/no-explicit-any` in `core/mcp/src/tooling.ts` then means something. |
| Stale generated code | New `codegen` job: `buf generate` then `git diff --exit-code -- sdk/go/gen sdk/ts/proto/src/gen`; `pnpm --filter @nurtuo/client gen:check`; `pnpm --filter @nurtuo/mcp sync:check`; `python bridges/python/scripts/gen_proto.py --check` (already in `bridge-python`). Protoc plugin versions from `.mise.toml`. |
| Interop | The `go` job (or a new `interop` job) runs `pnpm install --frozen-lockfile && pnpm turbo run build --filter=@nurtuo/control-plane...` first and sets `NURTUO_REQUIRE_INTEROP=1`; `requireNode` calls `t.Fatal` instead of `t.Skip` when that variable is set. |
| Canvas e2e | New `web-e2e` job: `pnpm exec playwright install --with-deps chromium`, `pnpm --filter @nurtuo/web test:e2e`, upload `core/web/test-results` on failure. |
| `bridge-dotnet` | After #41 (rename `Descriptor` → `ConnectorDescriptor`) lands and the socket test is fixed, remove `continue-on-error`. |
| Branch protection | Required checks: `go`, `perf`, `ts`, `codegen`, `interop`, `web-e2e`, `proto`, `licenses`, `bridge-python`, `license/cla`. |

### Phase 2: fill the test gaps the code already names

- `contracttest.ExecuteLargePayload`: stream a payload larger than the gRPC max message size and
  assert peak heap stays below a bound (`runtime.ReadMemStats` before/after).
- `contracttest.CrashBehaviour`: kill the connector process mid-batch; every document must end
  with a non-OK status.
- `contracttest.Listen`: run against a fixture connector that declares `OPERATION_TYPE_LISTEN`
  (depends on the `connector-listen-operation` spec).
- Replace the `b.Skip("TODO(M1)…")` benchmarks in `core/runtime/test/bench` or delete them and
  point `make bench` at `internal/bench`.
- `--json` output snapshots for `nurtuoctl` (see [ts-client-and-cli](implemented/2026-11-21-ts-client-and-cli.md)).

### Phase 3: measure test strength (non-blocking reports first)

- Mutation testing: `go-mutesting` (or `gremlins`) on `core/deploy/bundle`, `internal/gateway`,
  `sdk/go/flow`; StrykerJS on `sdk/ts/flow-validate` and `core/mcp/src/guardrails.ts`. Nightly,
  reported as a score; a gate only after a baseline exists.
- Property-based tests: `testing/quick` or `rapid` for bundle verification (any byte flip fails),
  `fast-check` for `applyOps` (undo after any op sequence restores the document), and the flow
  validator (TS and Go agree on random flows generated from the schema).

### Flaky-test rules (from #39)

1. Never split a token or id on a character its alphabet contains: base64url secrets contain `_`
   and `-`. Parse by known prefix and length (`authn.test.ts` fix).
2. No fixed `setTimeout`/`time.Sleep` to wait for another process; poll for the condition with a
   deadline (`cli/src/e2e.test.ts` fix, `eventually` in the interop harness).
3. A negative check ("X did not happen") waits for a positive signal that the system had the
   chance to do X before asserting (see [e2e-and-interop-testing](implemented/2026-11-21-e2e-and-interop-testing.md) gap 5).
4. Tests that need a service skip locally but fail in CI via a `NURTUO_REQUIRE_*` variable.

A lint rule (`no-restricted-syntax` for `setTimeout` in `*.test.ts` outside helpers) enforces rule 2
for TypeScript.

## Contracts

`NURTUO_REQUIRE_INTEROP` (and later `NURTUO_REQUIRE_POSTGRES`, `NURTUO_REQUIRE_PYTHON`) env vars
for tests: internal.

## Compatibility

None for users. Contributors: `pnpm turbo run lint` will report findings in existing code; phase 1
lands the config with existing violations fixed or explicitly disabled per line with a reason.

## Security

ESLint plugins and the Playwright browser download are new CI dependencies; pin versions in the
lockfile and pin the workflow actions by SHA.

## Testing (acceptance)

| Check | Acceptance criterion |
|---|---|
| lint | adding `console.log("x")` to `core/control-plane/src/api.ts` fails `pnpm turbo run lint` |
| codegen | editing one field in `proto/nurtuo/connector/v1/connector.proto` without `buf generate` fails `codegen`; same for `openapi.yaml` without `gen` and `connector.yaml` without `sync` |
| interop | with `NURTUO_REQUIRE_INTEROP=1` and no control plane `dist`, `go test ./internal/interop/...` fails; in CI the four interop tests report `PASS`, not `SKIP` (`-v` output) |
| web-e2e | the 10 Playwright tests run in CI; breaking `checkConnection` fails the job |
| bridge-dotnet | the job has no `continue-on-error` and is a required check |
| contract suite | `ExecuteLargePayload` and `CrashBehaviour` run (no `SKIP`) for the http, sftp, postgres and nats connectors |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Turning on type-checked ESLint reports hundreds of findings and blocks unrelated pull requests for days. | medium | rollout | Land config and fixes per package in separate pull requests; the gate becomes required when the last package is clean. |
| 2 | Interop and Playwright jobs add 3–5 minutes per pull request and new flakiness. | medium | CI time | Path filters (runtime, control plane, proto, web); retries `0` but traces uploaded; rule 2 above. |
| 3 | Mutation testing on the whole runtime takes hours. | low | CI time | Scoped packages, nightly only. |
| 4 | `buf generate` output differs between buf versions (`.mise.toml` says `latest`), so `codegen` fails for reasons unrelated to the change. | medium | toolchain | Pin buf and plugin versions first ([repo-foundation-and-ci](implemented/2026-11-21-repo-foundation-and-ci.md) gap 5). |

## Improvement opportunities

- `golangci-lint` config file committed (today the action runs with defaults).
- Coverage reports uploaded as artifacts for review, without a threshold.

## References

- Open Mercato: `harness-validation-gate`, `stryker-mutation-testing-ci-gate`,
  `agentic-property-based-testing`.

## Changelog

- 2026-11-22: created as draft.
