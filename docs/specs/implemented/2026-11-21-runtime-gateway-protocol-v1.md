<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Runtime gateway protocol v1 (`nurtuo.runtime.v1`)

Status: implemented (`Connect`); `FetchArtifact` is declared but not implemented on either side.
Date: 2026-11-21 (written as built on 2026-11-22).

## TLDR

A runtime always dials out to the control plane over mutual TLS 1.3 and keeps one bidirectional
`Connect` stream: `Hello`, heartbeats, deployment statuses and command results go up; `DesiredState`
and `Command`s come down. Desired state is level-triggered: the control plane resends it whenever
the runtime's `Hello.desired_state_revision` differs, so a lost message is repaired on reconnect.
The Go client is `core/runtime/internal/gateway`, the server is `core/control-plane/src/gateway.ts`.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/20 (proto, squash `e637dfd`), https://github.com/nurtuo/nurtuo/pull/25 (Go client, squash `ee4d733`), https://github.com/nurtuo/nurtuo/pull/26 (TypeScript server, squash `dcd60dc`) |
| ADRs | [ADR 0009](../../adr/0009-outbound-only-runtime-gateway.md) |
| Code | `proto/nurtuo/runtime/v1/gateway.proto`, `core/runtime/internal/gateway/{client.go,mtls.go}`, `core/control-plane/src/gateway.ts`, `core/control-plane/src/main.ts` |
| User docs | [operations.md](../../operations.md), [security.md](../../security.md#mtls-between-runtime-and-control-plane) |

## Problem and goals

Runtimes run inside customer networks behind firewalls and NAT where an inbound port is not
acceptable, yet the control plane must deploy, pause listeners and see status.

- Goals: outbound-only connectivity; runtime identity bound to a client certificate; the data path
  survives a control plane outage; idempotent reconcile instead of exactly-once delivery.
- Non-goals (as built): bundle transfer over the gateway (`FetchArtifact` is unimplemented), log or
  metrics streaming, certificate issuance and rotation, multiple control plane replicas.

## Design (as built)

**Service** `RuntimeGatewayService` in `proto/nurtuo/runtime/v1/gateway.proto`:

- `Connect(stream ConnectRequest) returns (stream ConnectResponse)`. Up: `Hello{runtime_id,
  version, os, arch, desired_state_revision}`, `Heartbeat{at}`, `DeploymentStatus{revision,
  deployment_id, state, message}`, `CommandResult{command_id, ok, message}`. Down:
  `DesiredState{revision, deployments[]}` with `Deployment{deployment_id, bundle_digest,
  queue_ids, listener_ids}`, and `Command{command_id, name, args}`.
- `FetchArtifact(FetchArtifactRequest{digest}) returns (stream FetchArtifactResponse{data})`:
  declared only. The control plane registers only `connect` (`Gateway.register`), so a call returns
  `UNIMPLEMENTED`; the runtime never calls it and reads bundles from `bundle_dir` instead.

**Runtime client** (`core/runtime/internal/gateway/client.go`):

- `NewClient(Config)` defaults: `Heartbeat` 15 s, `Backoff{Initial: 1s, Max: 1m, Factor: 2}`,
  `OutboxSize` 64. `Backoff.Next(attempt)` multiplies up to `Max` and subtracts up to 20 % jitter.
- `Run` dials once with `grpc.NewClient`, then loops `session` until the context ends. A session
  that lasted longer than 30 s resets the attempt counter.
- `session` opens `Connect`, sends `Hello` with `Handler.Revision()`, then starts one sender
  goroutine (gRPC allows one concurrent `Send`) that multiplexes heartbeats and the outbox. The
  receive loop calls `Handler.ApplyDesiredState` for `DesiredState` and `Handler.HandleCommand` for
  `Command`, both **synchronously** on the receive goroutine; command results are queued to the
  outbox with the command id copied in.
- `ReportStatus`/`enqueue` are best effort: when the 64-slot outbox is full the message is dropped
  with the log line `gateway outbox full, dropping message`.
- `MTLSCredentials(certFile, keyFile, caFile, serverName)` (`mtls.go`): client key pair, RootCAs
  from `ca_file` only (no system roots), optional `ServerName`, `MinVersion: tls.VersionTLS13`.

**Control plane server** (`core/control-plane/src/gateway.ts`):

- `createGatewayServer` is an HTTP/2 TLS server with `requestCert: true`,
  `rejectUnauthorized: true`, `minVersion: "TLSv1.3"`. It starts in `main.ts` only when
  `GATEWAY_CERT`, `GATEWAY_KEY` and `GATEWAY_CA` are set; port `GATEWAY_PORT` (default 8443).
- `runtimeIdFromCert`: identity is the URI SAN `spiffe://nurtuo/runtime/<id>` if present, else the
  subject CN.
- `connect`: the first message must be `Hello` (`INVALID_ARGUMENT` otherwise);
  `Hello.runtime_id` must equal the certificate identity (`PERMISSION_DENIED`); the runtime must be
  registered (`repo.touchRuntime`, else `PERMISSION_DENIED`). The newest session for a runtime
  replaces the older one. If the stored desired state's generation differs from the Hello
  revision, `DesiredState` is pushed immediately.
- Desired state: `deployment_id` is the component id (one slot per component per runtime),
  `bundle_digest` the revision digest, `revision` the desired-state generation. `queue_ids` and
  `listener_ids` are never filled by the server.
- Outbox: `publishOutbox` pushes `desired_state.changed` rows every 500 ms
  (`startOutboxPublisher`); rows for offline runtimes are marked published and repaired by the
  Hello reconcile.
- Status: `STATE_MAP` maps `applying -> pending`, `running -> running`, `paused -> stopped`,
  `error -> failed`, `stopped -> stopped`; unknown states are logged and ignored. A status is
  matched to the newest deployment row at or below the reported revision.
- Commands: `sendCommand(runtimeId, name, args, timeoutMs = 10_000)` resolves with the
  `CommandResult` or rejects with `command timed out`.
- `idleTimeoutMs` closes silent sessions; it defaults to 0 (disabled) and `main.ts` does not set it.

## Contracts

| Contract | Stability | Notes |
|---|---|---|
| proto package `nurtuo.runtime.v1` | stable, additive-only | header comment; `buf breaking` on pull requests |
| `Hello` first, identity check, revision-based reconcile | stable | ADR 0009 |
| `DeploymentStatus.state` values `applying`, `running`, `paused`, `error` | stable, open set | new states are additive; the server ignores unknown ones |
| `Command.name` values | open set | implemented in the runtime: `pause_listener`, `resume_listener` (arg `listener` = `<deployment>/<flow>`); named but unhandled: `replay_dlq`, `run_now`, `test_execute`; unknown commands answer `ok=false` |
| Certificate identity: URI SAN `spiffe://nurtuo/runtime/<id>`, else CN | stable | `runtimeIdFromCert` |
| Heartbeat 15 s, backoff 1 s to 60 s, outbox 64 | internal defaults | not configurable from `runtime.yaml` |
| `FetchArtifact` | experimental, unimplemented | see draft spec `bundle-artifact-delivery` (another area) |
| Server env `GATEWAY_CERT`, `GATEWAY_KEY`, `GATEWAY_CA`, `GATEWAY_PORT` | stable | `core/control-plane/src/main.ts` |

## Security

- Mutual TLS 1.3 on both sides; the client trusts only `ca_file`; the server requires a client
  certificate from `GATEWAY_CA` and binds `Hello.runtime_id` to it.
- No inbound port on the runtime host. The control plane is not on the data path.
- No certificate rotation, revocation (CRL/OCSP) or pinning; issuance is manual except in
  standalone mode, where a local CA is generated (see the standalone spec).
- Connection errors and `Test` diagnostics are redacted by `connhost.Manager` (`RedactError`,
  `Redact`) before they become a `DeploymentStatus.message`; there is no final redaction pass on
  the status path itself (`gateway.Client.ReportStatus` sends the message as given).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestHandshakeReconcileCommandReconnect` in `core/runtime/internal/gateway/client_test.go` | Hello, desired state, command round trip, reconnect | `cd core/runtime && GOWORK=off go test -race ./internal/gateway/...` | in-process fake server over bufconn |
| `TestMTLSRejectsClientWithoutCert` in the same file | server refuses a client without a certificate | same | |
| `gateway.test.ts` in `core/control-plane/src` (`"Hello whose runtime_id does not match the certificate is rejected"`, `"identity can come from a URI SAN (CN ignored)"`, `"client cert from an untrusted CA is refused at the TLS layer"`, `"reconnect resends state missed while offline"`, `"a newer connection for the same runtime replaces the older one"`) | server rules | `pnpm turbo run test --filter=@nurtuo/control-plane` | test certificates in `core/control-plane/testdata/certs` (TEST-ONLY keys) |
| `TestRuntimeAgainstTSGateway`, `TestWrongIdentityRejected`, `TestCertificateIdentities` in `core/runtime/internal/interop/interop_test.go` | Go client against the real TS server | `cd core/runtime && GOWORK=off go test ./internal/interop/...` | skipped when `node` is not on PATH or the control plane `dist` is missing |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A desired state with several connections under `connection_test: strict` takes up to 15 s per connection (`connhost.DefaultTestTimeout`) inside `ApplyDesiredState`, which runs on the receive goroutine; a `pause_listener` sent meanwhile is not read until the apply finishes, and the control plane's `sendCommand` rejects with `command timed out` after 10 s although the runtime later executes it | high | runtime client | run applies in a worker goroutine with a "latest desired state wins" slot; keep commands on the receive loop; add a test with a slow deployer |
| 2 | During a large reconcile (two statuses per deployment) while the stream is slow, more than 64 messages queue and `enqueue` drops statuses; the control plane keeps a deployment at `pending` forever because statuses are not resent unless the state changes | medium | runtime client | block with a bounded wait for statuses instead of dropping, or resend the full status set after each reconcile and on every Hello |
| 3 | A NAT or firewall silently drops the TCP flow; the client has no gRPC keepalive configured and only learns of the loss when a heartbeat `Send` fails, and the server's `idleTimeoutMs` is 0, so the control plane shows the runtime online while it is not | medium | both sides | set `grpc.WithKeepaliveParams` on the client and `idleTimeoutMs` (for example 3 x heartbeat) in `main.ts` |
| 4 | A runtime certificate leaks; there is no revocation, so the holder can impersonate the runtime until the certificate expires (2 years for standalone-issued certificates) | high | security | CRL or short-lived certificates with rotation; tracked in the security docs |
| 5 | An operator deploys to a runtime with `bundle_dir` unset; the runtime uses the in-memory placeholder deployer (`memDeployer` in `core/runtime/internal/runtime/run.go`) and reports `running` for flows it never started | high | runtime | see the supervisor and deployer spec; refuse or report `error` instead |
| 6 | Bundles are not delivered by the gateway (`FetchArtifact` is unimplemented on both sides); every deployment needs the bundle copied into `bundle_dir` out of band, and a missing file surfaces only as `error` with `fetch bundle ...: no such file` | high | protocol | draft spec `bundle-artifact-delivery` (owned by another area) |
| 7 | The `paused -> stopped` mapping in `STATE_MAP` loses information: the UI cannot tell a paused listener from a stopped deployment | low | control plane | add a `paused` deployment status |
| 8 | `queue_ids` and `listener_ids` in `Deployment` are never set by the server and never read by the runtime except in `supervisor.equal`; they are dead fields of a stable contract | low | contract | document as reserved, or use them for listener-only changes |
| 9 | ARCHITECTURE.md promises runtime/control plane compatibility across N-1 minor versions; no test runs an older runtime against a newer control plane | medium | compatibility | draft spec `backward-compatibility-contract` (version matrix job) |

## Improvement opportunities

- Send a `version` compatibility answer in the first `ConnectResponse` so an incompatible runtime
  fails fast with a clear message.
- Make heartbeat, backoff and outbox size configurable in `runtime.yaml` for constrained links.
- Expose session state (`connected`, `last_error`, `reconnect_in`) through the planned health
  endpoint (draft spec `observability-logging-telemetry-health`, another area).

## References

- Open Mercato: `module-runtime-start-hook` (start-up reconcile hook), `secure-sse-probe-event-scope`
  (scoping a long-lived event stream to an authenticated principal).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
