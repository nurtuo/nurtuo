<!-- SPDX-License-Identifier: Apache-2.0 -->
# NATS connector (`nats`)

Native Go connector for core NATS and JetStream, on `nats.go`. Source: `connectors/nats` (the
descriptor is built in `Describe`; there is no `connector.yaml` or `schemas/` directory yet).
Version `0.1.0`, host `go-native`, not streaming.

This is the connector for talking to an **external** NATS server. The runtime's own queues use an
embedded JetStream (see [concepts.md](../concepts.md#queues)), which is separate.

Status: tested against an embedded NATS server in the connector tests and registered in the `nurtuo` binary, so a deployed flow can use it through a bundle connection (secrets resolved from the runtime secret store, see [operations.md](../operations.md)). Not run against a production deployment.

## Operations

| Operation | Behaviour |
|---|---|
| `CREATE` | core NATS publish (fire and forget; flushed before success) |
| `EXECUTE` custom type `REQUEST` | request/reply; the reply is an output document `<id>-reply` |
| `EXECUTE` custom type `JETSTREAM_PUBLISH` | publish and wait for the JetStream ack |

Subject: document property `nats.subject`, else step option `subject`. Wildcards (`*`, `>`) and
whitespace are rejected (`APPLICATION_ERROR`). Document properties prefixed `nats.header.` become
NATS headers (CR, LF and `:` in names, CR/LF in values are rejected). For `JETSTREAM_PUBLISH` the
`Nats-Msg-Id` header defaults to the document id so retries of the same document deduplicate in
the stream. Step options: `subject`, `timeout_ms` (request and ack timeout, default 5000).

`Listen` (subscribe) is **not implemented**: the Go connector SDK interface has no `Listen`
method yet. `Browse` is not supported.

## Connection settings

| Setting | Type | Notes |
|---|---|---|
| `url` | string (required) | `nats://host:4222`, comma separated for a cluster |
| `token` | secret | |
| `username`, `password` | string, secret | set together |
| `creds` | secret | content of a `.creds` file (JWT + NKey seed) |
| `nkey_seed` | secret | user NKey seed |
| `name` | string | connection name, default `nurtuo` |
| `connect_timeout_ms` | integer | default 5000 |
| `tls.enabled` | boolean | default true when a `tls` object is present |
| `tls.ca_pem`, `tls.cert_pem`, `tls.key_pem` | string | `cert_pem` and `key_pem` together (mutual TLS) |
| `tls.server_name` | string | |
| `tls.insecure_skip_verify` | boolean | development only |

Use at most one of `token`, `username`/`password`, `creds`, `nkey_seed`. TLS minimum version is
1.2. The connection is created per Execute batch (no reconnect; the runtime owns retries).

## Tracked properties

`subject`, `stream`, `sequence` (the last two for JetStream publishes).

## Status mapping

| Outcome | Status |
|---|---|
| publish, reply or ack received | `SUCCESS` |
| missing/invalid subject, wildcard subject, invalid header, no responders, no JetStream stream captures the subject | `APPLICATION_ERROR` |
| timeout, publish or flush error, other errors | `FAILURE` |
| invalid settings (`InvalidArgument`), connection failure (`Unavailable`), unsupported operation (`Unimplemented`), batch too large (`ResourceExhausted`) | the whole stream fails |

## Security notes

- Prefer `creds` or `nkey_seed` and TLS; `token` and `password` travel inside the connection setup.
- `tls.insecure_skip_verify` disables server verification; do not use it in production.
- The subject is chosen by the document or the step: a flow that lets untrusted input set
  `nats.subject` can publish to any subject the credentials allow. Scope the NATS user's
  permissions to the subjects the flow needs.
- Secrets are redacted from error messages and diagnostics.

## Limits

The batch is buffered before connecting: at most 64 MiB and 100,000 documents per Execute
(`ResourceExhausted` beyond that). NATS' own maximum message size applies per message.
