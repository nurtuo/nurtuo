// SPDX-License-Identifier: AGPL-3.0-only

package bench

import (
	"context"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
)

// QueuePickup adapts a queue.QueueBackend to PickupBench: Publish goes through the backend and
// Subscribe uses its push subscription, acking after the callback returns.
type QueuePickup struct {
	Backend     queue.QueueBackend
	Queue       string
	Concurrency int
}

// NewQueuePickup ensures the queue exists and returns the adapter.
func NewQueuePickup(ctx context.Context, b queue.QueueBackend, name string, concurrency int) (*QueuePickup, error) {
	if err := b.Ensure(ctx, queue.QueueDef{Name: name, Retention: queue.RetentionWorkQueue}); err != nil {
		return nil, err
	}
	return &QueuePickup{Backend: b, Queue: name, Concurrency: concurrency}, nil
}

// Publish publishes payload as one message.
func (q *QueuePickup) Publish(ctx context.Context, payload []byte) error {
	return q.Backend.Publish(ctx, q.Queue, queue.Message{Data: payload})
}

// Subscribe starts push delivery; fn runs before the ack.
func (q *QueuePickup) Subscribe(ctx context.Context, fn func([]byte)) (func(), error) {
	sub, err := q.Backend.Subscribe(ctx, q.Queue, queue.SubOpts{Durable: "bench", Concurrency: q.Concurrency},
		func(ctx context.Context, d queue.Delivery) {
			fn(d.Message().Data)
			_ = d.Ack(ctx)
		})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Close() }, nil
}

var _ PickupBench = (*QueuePickup)(nil)
