<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Distributed runtime roles, runtime groups and HA

Status: draft. Date: 2026-11-22.

## TLDR

Today one runtime process does everything and `nurtuo role <name>` exits with "not implemented
yet". This spec defines **runtime groups**: several `nurtuo` processes that share one desired
state, one embedded NATS JetStream cluster for queues, and leader election for singleton work, so
that a group survives the loss of one node. Roles (`queue`, `worker`, `ingress`, `all`) let large
installations split work; `nurtuo run` stays `all`.

## Status and links

| | |
|---|---|
| Pull requests | none yet |
| ADRs | [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (gateway), [ADR 0007](../adr/0007-queue-dlq-via-advisory.md) (queues and DLQ); a new ADR "runtime groups and leader election" is required |
| Code (today) | `core/runtime/cmd/nurtuo/main.go` (`stubs = []string{"role"}`), `core/runtime/internal/runtime/run.go` (one process, `memory.New()` queues), `core/runtime/internal/queue/natsjs` (JetStream backend, not wired into the runtime process), `core/control-plane/src/gateway.ts` (one session per runtime id), `core/control-plane/migrations/0002_runtimes.sql` |
| User docs | [operations.md](../operations.md), [ARCHITECTURE.md](../ARCHITECTURE.md) |

## Problem and goals

The README promises "standalone or distributed" and "clusters and HA included"; ARCHITECTURE.md
lists `role` in `cmd/nurtuo` and "leader loss (M4)" as a chaos test. None of it exists:

- `nurtuo role` is a stub (exit 1).
- Queues are in memory per process (`memory.New()` in `RunWith`), so a second runtime cannot share
  work and a restart loses queued messages.
- The control plane binds one session per runtime id ("newest connection wins"), and desired state
  is per runtime, so two processes with the same id fight and two ids get separate deployments.
- Listeners bind per process; there is no notion of "exactly one node runs this schedule".

Goals:

- A runtime group of N >= 1 nodes (3 for HA) deployed as one target: deployments, queues and
  listeners are defined once per group.
- Loss of one node of three: queued messages survive (JetStream R3), HTTP listeners keep serving on
  the remaining nodes behind a load balancer, singleton work (schedules, single-consumer ordered
  queues, polling listeners) moves to another node within 30 s.
- Roles to separate concerns on large installs; one binary, one config format.
- Outbound-only stays true: nodes dial the control plane; nodes talk to each other only on the
  group's cluster port, inside the customer network.

Non-goals: multi-region active/active, autoscaling, Kubernetes operator (Helm chart is separate),
control plane HA (separate spec).

## Design (proposed)

### Group and node identity

- `runtime.yaml` gains `group_id` (default: `runtime_id`, so today's configs are a group of one) and
  `cluster: {listen, peers[], name}` for the embedded NATS cluster routes, with its own mTLS
  (`cluster.cert_file`, `cluster.key_file`, `cluster.ca_file`).
- Node identity stays the certificate (`runtime_id`); the control plane registers nodes under a
  group. Certificate URI SAN `spiffe://nurtuo/runtime/<id>` plus optional `spiffe://nurtuo/group/<group>`.

### Roles

| Role | Runs | Needs |
|---|---|---|
| `all` (default; what `nurtuo run` does) | everything below | |
| `queue` | embedded NATS JetStream server, cluster member | disk, cluster port |
| `worker` | engine, queue consumers, connector instances, bridges | a `queue` node in the group |
| `ingress` | HTTP and connector listeners, publishing into queues or running flows directly | a `worker` or `all` node for flows without queue |

`nurtuo role <name> [--config P]` runs one role; `nurtuo run` equals `nurtuo role all`. A
`roles: [...]` key in `runtime.yaml` is the service-friendly equivalent.

### Desired state and reconcile

- The control plane stores desired state per **group**; every node receives the same
  `DesiredState` (additive proto fields: `Hello.group_id = 6`, `Hello.roles = 7`).
- Each node applies the parts its roles cover. Deployment statuses are reported per node; the
  control plane aggregates (a deployment is `running` when every required role is running on at
  least one node, `degraded` when below the configured minimum).
- Revision rule unchanged per node.

### Singleton work and leader election

- Leases in a JetStream KV bucket `nurtuo_leases` (TTL 15 s, renewed every 5 s): key
  `<deployment>/<flow>` for schedule starts, ordered queues with one consumer, polling listeners.
- The holder runs the work; others wait. On lease loss the holder stops the work before the TTL
  expires (fencing by revision of the KV entry).

### Queues

- Wire `queue/natsjs` into the runtime process (prerequisite, also fixes data loss on restart).
- In a group, queues are JetStream streams with replicas = min(3, number of `queue` nodes).

## Contracts

| Contract | Change | Stability |
|---|---|---|
| CLI `nurtuo role <all\|queue\|worker\|ingress> [--config P]` | replaces the stub | stable once shipped |
| `runtime.yaml` keys `group_id`, `roles`, `cluster.{listen,peers,name,cert_file,key_file,ca_file}` | new, optional | stable |
| `nurtuo.runtime.v1`: `Hello.group_id` (6), `Hello.roles` (7); `DeploymentStatus.state` value `degraded` | additive | stable |
| Control plane REST: runtime groups resource (`/v1/runtime-groups`), deployments targeting a group | additive | stable |
| KV bucket `nurtuo_leases`, stream naming | new | internal |

## Compatibility

Additive: a runtime without `group_id` is a group of one, so existing configs, certificates and
deployments keep working. An older control plane ignores the new Hello fields (protobuf) and
treats each node as a separate runtime; the runtime must then refuse to start a multi-node group
(it can detect the missing group acknowledgement) instead of splitting work silently.

## Security

- Cluster routes use their own mTLS with a CA separate from the gateway CA; no plaintext routes.
- Leases and queue data stay inside the customer network; the control plane sees only statuses.
- A node certificate for group A must not join group B: the control plane checks the group SAN.

## Testing (acceptance)

| Test | What it proves |
|---|---|
| `TestRoleCommandRunsAll` in `cmd/nurtuo` | `nurtuo role all` behaves as `nurtuo run`; unknown role exits 2 |
| `TestGroupOfThreeSurvivesNodeLoss` (e2e, three in-process runtimes with embedded NATS cluster) | stop one node mid-traffic: no message lost, HTTP served by the others, queue backlog drained |
| `TestScheduleRunsOnExactlyOneNode` | a schedule start fires once per tick across three nodes, and on another node after the leader is killed within 30 s |
| `TestLeaseFencing` | a paused (SIGSTOP) leader that resumes after its lease expired does not run the work |
| `TestOldControlPlaneRefusesGroup` | a node with `group_id` and peers against a control plane without group support refuses to start |
| control plane `gateway.test.ts`: group desired state and status aggregation | `degraded` when a role is missing |
| Chaos workflow "leader loss (M4)" from ARCHITECTURE.md | repeated leader kills under load |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation |
|---|---|---|---|---|
| 1 | Network partition 2+1: the single node keeps its lease until TTL and both sides run a schedule for up to 15 s | medium | leases | fencing via KV revision on every side effect is impossible for external targets; document the window and keep TTL short |
| 2 | Operators enable roles before `queue/natsjs` is wired and lose messages on restart exactly as today | high | queues | ship JetStream wiring first; refuse `worker`/`ingress` roles without a `queue` node |
| 3 | The README keeps promising HA while this is draft | medium | docs | mark HA as planned in README until the acceptance tests pass |

## Open questions

- Do `ingress` nodes run flows locally when the start step is not followed by a queue, or must every
  listener publish into a queue? Proposed: local by default, queue when the flow says so.
- Should groups be visible in `nurtuo.bundle/v1` (target constraints), or stay deployment metadata?

## References

- Open Mercato: `lazy-auto-spawn-queue-workers`, `lazy-auto-spawn-scheduler` (singleton scheduler
  and worker placement).

## Changelog

- 2026-11-22: created (draft).
