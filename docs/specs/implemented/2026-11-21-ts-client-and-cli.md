<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: TypeScript client (`@nurtuo/client`) and CLI (`nurtuoctl`)

Status: implemented. Date: 2026-11-21.

## TLDR

`@nurtuo/client` is a dependency-free `fetch` client for the control plane REST API with wire types
generated from `core/control-plane/openapi.yaml`, retries for idempotent requests and typed errors.
`nurtuoctl` (`@nurtuo/cli`) validates, formats, diffs, pushes and deploys flows with stable exit
codes for CI. Both are Apache-2.0 and are tested only against an in-memory fake of the API.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/33 (squash commit `c534511`), flaky-test fix in https://github.com/nurtuo/nurtuo/pull/39 (`e22ffab`) |
| ADRs | — |
| Code | `sdk/ts/client/src/index.ts`, `sdk/ts/client/src/testing.ts`, `sdk/ts/client/src/schema.generated.ts`, `sdk/ts/client/scripts/gen-types.mjs`, `cli/src/{cli,args,config,completion,flowtools,index}.ts` |
| User docs | [cli.md](../../cli.md) |

## Problem and goals

Flows are files in Git; teams need to check, push and deploy them from CI and terminals without the
canvas, and integrators need a typed API client.

- Goals: local `validate`/`fmt`/`diff`/`init` with the same validator as the canvas and control
  plane; server commands for components, revisions, deployments and runtimes; deterministic exit
  codes; credentials outside shell history; shell completion; a reusable typed client.
- Non-goals: managing connections and secrets, users and roles, or listeners from the CLI; npm
  publishing (deferred until the `@nurtuo` npm org exists, ARCHITECTURE.md §7.1).

## Design (as built)

### `@nurtuo/client`

- `NurtuoClient(options)` with `baseUrl` (required), `token`, `timeoutMs` (30000, `0` disables),
  `retries` (2), `retryDelayMs` (200), `retryUnsafe` (false), `headers`, `fetch`.
- Methods: `listComponents`, `getComponent`, `createComponent`, `updateComponent`,
  `deleteComponent`, `listRevisions`, `createRevision`, `getRevision`, `listRuntimes`,
  `getRuntime`, `registerRuntime`, `getDesiredState`, `listDeployments`, `deploy`, `getDeployment`,
  `reportDeploymentStatus`.
- Retry policy: GET/PUT/DELETE (and POST/PATCH only with `retryUnsafe`) are retried on connection
  errors and HTTP 408/429/500/502/503/504, with exponential backoff and jitter
  (`retryDelayMs * 2^attempt * (0.5..1)`), honouring `Retry-After`, capped at 10 s per wait.
- Errors: `ApiError` (`status`, `code`, `message`, `body`) for HTTP errors, `NetworkError`
  (`timedOut`) when no response arrived.
- Types: `schema.generated.ts` is produced by `openapi-typescript` from
  `core/control-plane/openapi.yaml` (`pnpm --filter @nurtuo/client gen`; `gen:check` fails when stale).
- `@nurtuo/client/testing` exports `startFakeControlPlane()`, an in-memory HTTP implementation of
  the API that shares no code with the AGPL control plane and records every request.

### `nurtuoctl`

- Entry `cli/src/index.ts` → `run()` in `cli/src/cli.ts`; a declarative command table drives
  parsing (`args.ts`), help (`help <command>`, `--help`) and completion scripts (`completion.ts`
  for bash, zsh, PowerShell).
- Local commands: `validate`, `fmt [-w] [--check]`, `diff [--exit-code]`, `init [-t minimal|http-get]`,
  `completion`. Canonical formatting and the semantic diff live in `flowtools.ts`.
- Server commands: `login [--token-stdin] [--no-verify]`, `logout`, `component list|get|create|delete|revisions`,
  `flow push [--component] [--create] [--force]`, `flow pull [-r] [-o]`,
  `deploy <c> --runtime <id> [-r] [--wait]`, `deploy status <id> [--wait] [--timeout] [--interval]`,
  `runtime list [-e]`.
- `flow push` validates locally first; with errors nothing is sent. Identical content to the latest
  revision (key order ignored) is not pushed unless `--force`.
- Connection settings: flag, then `NURTUO_SERVER`/`NURTUO_TOKEN`, then the config file at
  `$NURTUO_CONFIG`, `%APPDATA%\nurtuo\config.json` or `${XDG_CONFIG_HOME:-~/.config}/nurtuo/config.json`,
  written with mode `0600` (`chmodSync` after write; not enforced on Windows).
- Exit codes (`EXIT` in `cli.ts`): 0 OK, 1 failure, 2 usage, 3 network, 4 auth (401/403).
- `--json` prints results to stdout and errors as `{"error":{"code","message"}}` to stderr.
- `--version` reads `version` from `cli/package.json` (`0.0.0` today).

## Contracts

| Contract | Stability |
|---|---|
| CLI command names, flags and exit codes (cli.md) | experimental until the first npm release, then stable (breaking a CI script is a breaking change) |
| `--json` output shapes | experimental (not documented field by field) |
| Environment variables `NURTUO_SERVER`, `NURTUO_TOKEN`, `NURTUO_CONFIG` | stable |
| Config file format `{server, token}` | internal |
| `NurtuoClient` methods, options, `ApiError`, `NetworkError` | experimental (package version `0.0.0`) |
| `@nurtuo/client/testing` `startFakeControlPlane()` | experimental |

## Security

- The token is sent as `Authorization: Bearer`, never printed; `login --token-stdin` keeps it out of
  shell history; cli.md warns that `--token` shows up in process listings.
- The token is stored in plain JSON. File mode `0600` on POSIX is the only protection.
- `NurtuoClient` does not reject an `http://` `baseUrl`; a token configured for a plain-HTTP server
  is sent in clear text (gap 3).
- POST is never retried by default, so `deploy` and `createRevision` cannot be duplicated by a retry.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `sdk/ts/client/src/client.test.ts` (7 tests) | round trip, 204, path encoding, errors, retry on 503 with `Retry-After`, no POST retry, 4xx not retried, timeouts | `pnpm turbo run test --filter=@nurtuo/client` | against `startFakeControlPlane()` |
| `cli/src/unit.test.ts` (6 tests) | arg parser, config path precedence, slugify, `formatFlow` idempotence, `diffFlows` | `pnpm turbo run test --filter=@nurtuo/cli` | |
| `cli/src/e2e.test.ts` (13 tests) | help, validate, login/config `0600`, components, push/pull/deploy/status/runtime list, exit 3 on network failure, diff, fmt, init, completion, logout | same | runs the built CLI against the fake; `deploy --wait` polling fixed in #39 |
| `pnpm --filter @nurtuo/client gen:check` | generated types match `openapi.yaml` | manual | not run in CI |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The client and CLI are tested only against `startFakeControlPlane()`, a hand-written fake. If the real control plane changes a response shape (for example `items` to `data` in `GET /v1/components`) without changing the fake, all client and CLI tests stay green and `nurtuoctl component list` breaks for users. | high | contract drift | Run `client.test.ts` and the CLI e2e suite a second time against the real `createHandler` from `core/control-plane` with `MemoryRepository` (as `core/control-plane/src/mcp-store.test.ts` does for the MCP store), in an AGPL test package so the Apache packages stay clean. |
| 2 | `gen:check` is not in CI. Editing `core/control-plane/openapi.yaml` without regenerating leaves `schema.generated.ts` stale; TypeScript keeps compiling against the old types. | medium | codegen | Add `pnpm --filter @nurtuo/client gen:check` to the `ts` job ([quality-gates](../2026-11-22-quality-gates.md)). |
| 3 | `nurtuoctl login --server http://cp.internal:8080` stores the server and every later command sends the bearer token over plain HTTP. | medium | security | Refuse `http://` for non-loopback hosts unless `--insecure` is passed; same option in `NurtuoClient`. |
| 4 | `core/mcp/src/control-plane-client.ts` is a second, independent REST client for the same API (AGPL), with its own error mapping and no retries. The two can disagree on paths or error codes. | medium | duplication | Make `@nurtuo/mcp` use `@nurtuo/client` (AGPL may depend on Apache). |
| 5 | `--json` output is not specified; a field rename in `cli.ts` breaks CI scripts that parse it, and no test pins the shapes. | medium | contract | Document the shapes in cli.md and add snapshot assertions in `e2e.test.ts`. |
| 6 | The token is stored in plain text. On a shared Windows machine `%APPDATA%\nurtuo\config.json` has the default ACLs of the profile, not `0600`. | low | security | Optional OS keychain storage (Windows Credential Manager, macOS Keychain, libsecret). |
| 7 | No packaging: `@nurtuo/cli` is `0.0.0` and not published; users must build the monorepo to get `nurtuoctl`. | medium | distribution | npm publishing with provenance ([release-signing-and-installers](../2026-11-22-release-signing-and-installers.md)), or a `bun build --compile` binary. |
| 8 | Components are resolved "by id or by name" with a full `GET /v1/components` listing per command; with thousands of components every command downloads the whole list. | low | performance | Server-side `?name=` filter. |

## Improvement opportunities

- `nurtuoctl flow push --all flows/` for repositories with many flows.
- `nurtuoctl deploy --wait` could stream status changes instead of polling once a watch endpoint exists.
- A `--profile` option for several control planes (staging, production) in one config file.

## References

- Open Mercato: `cli-module-search`, `mercato-cli-skills-sync`.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
