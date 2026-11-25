<!-- SPDX-License-Identifier: Apache-2.0 -->
# Runtime <-> control-plane interop (as implemented)

Verified by `core/runtime/internal/interop` (Go test driving the real `runtime.RunWith` against the
real TypeScript gateway in a node child process). Proto: `proto/nurtuo/runtime/v1/gateway.proto`.

## Transport

- gRPC (the gRPC wire protocol, not Connect) over HTTP/2 with TLS 1.3 only, one bidirectional stream
  `RuntimeGatewayService.Connect` per runtime. The runtime always dials out.
- Server (`core/control-plane/src/gateway.ts`, `createGatewayServer`): `node:http2` secure server,
  `requestCert: true`, `rejectUnauthorized: true`, trust anchor = gateway CA. ALPN `h2` is negotiated by
  node:http2 and by grpc-go without extra configuration.
- Client (`core/runtime/internal/gateway/mtls.go`): client cert + key, `RootCAs` = CA file,
  `ServerName` from `server_name` (the interop test sets `localhost`; when empty, grpc-go derives it
  from the dial target, which is not exercised here; the test PKI server certificate is `CN=localhost, SAN DNS:localhost, IP:127.0.0.1`).
- A client certificate from another CA fails the TLS handshake; the runtime sees an `Unavailable`
  session error and retries.

## Identity

The gateway derives the runtime id from the verified client certificate: URI SAN
`spiffe://nurtuo/runtime/<id>` wins, otherwise the subject CN. The first message must be `Hello`
and `Hello.runtime_id` must equal that identity, else `PermissionDenied` (a runtime that is not
registered via `POST /v1/runtimes` also gets `PermissionDenied`). A newer connection for the same
runtime replaces the older one.

## Messages

1. Runtime -> `Hello{runtime_id, version, os, arch, desired_state_revision}` where the revision is
   the last fully applied one.
2. Gateway reconciles: if the stored desired-state generation differs from
   `desired_state_revision` it sends `DesiredState{revision, deployments[]}`; equal means no message.
3. `DesiredState.deployments[i] = {deployment_id = component id, bundle_digest = revision digest}`.
   The runtime diffs by digest, applies, and reports `DeploymentStatus{deployment_id, revision, state, message}`
   with `state` in `applying|running|error|paused|stopped` (mapped to pending/running/failed/stopped/stopped).
   `revision` is the desired-state generation being applied; the gateway attributes it to the
   newest deployment row of that component with `generation <= revision`.
4. Runtime -> `Heartbeat` every 15 s (client default). The gateway idle timeout is off unless
   configured (`idleTimeoutMs`); if enabled it must exceed the heartbeat interval.
5. Gateway -> `Command{command_id, name, args}`; runtime answers `CommandResult` (e.g. `pause_listener`).

## Push paths

- REST `POST /v1/deployments` and `DELETE /v1/runtimes/{id}/deployments/{componentId}` bump the
  desired-state generation and write an outbox row. The outbox publisher (100 ms in the test launcher,
  500 ms default) pushes the new `DesiredState` to the connected runtime and marks the row published.
  Rows for offline runtimes are consumed; the `Hello` reconcile delivers state on reconnect.
- Reconnect: Go client backoff 1 s .. 60 s (x2, 20 % jitter). While disconnected the runtime keeps
  serving its last applied state.

## Artifacts

`bundle_digest` identifies the local bundle. With `trusted_keys` set, the runtime verifies the
SHA-256 and Ed25519 signature of `<hex>.bundle` in `bundle_dir`. If the file is missing or fails
verification, the runtime requests the artifact on the same outbound mTLS connection as
`Connect`. It caches the download under the digest after both checks pass.

When signing is enabled, the control plane advertises the signed archive's SHA-256 for derived
revisions and serves it only to runtimes whose desired state references it. A revision submitted
with an explicit `digest` still requires a manually placed archive. Runtimes with empty
`trusted_keys` read local `<hex>.json` files only. See the distributed setup steps in
[operations.md](operations.md#enable-signing-for-distributed-runtimes).

Standalone enables signing and establishes runtime trust at bootstrap. CI jobs that use
standalone start with the same defaults, so they can deploy derived revisions with an empty
cache. See [the standalone data directory layout](operations.md#data-directory-layout-standalone).
