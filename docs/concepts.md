<!-- SPDX-License-Identifier: Apache-2.0 -->
# Concepts

The vocabulary of Nurtuo, as implemented in the code today. Where the design (see
[ARCHITECTURE.md](ARCHITECTURE.md)) is ahead of the code, this page says so.

## Flow

A flow is one integration process, stored as a small JSON document with `schema:
"nurtuo.flow/v1"`: an `id`, a `name`, an integer `revision`, a list of `steps` and a map of
`edges`. It holds logic only; canvas positions are stored separately. The JSON Schema
(`sdk/ts/flow-schema/schema/flow.v1.schema.json`) is the source of truth; the TypeScript
validator and the Go validator must agree on every file of a shared corpus. See
[flow-reference.md](flow-reference.md) and [flow-validation.md](flow-validation.md).

## Steps, ports and edges

A **step** has an `id` (lowercase, `^[a-z][a-z0-9_-]*$`), a `kind` and a `config` object whose
shape depends on the kind. The 15 kinds are `start`, `connector`, `queue`, `map`, `script`, `message`,
`setProperties`, `decision`, `branch`, `route`, `tryCatch`, `processCall`, `notify`, `stop` and
`returnDocuments`.

A step has named output **ports**: most steps have `default`; `decision` has `true` and `false`;
`branch` has `1`..`paths`; `route` has one port per route id plus `default`; `tryCatch` has `try`
and `catch`; `stop` and `returnDocuments` have none (they are terminal).

**Edges** are a map `"stepId:port" -> targetStepId`. One outgoing edge per port is
representable by construction (a map key is unique). Every port must be connected, so every
path ends in a terminal step, and a flow must be acyclic. These are validation rules, covered in
[ADR 0002](adr/0002-edges-as-step-port-map.md) and [ADR 0003](adr/0003-cycles-are-errors.md).

```json
"edges": { "start:default": "get_employee", "found:true": "to_response", "found:false": "not_found" }
```

## Documents and properties

The engine processes **documents**. A document is `{id, props, payload, status}`: a string id,
a `props` map of string to string (the properties), a payload that is a stream of bytes (spilled
to disk above a threshold) and a status: `ok`, `error` or `dropped`.

**Document properties** travel with one document (for example `http.request.method`, `http.request.header.*`,
`queue.key`, `tracked.status_code`). **Process properties** are shared by all documents of one
execution and are set by `setProperties` with `scope: "process"`. Connectors can also report
**tracked properties** (declared in the connector descriptor, for example `status_code` and `url`
for HTTP): the engine exposes them on the document as `tracked.<name>`.

The engine is **set-at-a-time**: documents at a step form a set and the step processes the set as a
whole. `decision` and `route` split the set across ports, `branch` runs its paths one after
another over copies of the set. A document that fails is marked `error` and is routed to the
nearest enclosing `tryCatch` (or ends up in the result's failed set); other documents continue.

## Conditions, templates and maps

`decision` and `route` take a boolean condition (the schema calls it CEL). `message` and
`setProperties` take `{{operand}}` templates. `map` takes a JSONata expression over the JSON
payload. What each evaluator supports in the runtime today is in
[flow-reference.md](flow-reference.md) (decision, setProperties and map sections).

## Connectors and connections

A **connector** adapts an external system. All connectors, whether native Go, bridged (JVM, Python,
.NET; bridge SDKs and a Go launcher exist, see [../bridges/README.md](../bridges/README.md)) or WASM (planned for connectors),
speak one gRPC contract, `nurtuo.connector.v1`
(`proto/nurtuo/connector/v1/connector.proto`): `Describe`, `Browse`, `Execute` (stream of
document chunks in, a result per document out), `Test` and `Listen`. A connector is described by a
**descriptor** (id, version, host, operations, connection JSON Schema, tracked properties,
capabilities), mirrored in `connector.yaml`.

An **operation** is one of `GET`, `QUERY`, `CREATE`, `UPDATE`, `UPSERT`, `DELETE`, `EXECUTE` (with
a custom type such as `REQUEST`) or `LISTEN`. A **connection** is a connector's settings
(host, credentials, ...); secrets are references resolved by the runtime and injected in memory.
A connector step references `connector`, `operation` and optionally `connection` by name.

Per-document status is `SUCCESS`, `APPLICATION_ERROR` (the target rejected the document) or
`FAILURE` (technical). Both error statuses fail the document in the engine. The four native
connectors are documented in [connectors/](connectors/http.md): [http](connectors/http.md),
[sftp](connectors/sftp.md), [postgres](connectors/postgres.md), [nats](connectors/nats.md).
To write your own see [connector-authoring.md](connector-authoring.md).

## Queues

A **queue** is a deployable component behind the `QueueBackend` interface
(`core/runtime/internal/queue`): `Ensure`, `Publish`, `Subscribe` (push, never poll), `Stats`,
dead-letter listing and replay. Retention is `workqueue` (default), `limits` or `interest`;
ordering `none`, `per-key` or `strict`; `MaxDeliveries` defaults to 5. The default backend is
embedded NATS JetStream (`queue/natsjs`): each queue is stream `Q_<name>` plus a dead letter stream
`Q_<name>_DLQ`. A message that is nacked until max deliveries, or terminated, is copied to the
DLQ; a message that exhausts deliveries server-side (ack timeout, crash) is caught through the
JetStream max-deliveries advisory ([ADR 0007](adr/0007-queue-dlq-via-advisory.md)). There is also
an in-memory backend used for tests and as the placeholder in the runtime process.

## Listeners and triggers

A **listener** is a push trigger that feeds documents to a flow: the HTTP listener (`start` with
`type: "listener"`, `connector: "http"`) and the queue listener (`type: "queue"`). Listeners have
lifecycle states `stopped`, `running` and `paused` and count received and failed deliveries.
Subscribe-based transports implement the small `Source` interface and are wrapped into a
listener, which is how queue (and later AMQP, Kafka) listeners share pause/resume and ack/nack
logic ([ADR 0008](adr/0008-listener-source-abstraction.md)). A delivery is acked when the flow run
produced no failed documents and nacked otherwise.

## Runtime, supervisor and gateway

The **runtime** is the single static Go binary `nurtuo`. It runs flows. Its **supervisor**
reconciles the **desired state** (a list of deployments, each pointing at a bundle digest) against
what is actually deployed and reports status. The runtime reaches the control plane through the
**gateway**, one long-lived bidirectional gRPC stream that the runtime always dials (outbound only,
mTLS); the control plane never connects to a runtime. If the connection drops the runtime keeps
running its last applied state and reconnects with exponential backoff (1 s up to 60 s).

## Control plane

The **control plane** (TypeScript, `core/control-plane`) stores components (`flow`, `map`,
`connector`, `script`) and their immutable revisions (numbered by the server, digest = sha256 of
canonical JSON), tracks runtimes, and turns a deploy request into a new desired-state generation
plus an outbox row in one transaction. It exposes a REST API (`core/control-plane/openapi.yaml`)
and uses Postgres (or an in-memory repository when `DATABASE_URL` is unset). The MCP server
(`core/mcp`) gives AI agents the tools `search`, `get_flow`, `apply_ops`, `validate` and
`dry_run`; the canvas (`core/web`) edits flows through the same commands
(`addStep`, `removeStep`, `updateConfig`, `connect`, `disconnect`, `setLabel`, `renameStep`).

## Bundles

A **bundle** (`nurtuo.bundle/v1`) is the immutable, deterministic, signed deployment unit: a tar
with `manifest.json`, `config.json`, the component documents and a detached Ed25519 signature
`manifest.sig`. The control plane packs and signs; the runtime is meant to verify before use.
See [ADR 0006](adr/0006-bundle-format-and-domain-separated-signature.md) and
[security.md](security.md) for what is and is not verified today.

## Modes

- **Standalone** (`nurtuo standalone`): one host runs embedded Postgres, the control plane as a
  supervised child process, and the runtime.
- **Distributed**: control plane elsewhere, runtimes anywhere, connected by the outbound-only
  gateway. The service commands (`nurtuo service ...`) install the runtime as an OS service.
