<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: HTTP listener routing (shared server, path parameters, safe patterns)

Status: draft. Date: 2026-11-22.

## TLDR

Today every HTTP flow gets its own `http.Server` on `http_addr`, and its `path` is handed to
`http.ServeMux` unchecked: with a fixed port only one HTTP flow can run, `{id}` segments match but
their values are lost, and an invalid pattern panics the runtime. This spec defines one shared
server per address with a validated route table, path parameters exposed as document props, and
conflict detection at deploy time.

## Status and links

| | |
|---|---|
| Pull requests | — (builds on https://github.com/nurtuo/nurtuo/pull/24 and on https://github.com/nurtuo/nurtuo/pull/42, in review) |
| ADRs | [ADR 0008](../adr/0008-listener-source-abstraction.md); a new ADR is needed for the route syntax |
| Issues | https://github.com/nurtuo/nurtuo/issues/40 (auth and TLS, related) |
| Code | `core/runtime/internal/listener/http.go`, `factory.go`, `manager.go`; `sdk/ts/flow-schema/schema/flow.v1.schema.json` (`startConfig.path`); `examples/flows/get-employee.flow.json` |
| As-built spec | [listeners](implemented/2026-11-21-listeners.md) |

## Problem and goals

Evidence on `main` (`3d3d506`):

- `httpListener.Start` calls `net.Listen("tcp", cfg.Addr)` per flow and
  `mux.HandleFunc(cfg.Path, ...)` with the raw path. With `http_addr: 0.0.0.0:8080` the second
  HTTP flow fails with "address already in use"; with the default `127.0.0.1:0` each flow has an
  unknown random port.
- Go 1.22+ `ServeMux` treats the path as a pattern. Checked with go1.27.4: `/employees/{id}`
  matches `/employees/7` (the listener never reads `r.PathValue`, so the flow sees only
  `http.request.path`), `/hook/` matches `/hook/x/y`, and `/a/{` and `/a/{id}/{id}` panic in
  `HandleFunc`. `Manager.Apply` does not recover, so the runtime exits.
- `examples/flows/get-employee.flow.json` uses `/employees/{id}` with no `method`, so the listener
  defaults to POST and answers 405 to `GET /employees/7`, and the flow has no `id` prop to query
  with.

Goals:

- Any number of HTTP flows on one address (one server per distinct address, TLS config included).
- A documented route syntax: literal segments and `{name}` parameters; optionally one trailing
  `{name...}` remainder parameter. No implicit prefix match from a trailing `/`.
- Path parameter values available to the flow as `http.request.path.<name>` props.
- Invalid patterns rejected by the validator (TS and Go, same code) and, as defence in depth, by
  the runtime without panicking.
- Conflicts (two deployed flows with the same method and an overlapping pattern) fail the second
  deployment with a clear message instead of silently shadowing.
- The bound address of each route is reported in deployment status.

Non-goals: host-based routing, regex parameters, query-string routing, per-route rate limits
(separate spec), changing auth semantics from PR #42.

## Design (proposed)

**Route syntax.** `path` is `/` or a sequence of `/segment` where each segment is either a literal
(RFC 3986 `pchar` characters, no `{` or `}`) or a parameter `{name}` with `name` matching
`[a-z][a-zA-Z0-9_]*`; the last segment may be a remainder parameter `{name...}`. No trailing `/`.
The exact regular expression lives in the schema. Parameter names are unique within a path. Method is
upper-cased; the default stays POST for compatibility, and the canvas sets `GET` explicitly when a
path has parameters.

**Validator.** The schema gets a `pattern` on `startConfig.path` (stage 1, `schema.pattern`). A new
semantic rule `start.path_param_duplicate` covers duplicate names, which a regex cannot express.
Both are added to the conformance corpus first (ADR 0004).

**Shared server.** A new `listener.Router` owns one `http.Server` and one `net.Listener` per
`(address, TLS config fingerprint)`. `FromStart` returns a lightweight route listener that
registers `(method, pattern) -> handler` with the router on `Start` and unregisters on `Stop`;
pause and status stay per route. The router builds its own `ServeMux` per change (copy on write,
swapped atomically) inside a `recover`, so a bad pattern is returned as an error. The server
starts with the first route and shuts down gracefully after the last one is removed.

**Conflicts.** Before registering, the router checks the new pattern against existing ones with
the same method using `ServeMux` conflict rules (registration of conflicting patterns panics in Go,
so the check runs in the same recovered builder). `EngineDeployer.Apply` reports the conflict as a
deployment failure naming both flows.

**Props.** For each parameter `{name}` the document gets `http.request.path.<name>` with the
unescaped value; `http.request.path` keeps the full path. These names do not collide with the HTTP
connector override props (`TestHTTPRequestPropsDoNotCollideWithConnectorOverrides` is extended).

**Status.** `Status.Addr` becomes the shared server address; deployment status lists
`method path -> address` per HTTP route.

**Auth and TLS (PR #42).** Auth stays per route. TLS is per server: two routes on the same address
with different `tls` secrets are a conflict. The public-bind refusal (`isLoopback`) is evaluated
per route as today.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `startConfig.path` | constrained by a schema `pattern`; trailing `/` no longer means prefix | stable (schema) |
| Diagnostic `start.path_param_duplicate` | new | stable |
| Props `http.request.path.<name>` | new | stable |
| Several HTTP flows per `http_addr` | new behaviour | stable |
| Deployment status field with HTTP route addresses | new | experimental |

## Compatibility

- Flows whose `path` ends in `/` and rely on prefix matching stop matching sub-paths. Migration:
  use `{rest...}`. A deploy-time warning lists such flows for one release before the change.
- Flows with a path that the new pattern rejects become invalid; today such paths either panic
  the runtime or never match, so no working flow is expected to break.
- The default method stays POST. The GetEmployee example gets `"method": "GET"` and uses
  `http.request.path.id`.

## Security

- Removes a crash-the-runtime input (invalid pattern) reachable by anyone allowed to deploy flows.
- One shared server means one set of server timeouts: set `ReadHeaderTimeout` 10 s (as today),
  `ReadTimeout` and `WriteTimeout` from runtime config (default 60 s), `IdleTimeout` 120 s.
- Parameter values are attacker-controlled strings; they are unescaped once and passed as props,
  never interpreted as paths on disk by the runtime.

## Testing

Acceptance tests (new, `core/runtime/internal/listener`):

| Test | Expectation |
|---|---|
| `TestRouterTwoFlowsOneAddress` | two flows on `127.0.0.1:<fixed>` both answer; stopping one leaves the other running |
| `TestRouterPathParams` | `GET /employees/{id}` with `/employees/7` gives prop `http.request.path.id = "7"`; `/employees/7/x` is 404 |
| `TestRouterInvalidPatternNoPanic` | `NewHTTP`/`Start` with `/a/{` returns an error; the process does not panic |
| `TestRouterConflict` | a second `POST /hook` on the same address fails with an error naming the first route |
| `TestRouterTLSMismatchConflict` | two routes on one address with different TLS secrets fail (after PR #42) |
| `TestRouterGracefulStop` | in-flight request on route A completes while route B is removed |
| corpus `invalid/start-path-bad-pattern.json`, `invalid/start-path-param-duplicate.json` | `schema.pattern` and `start.path_param_duplicate` in TS and Go |
| `TestGetEmployeeOverHTTP` (`internal/e2e`) | the updated example answers `GET /employees/7` with the employee JSON |

Run: `cd core/runtime && GOWORK=off go test -race ./internal/listener/... ./internal/e2e/...`,
`cd sdk/go && GOWORK=off go test ./flow/...`, `pnpm turbo run test --filter=@nurtuo/flow-validate`.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | The shared server becomes a single point of failure: a panic in one route handler is recovered by `net/http` per request, but a router bug affects every HTTP flow on that address. | medium | listener | router unit tests with `-race`; keep the router small |
| 2 | Changing a trailing-`/` path from prefix to exact match breaks a deployed webhook silently if the warning release is skipped. | medium | compatibility | deploy-time warning for one release; changelog entry |
| 3 | Conflict detection depends on `ServeMux` semantics, which may change between Go versions. | low | listener | conflict tests pinned in CI on the toolchain from `.mise.toml` |

## Improvement opportunities

- Report routes to the control plane so the console can show the public URL of each flow.
- Per-route body limits (`maxBodyBytes`) with a runtime ceiling.

## References

- Open Mercato: `inbound-webhook-handlers` (one endpoint per handler, routing and verification).

## Changelog

- 2026-11-22: created.
