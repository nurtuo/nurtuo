<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0008: Push listeners share a lifecycle through a minimal `Source` abstraction

Date: 2026-11-21. Status: accepted (`core/runtime/internal/listener`).

**Context.** A flow can be triggered by an HTTP request, a queue message, and later AMQP, Kafka,
MQTT, CDC or bridge listeners (JMS, IBM MQ). Each transport has its own consume loop, but operators
need the same controls everywhere: start, stop, pause and resume (a command from the control
plane), status (received, failed counts) and consistent ack/nack behaviour.

**Decision.** `Listener` is the lifecycle interface (`Start`, `Stop`, `Pause`, `Resume`,
`Status`; states `stopped`, `running`, `paused`) feeding a `Handler` that runs one delivery, a set
of documents, through a flow. Subscribe-style transports implement only
`Source.Consume(ctx, deliver)`: block, call `deliver` per message batch, return when the context is
cancelled; `deliver` returns nil for success (ack) or an error (nack). `NewSourceListener` adapts a
`Source` to a `Listener`, adding the state machine, a pause gate (deliveries wait, in-flight ones
finish) and counters. A delivery counts as failed if the handler errors or the result contains
failed documents. The queue listener is `QueueSource` over `QueueBackend.Subscribe`; the durable
consumer name defaults to `nurtuo-<queue>`. Request/response transports (the HTTP listener) implement
`Listener` directly, because the reply is part of the call. A `Manager` reconciles a list of desired
listeners (key, spec fingerprint, factory): new ones start, changed specs restart, removed ones stop,
and pause state follows the desired list. `FromStart` builds a listener from a `start` step config.

**Consequences.**

- A new push transport needs one `Consume` method and gets pause/resume, counters and uniform
  ack/nack semantics for free.
- A single failed document nacks the whole delivery; with queue deliveries of one document each
  (today's `QueueSource`) that means per-message retries and DLQ, but a multi-document delivery
  would be retried as a whole.
- HTTP semantics (status mapping, body limits) stay separate from the `Source` model.
- Bridge listeners (`Listen` RPC) and `schedule` starts are not built by the factory yet.
