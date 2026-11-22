<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Listeners (HTTP and queue triggers)

Status: implemented (written as built); opt-in auth and TLS for the HTTP listener in review in
PR #42. Date: 2026-11-21.

## TLDR

A listener turns an external event into a document set and runs a flow on it. Two exist: the HTTP
listener (request and response) and the queue listener (`QueueSource` behind the generic
`Source` adapter). A `Manager` reconciles running listeners with the deployed flows. On `main` the
HTTP listener has no authentication and no TLS, binds `127.0.0.1:0` by default, starts **one HTTP
server per flow**, and passes the flow's path straight to `http.ServeMux`, where an invalid pattern
panics the runtime.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/24 (squash commit `2903374`); in review: https://github.com/nurtuo/nurtuo/pull/42 (opt-in auth and TLS, branch `feat/http-listener-auth`) |
| ADRs | [ADR 0008](../../adr/0008-listener-source-abstraction.md) |
| Issues | https://github.com/nurtuo/nurtuo/issues/40 (HTTP listener has no auth or TLS) |
| Code | `core/runtime/internal/listener/` (`listener.go`, `http.go`, `factory.go`, `manager.go`, `start.go`); deployer integration in `core/runtime/internal/runtime/deployer.go` |
| User docs | [flow-reference.md: start](../../flow-reference.md#start), [operations.md](../../operations.md), [security.md](../../security.md) |

## Problem and goals

Flows start from outside events. Operators need the same lifecycle everywhere (start, stop,
pause, resume, status) and consistent success and failure semantics, while HTTP callers need a
synchronous answer built from the flow's result.

- Goals: one `Listener` lifecycle; a minimal `Source` interface for subscribe-style transports;
  HTTP request to document and result to response mapping; bounded request bodies; graceful stop
  that waits for in-flight requests; desired-state reconciliation.
- Non-goals (as built): `schedule` and `manual` starts (nothing fires them); bridge (`Listen` RPC)
  listeners; path parameters and shared HTTP servers (draft
  [http-listener-routing](../2026-11-22-http-listener-routing.md)); rate limiting.

## Design (as built)

**Lifecycle** (`listener.go`). `Listener{Start(ctx, Handler), Stop(ctx), Pause, Resume, Status}`
with states `stopped`, `running`, `paused`; `Status{State, Addr, Received, Failed}`.
`Handler(ctx, *docs.Set) (*engine.Result, error)`; `FlowHandler(eng, f)` runs `eng.Run` with a
fresh `Process` per delivery. `NewSourceListener(src)` adapts a `Source.Consume(ctx, deliver)`:
a pause gate holds deliveries (in-flight ones finish), and a delivery counts as failed if the
handler errors or the result has failed documents; failure makes `deliver` return an error (nack).

**HTTP listener** (`http.go`). `NewHTTP(HTTPConfig{Addr, Path, Method, MaxBodyBytes, Timeout,
Spool})`: path must start with `/`; defaults `Method` POST, `Addr` `127.0.0.1:0`, `MaxBodyBytes`
1 MiB. `Start` calls `net.Listen("tcp", Addr)` and serves an `http.ServeMux` with the single
pattern `Path` (`mux.HandleFunc(l.cfg.Path, l.serve)`), `ReadHeaderTimeout` 10 s, no read, write or
idle timeouts. Per request (`serve`):

1. Wrong method: 405 with `Allow`. Paused: 503 with `Retry-After: 5`.
2. The body is read through `http.MaxBytesReader` into the spool (413 above the limit).
3. One document `http-<n>` with props `http.request.method`, `http.request.path`,
   `http.request.header.<lowercased>` (values joined by `,`), `http.request.query.<name>` (first
   value). These names deliberately do not collide with the HTTP connector's override props
   (`TestHTTPRequestPropsDoNotCollideWithConnectorOverrides`).
4. Run the flow (with `Timeout` if set; 504 on deadline). Handler error: 500 "flow failed".
   Failed documents: 500, or the error code itself when it is a number 400..599 (message as body).
   A stop step with status 100..599: that status with the stop message.
5. Otherwise `writeReturned`: no documents gives 204; status from `http.status` of the first
   document (default 200); headers from `http.response.header.<name>` of the first document; one
   document is written as is; several become a JSON array (valid JSON embedded, other bodies as
   strings).

`Stop` calls `http.Server.Shutdown(ctx)` (falls back to `Close`) and waits for the serve loop.

**Matching.** Because `Path` is used as a Go 1.22+ `ServeMux` pattern: `/employees/{id}` matches
`/employees/7` but the `id` value is not exposed to the flow (only `http.request.path`); a path
ending in `/` matches every sub-path; `/a/{` or `/a/{id}/{id}` make `HandleFunc` panic (verified
with go1.27.4 on 2026-11-22). See the routing draft.

**Factory and manager** (`factory.go`, `manager.go`, `start.go`). `StartOf(f)` returns the start
config and whether it is a listener (`type` `listener` or `queue`). `FromStart(sc, FactoryDeps)`
builds an HTTP listener for connector `http` or empty (other connectors are refused) using
`deps.HTTPAddr`, or a queue `SourceListener` over `deps.Queues`. `ForFlow` wraps that in a
`Desired{Key, Spec, Paused, Factory}`; `EngineDeployer` uses key `<deployment>/<flow>` and spec =
bundle digest. `Manager.Apply` stops removed listeners, restarts changed specs, starts new ones,
aligns pause state, and returns joined errors. `StartRunner` (registered by `listener.Register`)
validates the binding when the start step runs.

**Runtime config.** `runtime.yaml` key `http_addr` sets `FactoryDeps.HTTPAddr` for **every** HTTP
flow; supervisor commands `pause_listener` and `resume_listener` toggle pause.

**In review: PR #42** (closes #40; diff read with `gh pr diff 42` on 2026-11-22). Adds to
`startConfig` (schema, Go `flow.StartConfig`, TS types, corpus: 2 valid and 5 invalid cases):

- `auth` with `type` `bearer` (`Authorization: Bearer`), `apiKey` (header, default `X-API-Key`) or
  `hmac` (of the raw body; `sha256`/`sha512`, `hex`/`base64`, optional `prefix`, header default
  `X-Signature`); the secret must be a `secret://` reference resolved through
  `connections.Resolver.ResolveRef` (also registered with the log redactor). Comparisons are
  constant time (`checkHeader` hashes both sides first; `hmac.Equal`). Failure is 401 (with
  `WWW-Authenticate` for bearer) before the flow runs; bearer and apiKey headers are not copied
  into document props.
- `tls` with PEM certificate and key from secrets; minimum TLS 1.2.
- Safe default: `NewHTTP` refuses a non-loopback `Addr` (`0.0.0.0`, `::`, `:port`, an interface IP)
  without `auth` unless `insecure: true`; `isLoopback` decides.
- Tests: `TestHTTPBearerAuth`, `TestHTTPAPIKeyAuth`, `TestHTTPHMACAuth`, `TestHTTPAuthConfigErrors`,
  `TestIsLoopback`, `TestHTTPRefusesPublicBindWithoutAuth`, `TestHTTPTLSFromStart`,
  `TestFromStartAuthSecretErrors`, `TestResolveRef`.
- Not covered (stated in the PR): rate limiting, replay protection for `hmac`, client certificates,
  per-flow bind addresses; secrets are read at listener start, so rotation needs a redeploy.

## Contracts

| Contract | Stability |
|---|---|
| Request props `http.request.method`, `.path`, `.header.*`, `.query.*` | stable (flow authors use them) |
| Response props `http.status`, `http.response.header.*`; numeric error code 400..599 as status; stop status | stable |
| Status mapping: 204 for no documents, JSON array for several, 405, 413, 503 paused, 500, 504 | stable |
| `start` config `type`, `connector`, `path`, `method`, `queue`; PR #42 adds `auth`, `tls`, `insecure` | stable (schema) |
| Queue document props: message properties, `queue.key`; durable `nurtuo-<queue>` | experimental |
| `runtime.yaml` `http_addr`; commands `pause_listener`, `resume_listener` | experimental |
| Go API `Listener`, `Source`, `Manager`, `Desired`, `FromStart`, `HTTPConfig` | internal |

## Security

On `main`:

- No authentication and no TLS: anyone who can reach the bound address runs the flow. The default
  bind is loopback (`127.0.0.1:0`), so exposure needs an explicit `http_addr` such as
  `0.0.0.0:8080` (issue #40).
- Body limit 1 MiB (not configurable from the flow or `runtime.yaml`); `ReadHeaderTimeout` 10 s
  against slow-header attacks; no body read timeout, so a client sending 1 byte every few seconds
  keeps a connection and a goroutine for as long as it likes.
- All request headers, including `Authorization` and cookies, become document props, which flows
  may log or forward. PR #42 drops the configured credential header only.
- No rate limit and no concurrency cap per listener.

With PR #42 merged: opt-in bearer, API key or HMAC auth, TLS 1.2+, and refusal to bind a public
address without auth unless `insecure: true`.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestHTTPEchoWithProps`, `TestHTTPRequestBodyBecomesDocument`, `TestHTTPFlowFailureAndStop`, `TestHTTPLimitsMethodPause`, `TestHTTPGracefulShutdownWaitsForInflight` (`listener/listener_test.go`) | HTTP mapping, limits, 405/413/503, graceful stop | `cd core/runtime && GOWORK=off go test -race ./internal/listener/...` | |
| `TestManagerReconcile`, `TestSourceListenerPauseResume` | manager and source adapter | same | |
| `TestFromStartHTTP`, `TestFromStartQueueAckNack` (`listener/factory_test.go`) | factory, queue ack/nack | same | memory queue backend |
| `TestHTTPRequestPropsDoNotCollideWithConnectorOverrides` (`listener/collision_test.go`) | prop naming regression | same | |
| `TestChain`, `TestRealWiringHTTPConnector` (`core/runtime/internal/e2e`) | HTTP listener as deployed by the runtime | `cd core/runtime && GOWORK=off go test -race ./internal/e2e/...` | |
| PR #42 tests (listed above) | auth, TLS, bind refusal | same, on branch `feat/http-listener-auth` | in review |

No test covers path patterns with `{}` or a trailing `/`, a fixed `http_addr` with two HTTP flows,
or an invalid pattern.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A flow with `path` `/a/{` (or a duplicate wildcard name) is valid for the validator; on deploy `mux.HandleFunc` panics inside `Manager.Apply`, nothing recovers, and the runtime process exits, taking every other deployment down. As long as the deployment stays in the control plane's desired state, the supervisor applies it again after the restart (ADR 0009) and the crash repeats. | critical | listener, schema | validate the pattern in `NewHTTP` (use `http.NewServeMux` in a recover, or parse it ourselves) and add a schema `pattern`; draft [http-listener-routing](../2026-11-22-http-listener-routing.md) |
| 2 | With `http_addr: 0.0.0.0:8080`, the first HTTP flow binds the port and every other HTTP flow fails to start with "address already in use"; with the default `127.0.0.1:0` each flow gets a random port that callers cannot know. In practice one HTTP flow per runtime. | high | listener | one shared server per address with a router; routing draft |
| 3 | No auth or TLS on `main` (issue #40): a runtime exposed with `http_addr: 0.0.0.0:...` runs any flow for any caller, and webhook secrets travel in clear text. | high | security | PR #42 (in review); merge it and make `auth` required for non-loopback binds (it is) |
| 4 | `GET /employees/{id}`-style flows: the pattern matches, but `{id}` is not passed to the flow, so the example `get-employee.flow.json` cannot read the id without parsing `http.request.path`. The documented method default is POST, so the example (no `method`) answers 405 to `GET`. | medium | listener | routing draft (`http.request.path.<name>` props) |
| 5 | No request deadline: `FromStart` never sets `HTTPConfig.Timeout`, and the server has no write timeout. A hung connector call keeps the HTTP request, its goroutine and its spooled body until the client disconnects. | medium | listener | configurable timeout per start step with a runtime default (for example 30 s) |
| 6 | A queue delivery that fails is nacked with delay 0, so `MaxDeliveries` (5) is consumed within milliseconds (see [queues-and-dead-letter](2026-11-21-queues-and-dead-letter.md)). | high | listener, queue | use the queue's backoff; covered there |
| 7 | All inbound headers (cookies, `Authorization` on `main`) are copied into document props; a flow that logs props or forwards them leaks credentials. | medium | listener | allow-list or deny-list for header props; PR #42 removes the auth header only |
| 8 | `schedule` and `manual` starts are accepted but never fired; a nightly-sync flow deploys "running" and never runs. | medium | runtime | scheduler spec (other area) or a validator warning |
| 9 | `MaxBodyBytes` is fixed at 1 MiB from flows; a 5 MiB webhook (for example a large order) is always rejected with 413. | low | listener | `maxBodyBytes` in `startConfig` with a runtime ceiling |

## Improvement opportunities

- Report each listener's bound address in deployment status so callers can find random ports.
- Per-listener metrics (received, failed, latency) through the telemetry work.

## References

- Open Mercato: `inbound-webhook-handlers` (signature verification and routing for inbound hooks),
  `payment-gateway-webhook-body-limits` (body size policy per endpoint).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`; PR #42 described as in review.
