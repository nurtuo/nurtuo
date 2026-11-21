<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0009: Runtimes dial out; one long-lived gateway stream carries desired state and status

Date: 2026-11-21. Status: accepted (client and supervisor implemented; control plane server side pending).

**Context.** Runtimes run inside customer networks, often behind firewalls and NAT, on hosts where
opening an inbound port is unacceptable. The control plane must still deploy, pause listeners and
receive status. The runtime must keep working when the control plane is away.

**Decision.** Contract `nurtuo.runtime.v1.RuntimeGatewayService` (`proto/nurtuo/runtime/v1/gateway.proto`):
the runtime always initiates a mutually authenticated TLS (TLS 1.3 minimum) gRPC connection. One
bidirectional `Connect` stream: the runtime sends `Hello` (id, version, OS, arch, last applied
revision), heartbeats (every 15 s by default), deployment statuses and command results; the control plane sends
`DesiredState` (revision plus deployments referencing bundle digests) and `Command`s. Bundles are
fetched with the server-streaming `FetchArtifact(digest)`. The supervisor reconciles actual state
to desired state; the revision advances only when every deployment applied, so a partial failure
makes the control plane resend on the next handshake. On disconnect the runtime keeps running its
last applied state and reconnects with exponential backoff and jitter (1 s to 60 s). `Command` names
are an open set; unknown commands are answered with `ok=false`.

**Consequences.**

- No inbound ports on the runtime host; works through outbound-only firewalls.
- The control plane is not on the data path: an outage stops deployments and reporting, not flows.
- Desired state is a level-triggered reconcile, so lost messages are repaired on reconnect.
- Security hinges on certificate issuance and on the server verifying the client identity against
  `Hello.runtime_id`; the server side is not implemented yet (see [security.md](../security.md)).
- Additive-only changes within `v1` (enforced by `buf breaking`).
