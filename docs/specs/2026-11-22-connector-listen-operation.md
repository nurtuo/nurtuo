<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Connector `Listen` operation (push listeners through connectors)

Status: draft. Date: 2026-11-22.

## TLDR

Make `ConnectorService.Listen` real end to end: an optional `connector.Listener` interface in the
Go SDK, a `listener.Source` in the runtime that drives a connector's `Listen` stream with
per-document acks, `Listen` in the NATS connector and in the Python, JVM and .NET bridge SDKs, and
a `Listen` subtest in `contracttest`. The proto needs no breaking change; the semantics it leaves
open (document boundaries, ack timeout, redelivery, flow control, pause) are fixed here.

## Status and links

| | |
|---|---|
| Pull requests | none yet |
| ADRs | [ADR 0008](../adr/0008-listener-source-abstraction.md) (the `Source` abstraction this plugs into); a new ADR is needed for the ack/redelivery semantics |
| Code (today) | `proto/nurtuo/connector/v1/connector.proto` (`Listen`, `ListenStart`, `Ack`, `ListenResponse`), `sdk/go/connector/connector.go`, `sdk/go/connector/contracttest/contracttest.go` (`Listen` subtest is `t.Skip`), `connectors/nats/connector.go`, `core/runtime/internal/listener/factory.go` (`FromStart`), `bridges/jvm/nurtuo-bridge-sdk/src/main/java/io/nurtuo/bridge/sdk/ConnectorService.java` |
| User docs | [connector-authoring.md](../connector-authoring.md), [bridges/README.md](../../bridges/README.md) |

## Problem and goals

Inbound integrations (a JMS or IBM MQ queue, a NATS subject, a Kafka topic, later CDC) are the
other half of an integration platform. Today only the runtime's own HTTP listener and in-memory
queues can trigger a flow. Evidence:

- `connector.Connector` has no `Listen` method and `connector.Serve` embeds
  `UnimplementedConnectorServiceServer`, so `Listen` answers `UNIMPLEMENTED` for every Go connector.
- `connectors/nats/connector.go` package comment: "Listen (subscribe with ack/nack) is not implemented".
- The JVM SDK (`ConnectorService.java`) and the Python and .NET SDKs do not implement `Listen`.
- `contracttest.Run` skips `Listen` ("needs a connector that declares OPERATION_TYPE_LISTEN").
- `listener.FromStart` returns `listener: connector %q listeners are not supported by the factory`
  for any `{"type":"listener","connector":"<not http>"}` start step.

Goals:

- A flow whose start step is `{"type":"listener","connector":"nats","connection":"bus","options":{...}}`
  runs once per inbound document, with at-least-once delivery and per-document ack/nack.
- The same works for a bridged connector (JMS through the JVM bridge) without engine changes.
- Pause/resume, counters and status come from the existing `listener.NewSourceListener`.
- Backpressure: the connector never has more than `max_in_flight` unacknowledged documents.

Non-goals: exactly-once delivery; transactions spanning listener and target; polling listeners
(SFTP directory watch, scheduled queries) which belong to a `schedule` start; Kafka and MQTT
connectors themselves.

## Design (proposed)

### Wire semantics (no breaking proto change)

The existing messages are kept. Rules that the proto comment will state:

1. The client (runtime) sends exactly one `ListenStart{listener_id, connection, options,
   max_in_flight}` first. `max_in_flight == 0` means 1.
2. The server streams `ListenResponse{chunk}`. A document is the run of chunks with the same
   `document_id`, `properties` on the first chunk, `last=true` on the final one; documents are not
   interleaved. `document_id` must be unique per delivery attempt and stable across redeliveries of
   the same message when the source provides an id (JMS `JMSMessageID`, NATS `Nats-Msg-Id` or stream
   sequence).
3. For every complete document the client sends one `Ack{document_id, status}`: `SUCCESS` commits
   (JMS acknowledge, JetStream `Ack`), `APPLICATION_ERROR` commits and marks the message as handled
   by the flow's error path, `FAILURE` asks for redelivery (JetStream `Nak`, JMS session recover);
   redelivery limits and dead-lettering are the source system's.
4. The server must not send a new document while `max_in_flight` documents are unacknowledged.
5. An unacked document is redelivered by the source after its own timeout (JetStream `AckWait`,
   JMS session close). The runtime does not guarantee an ack after a crash.
6. The client ends the stream by cancelling it; the server stops receiving, leaves unacked documents
   to be redelivered and returns. A non-OK status from the server ends the listener; the runtime
   restarts it with backoff (1 s doubling to 60 s).
7. Connectors without listeners keep answering `UNIMPLEMENTED`; a connector that supports it
   declares an operation with `OPERATION_TYPE_LISTEN` in its descriptor.

Additive proto change (optional, decided in review): `ListenStart.ack_timeout_ms` so the runtime
can tell the connector its expected processing bound.

### Go SDK

```go
// In sdk/go/connector: optional, so existing implementations keep compiling.
type Listener interface {
    Listen(stream connectorv1.ConnectorService_ListenServer) error
}
```

`connector.Serve` checks `c.(Listener)` and forwards `Listen`; otherwise `UNIMPLEMENTED` as today.
A helper `connector.ListenLoop(stream, func(ctx, start) (<-chan Inbound, error))` handles chunking,
`max_in_flight` and ack routing so connector authors write only the subscribe part.

### Runtime

- `listener.ConnectorSource{Lookup, FlowID, Start}` implements `Source.Consume(ctx, deliver)`:
  resolve the connector instance and connection through `connhost.Manager.Lookup`, open `Listen`
  (in process through a `bufconn` pair for native connectors, or the bridge's client), send
  `ListenStart`, assemble each document into a `docs.Set` of one document (spooled through
  `docs.Spool` like HTTP bodies), call `deliver`, then send `Ack` (`nil` error and no failed
  document: `SUCCESS`; failed documents only by business error: `APPLICATION_ERROR`; otherwise
  `FAILURE`).
- `listener.FromStart` builds a `NewSourceListener(&ConnectorSource{...})` for
  `{"type":"listener","connector":X}` when `X` declares `OPERATION_TYPE_LISTEN`; otherwise the
  deploy fails with `connector X does not support listeners` (validated at `Apply`, not at the
  first message).
- Pause: the `NewSourceListener` pause gate holds `deliver`; because no ack is sent, the connector
  stops after `max_in_flight` documents. Resume continues. No message is lost or acked while paused.
- Status: counters from `NewSourceListener`; `listener_id` = `<deployment>/<flow>`.

### Connectors and bridges

| Implementation | Source mechanism | Ack mapping |
|---|---|---|
| `connectors/nats` | JetStream durable pull consumer (`options.stream`, `options.consumer`), or core subscription when `options.jetstream=false` (no redelivery: `FAILURE` is logged) | `Ack` / `Nak` / `Term` for `APPLICATION_ERROR` when configured |
| JVM SDK + `nurtuo-connector-jms` | `MessageConsumer` on a `CLIENT_ACKNOWLEDGE` session per in-flight slot | `acknowledge()` / `session.recover()` |
| Python SDK | `Connector.listen(ctx, start) -> AsyncIterator[InboundDocument]` with `ack` callbacks | connector-defined |
| .NET SDK | `ConnectorBase.ListenAsync` | connector-defined |

## Contracts

| Contract | Change | Stability after this spec |
|---|---|---|
| `Listen` semantics (rules 1 to 7 above) | documented in the proto comment | stable |
| `ListenStart.ack_timeout_ms` (if accepted) | additive field 5 | stable |
| Go `connector.Listener`, `connector.ListenLoop` | new | experimental |
| Flow start step `{"type":"listener","connector":X,"connection":Y,"options":{...}}` for non-HTTP connectors | `connection` and `options` are new optional properties of `startConfig` in `sdk/ts/flow-schema/schema/flow.v1.schema.json` (today `additionalProperties: false`) and of `flow.StartConfig` in `sdk/go/flow/flow.go` | stable, additive to `nurtuo.flow/v1` |
| NATS listener options `stream`, `consumer`, `subject`, `jetstream` | new | experimental |
| `contracttest.Options.Listen` (`ListenCase`) | new | experimental |

## Compatibility

Additive only: no message, field or RPC changes meaning; `buf breaking` stays green. The flow
format gains two optional start properties; a flow that uses them is rejected by an older
validator (`additionalProperties: false`), so the canvas and `nurtuoctl` must ship the new schema
before such flows are authored (ADDITIVE-ONLY class in the draft spec
`backward-compatibility-contract`). Existing Go
connectors compile unchanged because `Listener` is a separate optional interface. A runtime with
this feature talking to an older bridge gets `UNIMPLEMENTED` and fails the deployment with a clear
message instead of starting a listener.

## Security

- The connection's secrets are injected in `ListenStart.connection` like in `Execute`; the
  `connhost.redacting` wrapper must also wrap the Listen path (errors, status).
- Inbound document size: chunks are streamed to the spool; a per-document cap (default 64 MiB,
  configurable per listener) rejects larger messages with `FAILURE` and a log line, never OOM.
- A listener cannot be started by flow input; only deployed flows start listeners.

## Testing (acceptance)

| Test | What it proves |
|---|---|
| `contracttest.Run` subtest `Listen` with `Options.Listen{Start, Trigger func(n int), Want int}` | a connector declaring `OPERATION_TYPE_LISTEN` delivers `Want` documents, never exceeds `max_in_flight` unacked, redelivers after `FAILURE`, ends cleanly on cancel; skipped (not failed) when the connector does not declare LISTEN |
| `TestNATSListenAckNak` in `connectors/nats` (embedded `nats-server`) | JetStream ack and nak, redelivery count |
| `TestConnectorSourcePauseHoldsAcks` in `core/runtime/internal/listener` | pause sends no ack and receives at most `max_in_flight` documents |
| `TestFromStartRejectsConnectorWithoutListen` | deploy-time error for a connector without LISTEN |
| `TestPythonListenBridgeConforms` in `connectors/bridge` | the Python SDK passes the `Listen` subtest through the launcher (`NURTUO_TEST_PYTHON`-gated like today) |
| JVM `JmsConnectorListenTest` (ActiveMQ Artemis embedded) | JMS acknowledge/recover; requires the `bridge-jvm` CI job |
| e2e `TestNATSListenerFlow` in `core/runtime/internal/e2e` | a deployed flow triggered by a NATS message calls an HTTP mock once per message |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation |
|---|---|---|---|---|
| 1 | The runtime crashes after the flow succeeded but before the `Ack` reached the connector; the message is redelivered and the target receives it twice | high | semantics | document at-least-once; flows writing to targets use idempotency keys (document id) |
| 2 | A slow flow keeps documents unacked longer than JetStream `AckWait`; the server redelivers while the first attempt still runs | medium | NATS | set `AckWait` from `ack_timeout_ms`, send `InProgress` while processing |
| 3 | Document ids are reused across executions (listener counters `http-<n>`, `q-<n>` restart at 1 per process, finding from the native connector specs), so source ids must be preferred and generated ids must include a process-unique prefix | medium | runtime | use source message ids; generated ids get a random per-process prefix |
| 4 | The JVM bridge still has no CI job; the JMS listener would ship untested | high | CI | `bridge-jvm` job is a prerequisite of the JMS part |

## Open questions

- Should `APPLICATION_ERROR` ack or dead-letter? Proposed: ack, with the flow's error path
  responsible; configurable per listener later.
- One `Listen` stream per listener or per in-flight slot for JMS? Proposed: one stream, the JVM SDK
  multiplexes sessions.

## References

- Open Mercato: `webhooks-module` (inbound event handling and acknowledgement),
  `inbound-webhook-handlers`.

## Changelog

- 2026-11-22: created (draft).
