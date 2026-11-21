<!-- SPDX-License-Identifier: Apache-2.0 -->
# ADR 0007: Queue dead-lettering with a JetStream advisory for server-side exhaustion

Date: 2026-11-21. Status: accepted (`core/runtime/internal/queue/natsjs`).

**Context.** Queues are push-based, backed by embedded NATS JetStream. A message that keeps failing
must end up in a dead letter queue (DLQ) that operators can list and replay. Handlers can settle a
delivery explicitly (`Ack`, `Nak` with delay, `Term`). But a handler can also crash or hang; then
the message is never settled, JetStream redelivers it after the ack wait, and when `MaxDeliver` is
reached the server just stops delivering it. Without handling, such a message stays in the work
queue stream, invisible to consumers and to the DLQ.

**Decision.**

- Each queue is two streams: `Q_<name>` (subject `nq.<name>`) and `Q_<name>_DLQ` (subject
  `nqdlq.<name>`). The DLQ name is always derived from the queue name.
- Explicit paths: when a delivery is nacked and the attempt count has reached `MaxDeliveries`
  (default 5), or it is terminated, the backend copies the message to the DLQ with headers for the
  reason and attempts and then terminates it in the source stream.
- Server-side path: the backend subscribes to
  `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>`. On an event for one of its streams it reads the
  message by stream sequence, publishes it to the DLQ (reason "max deliveries exceeded (ack
  timeout)", attempts from the event) and deletes it from the source stream. If the message is
  already gone (dead-lettered through the explicit path) the handler does nothing.
- DLQ messages keep the original properties, key and id (`Nurtuo-*` headers) so
  `ReplayDeadLetter` can republish them and remove the DLQ entry.

**Consequences.**

- No message is silently stranded after retries are exhausted, whether the handler failed loudly or
  not; the DLQ is the single place to inspect and replay.
- Advisories are delivered at most once and best effort. If the DLQ publish fails the message is
  left in place and visible only via queue stats; there is no retry of the advisory handler.
- The DLQ is limited by the same `MaxAge`/`MaxBytes` as the queue (the DLQ stream config copies
  them), so old dead letters can expire.
- The design couples to JetStream's advisory subject; another backend (Kafka, Postgres) must
  provide an equivalent through the `QueueBackend` interface.
