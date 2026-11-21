// SPDX-License-Identifier: AGPL-3.0-only

package bench

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"
)

// PickupBench is the minimal surface the pickup scenario needs: publish a payload and get it
// pushed to a consumer (no polling interval).
//
// ADAPTER POINT: wrap queue.QueueBackend (core/runtime/internal/queue, feat/queues-backend) in a
// type implementing PickupBench (Publish -> backend publish; Subscribe -> backend Subscribe with
// a Handler that calls fn(payload) and acks) and pass it to RunQueuePickup. Until then the
// in-memory implementation below is used and measures only the harness + Go scheduling floor.
type PickupBench interface {
	Publish(ctx context.Context, payload []byte) error
	// Subscribe starts push delivery; fn is called once per message. Returns a stop func.
	Subscribe(ctx context.Context, fn func(payload []byte)) (stop func(), err error)
}

// MemoryPickup is an in-memory channel-backed PickupBench.
type MemoryPickup struct{ ch chan []byte }

// NewMemoryPickup returns a MemoryPickup with the given buffer.
func NewMemoryPickup(buf int) *MemoryPickup { return &MemoryPickup{ch: make(chan []byte, buf)} }

// Publish enqueues a payload.
func (m *MemoryPickup) Publish(ctx context.Context, p []byte) error {
	select {
	case m.ch <- p:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe pushes each payload to fn from a single goroutine.
func (m *MemoryPickup) Subscribe(ctx context.Context, fn func([]byte)) (func(), error) {
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case p := <-m.ch:
				fn(p)
			case <-cctx.Done():
				return
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

// RunQueuePickup measures publish -> consumer-handler latency. The publish timestamp (monotonic
// nanoseconds since a base instant) travels in the payload.
func RunQueuePickup(ctx context.Context, pb PickupBench, lc LoadConfig) (Result, error) {
	base := time.Now()
	h := NewHistogram()
	var got atomic.Int64
	done := make(chan struct{})
	stop, err := pb.Subscribe(ctx, func(p []byte) {
		sent := int64(binary.BigEndian.Uint64(p))
		h.Record(time.Since(base) - time.Duration(sent))
		if got.Add(1) == int64(lc.N) {
			close(done)
		}
	})
	if err != nil {
		return Result{}, err
	}
	defer stop()
	errs := drive(ctx, lc, NewHistogram(), func(ctx context.Context, _ int) error {
		p := make([]byte, 8)
		binary.BigEndian.PutUint64(p, uint64(time.Since(base)))
		return pb.Publish(ctx, p)
	})
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		return Result{}, fmt.Errorf("pickup: only %d/%d delivered", got.Load(), lc.N)
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return Result{Scenario: "queue_pickup", Load: loadOf(lc), Errors: errs, Latency: h.Summary()}, nil
}
