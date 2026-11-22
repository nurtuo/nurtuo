<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: NATS connector

Status: implemented. Date: 2026-11-21.

## TLDR

`connectors/nats` is the native Go connector (on `nats.go`) for an **external** NATS server: core
publish (`CREATE`), request/reply (`EXECUTE`/`REQUEST`) and JetStream publish with ack
(`EXECUTE`/`JETSTREAM_PUBLISH`). The one thing to know: it only sends. `Listen` (subscribe) is not
implemented, and JetStream deduplication keys on the document id, which is not unique across
executions.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/31 (squash commit `2251a82`); https://github.com/nurtuo/nurtuo/pull/41 (open, renames the proto message `Descriptor` to `ConnectorDescriptor`, touches `Describe`) |
| ADRs | [ADR 0005](../../adr/0005-set-at-a-time-engine-with-per-document-status.md), [ADR 0010](../../adr/0010-connections-and-secrets.md); the runtime's own queues (embedded JetStream, [ADR 0007](../../adr/0007-queue-dlq-via-advisory.md)) are separate |
| Issues | none |
| Code | `connectors/nats/` (`connector.go`, `config.go`, `connector.yaml`); registered in `core/runtime/internal/connhost/registry.go` (`Builtins`) |
| User docs | [docs/connectors/nats.md](../../connectors/nats.md) |

## Problem and goals

Event-driven integrations hand messages to NATS subjects and JetStream streams, and call services
over request/reply. Flows need to publish documents with headers, get an at-least-once ack from
JetStream, and use a reply as the next document.

- Goals: core publish, request/reply with timeout, JetStream publish with ack and stream sequence,
  token / user-password / creds / NKey auth, TLS and mTLS, per-document subject and headers,
  deduplication of retried JetStream publishes, bounded batch buffering.
- Non-goals (M1): subscribe / consume (`Listen`), KV and object store, stream and consumer
  management, `Browse`, long-lived connections.

## Design (as built)

**Descriptor.** `(*Connector).Describe` in `connectors/nats/connector.go`: id `nats` (`ID`),
version `0.1.0` (`Version`), host `HOST_GO_NATIVE`, display name `NATS`, operations `CREATE`,
`EXECUTE`/`REQUEST` (`CustomRequest`) and `EXECUTE`/`JETSTREAM_PUBLISH` (`CustomJetStreamPub`),
each with the options schema `subject`, `timeout_ms`; tracked properties `subject`, `stream`,
`sequence`; `Capabilities{Streaming: false}`. The connection schema is built inline in `Describe`;
`connector.yaml` lists the fields (`schema: inline`). `Browse` returns
`connector.ErrNotImplemented`.

**Connection settings** (`parseConfig` in `config.go`; string values are trimmed):

| Key | Default | Notes |
|---|---|---|
| `url` (required) | | `nats://host:4222`, comma separated for a cluster |
| `token` | | `format: secret` |
| `username`, `password` | | set together; `password` is `format: secret` |
| `creds` | | content of a `.creds` file; JWT and seed extracted by `extractBlock` |
| `nkey_seed` | | user seed; `nkeys.FromSeed`, signs the nonce |
| `name` | `nurtuo` | connection name |
| `connect_timeout_ms` | 5000 | `nats.Timeout` |
| `tls` object | absent → no TLS options | present → `enabled` defaults to true |
| `tls.ca_pem`, `tls.cert_pem`, `tls.key_pem`, `tls.server_name`, `tls.insecure_skip_verify` | | `cert_pem` and `key_pem` together; `key_pem` is `format: secret`; `MinVersion` TLS 1.2 |

At most one of `token`, `username`/`password`, `creds`, `nkey_seed`. `Config.options` always adds
`nats.NoReconnect()`: one short-lived connection per `Execute` batch and per `Test`; the runtime
owns retries. `connectCtx` makes the blocking `nats.Connect` cancellable (a late connection is
closed).

**Test.** Connects and reports the server version, then `jetstream.AccountInfo` to add
"JetStream available" or "not available". A failed connect is `Ok: false` with a redacted
diagnostic.

**Execute.** The first message must be `ExecuteStart`; invalid settings → `codes.InvalidArgument`,
an unsupported operation → `codes.Unimplemented`. Options: `subject` (default subject),
`timeout_ms` (request and ack timeout, default `defaultRequestTimeout` = 5 s). The whole batch is
buffered **before connecting**, bounded by `maxBatchBytes` = 64 MiB and `maxBatchDocs` = 100 000
(`codes.ResourceExhausted` beyond). Then one connection is opened (`codes.Unavailable` on failure)
and documents are processed in arrival order:

1. Subject: property `nats.subject` (`PropSubject`), else option `subject`. Empty or whitespace,
   or containing `*` / `>`, is `APPLICATION_ERROR`.
2. Headers: properties `nats.header.<name>` (`PropHeaderPrefix`); CR/LF or `:` in a name, CR/LF in
   a value is `APPLICATION_ERROR`.
3. `JETSTREAM_PUBLISH` sets `Nats-Msg-Id` to the **document id** unless the document provides one,
   so a retried document is deduplicated by the stream.
4. `send`: `CREATE` → `PublishMsg` + `FlushWithContext` (the server received it; no subscriber is
   required). `REQUEST` → `RequestMsgWithContext`; the reply payload becomes output document
   `<id>-reply` (one chunk, no properties). `JETSTREAM_PUBLISH` → `js.PublishMsg`; tracked
   `stream`, `sequence`. Tracked `subject` always.

**Status mapping** (`send`, `Execute`):

| Outcome | Status |
|---|---|
| flushed publish, reply received, JetStream ack | `SUCCESS` |
| missing/invalid/wildcard subject, invalid header | `APPLICATION_ERROR` |
| `nats.ErrNoResponders` (request), `jetstream.ErrNoStreamResponse` (no stream captures the subject) | `APPLICATION_ERROR` |
| timeout, publish/flush error, any other error | `FAILURE` |
| invalid settings / unsupported operation / batch too large / connection failure | whole stream fails (`InvalidArgument` / `Unimplemented` / `ResourceExhausted` / `Unavailable`) |

## Contracts

| Contract | Stability |
|---|---|
| Connector id `nats`, version `0.1.0`, the three operations above | experimental |
| Connection keys (inline schema in `Describe`) | experimental |
| Step options `subject`, `timeout_ms` | experimental |
| Document properties `nats.subject`, `nats.header.<name>` | experimental |
| Default `Nats-Msg-Id` = document id for JetStream publishes | experimental (see Gaps 1) |
| Output document `<id>-reply` | experimental |
| Tracked properties `subject`, `stream`, `sequence` | experimental |
| Exported constants `ID`, `Version`, `CustomRequest`, `CustomJetStreamPub`, `PropSubject`, `PropHeaderPrefix` | internal |

## Compatibility

No contract change in this spec. Open https://github.com/nurtuo/nurtuo/pull/41 changes the return
type of `Describe` to `*connectorv1.ConnectorDescriptor`; wire format unchanged. Update this spec
when it merges.

## Security

- **Auth.** Token, user/password, creds (JWT + NKey) or NKey seed; mixing them is a configuration
  error. Secrets are marked `format: secret` and recognised by `connections.SecretPaths`.
- **TLS.** Optional: without a `tls` object the client connects in plain text unless the URL uses
  `tls://` or the server requires TLS. `tls.insecure_skip_verify` is an explicit opt-in; TLS 1.2
  minimum; mTLS via `cert_pem`/`key_pem`.
- **Redaction.** `redact` masks `token`, `password`, `creds`, `nkey_seed`, `tls.key_pem` in connect
  errors and diagnostics; certificate and key parse errors use fixed messages. Per-document
  `FAILURE` messages (`fail(err)`) are not passed through `redact` in the connector and rely on the
  runtime `redacting` wrapper.
- **Subject injection.** A document may choose any subject the credentials allow; wildcards are
  refused. Scope the NATS user's publish permissions.
- **Limits.** 64 MiB / 100 000 documents per batch; NATS `max_payload` per message.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestContract` in `connectors/nats/connector_test.go` | shared suite against an embedded `nats-server` with JetStream (`startServer`): `publish` (no subject → `APPLICATION_ERROR`), `request-reply` (no responders → `APPLICATION_ERROR`), `jetstream-publish` (no stream → `APPLICATION_ERROR`) | `cd connectors && GOWORK=off go test -race ./nats/...` | no external server needed |
| `TestTestConnection` | Test ok / token auth / failure diagnostics without secrets | same | |
| `TestPublishAndReplyOutput` | published payload and headers arrive, reply becomes `<id>-reply` | same | |
| `TestRejectsWildcardSubjectsAndBadHeaders` in `findings_test.go` | wildcard subjects, CR/LF headers | same | |
| `TestJetStreamPublishDedupesByDocumentID` | two publishes of `doc-1` store one message | same | |
| `TestTestHonoursContext` | Test returns when the context is cancelled | same | |
| `TestBatchBufferingBounded` | `ResourceExhausted` above the batch bounds | same | |

Not covered: TLS/mTLS, creds and NKey authentication, cluster URLs. Contract-suite `Listen`,
`ExecuteLargePayload` and `CrashBehaviour` are skipped.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Document ids are not unique across executions: the HTTP listener names documents `http-<n>` from a per-process counter (`core/runtime/internal/listener/http.go`, `l.seq`) that restarts at 1, and each listener has its own counter; queue deliveries without an id become `q-<n>` (`core/runtime/internal/listener/factory.go`). Two different orders arriving as `http-1` (after a restart, or on two listeners) within the stream's duplicate window (default 2 minutes) are published with the same `Nats-Msg-Id`; JetStream stores only the first, and the connector reports `SUCCESS` for the second because `ack.Duplicate` is not checked. Silent data loss. | high | correctness | Default `Nats-Msg-Id` to `execution_id` + `step_id` + document id; report `ack.Duplicate` as a tracked property (`duplicate=true`) and document the behaviour. |
| 2 | Flows cannot start from NATS: no subscribe/consume (`Listen`). The Go SDK `connector.Connector` has no `Listen` method, `connector.Serve` does not register it, and the contract suite skips `Listen`. Users bridge through an HTTP listener or the runtime's internal queues. | medium | functionality | Add `Listen` to the SDK interface and the contract suite, then a JetStream pull-consumer `Listen` with ack/nak mapped from document status. |
| 3 | A connection with `url: nats://broker:4222`, a `password`, and no `tls` object sends the password and every payload in clear text; `Test` reports success without a warning. | medium | security | Warn in the `Test` diagnostic when TLS is off and credentials are set; consider requiring an explicit `tls.enabled: false`. |
| 4 | A `CREATE` publish to `orders.created` with no subscriber (consumer down) is flushed and reported `SUCCESS`; the message is gone. | low | correctness (by design) | Documented as fire-and-forget; recommend `JETSTREAM_PUBLISH` for anything that must not be lost. |
| 5 | A batch of 70 MiB fails the whole stream with `ResourceExhausted` and no per-document results; every document is retried or failed together. | low | reliability | Let the engine split batches using an advertised limit (`Capabilities.max_batch_size` / bytes), or process documents incrementally. |
| 6 | A request/reply service returns status headers or a reply subject; the `<id>-reply` document has no properties, so headers are lost to the flow. | low | functionality | Copy reply headers to `nats.header.*` properties on the output document. |
| 7 | A flow runs one small batch per second: each batch opens a new TCP + TLS + auth connection (`NoReconnect`, closed at the end), adding latency and load on the server. | low | performance | Cache connections per settings hash with a health check, as the Postgres connector does with pools. |
| 8 | `docs/connectors/nats.md` says there is "no `connector.yaml`", but `connectors/nats/connector.yaml` exists. | low | docs | Fix the user doc. |

## Improvement opportunities

- `Listen` via JetStream durable pull consumers (ack on `SUCCESS`, nak with delay on `FAILURE`,
  term on `APPLICATION_ERROR`) and core subscriptions with queue groups.
- JetStream publish options: expected stream, expected last sequence (optimistic concurrency),
  per-message TTL.
- KV `GET`/`PUT` and object store operations.
- `Browse` of streams and subjects for the canvas.
- Connection reuse across batches; cluster discovery options.
- One schema style for secrets across connectors (`x-nurtuo-secret` instead of `format: secret`).

## References

- Open Mercato `webhook-hub`: outbound event delivery with retries and deduplication; ideas for
  idempotency keys that are unique per delivery, not per payload position.

## Changelog

- 2026-11-22: created (as built, from `main` at `3d3d506`).
