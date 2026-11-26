// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/memory"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/natsjs"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

func TestFromStartHTTP(t *testing.T) {
	l, err := FromStart(context.Background(), flow.StartConfig{Type: "listener", Connector: "http", Path: "/in", Method: "put"}, FactoryDeps{})
	if err != nil {
		t.Fatal(err)
	}
	h, ok := l.(*httpListener)
	if !ok {
		t.Fatalf("got %T, want *httpListener", l)
	}
	if h.cfg.Path != "/in" || h.cfg.Method != "PUT" {
		t.Fatalf("cfg = %+v", h.cfg)
	}
	if _, err := FromStart(context.Background(), flow.StartConfig{Type: "listener", Connector: "sftp", Path: "/x"}, FactoryDeps{}); err == nil {
		t.Fatal("non-http connector must be rejected")
	}
	if _, err := FromStart(context.Background(), flow.StartConfig{Type: "listener", Connector: "http", Path: "no-slash"}, FactoryDeps{}); err == nil {
		t.Fatal("bad path must be rejected")
	}
}

func TestFromStartQueueAckNack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mb := memory.New()
	defer func() { _ = mb.Close() }()
	if err := mb.Ensure(ctx, queue.QueueDef{Name: "orders", MaxDeliveries: 2}); err != nil {
		t.Fatal(err)
	}
	l, err := FromStart(context.Background(), flow.StartConfig{Type: "queue", Queue: "orders"}, FactoryDeps{Queues: mb})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	okSeen := make(chan string, 4)
	h := func(_ context.Context, in *docs.Set) (*engine.Result, error) {
		n := calls.Add(1)
		if n == 1 {
			return nil, errors.New("boom") // first delivery nacked, then redelivered
		}
		okSeen <- "ok"
		return nil, nil
	}
	if err := l.Start(ctx, h); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Stop(ctx) }()
	if err := mb.Publish(ctx, "orders", queue.Message{ID: "m1", Data: []byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-okSeen:
	case <-ctx.Done():
		t.Fatal("message not redelivered after nack")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := mb.Stats(ctx, "orders")
		if st.Depth == 0 && calls.Load() == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls = %d, want 2 (nack then ack)", calls.Load())
	}
	if s := l.Status(); s.Failed != 1 || s.Received != 2 {
		t.Fatalf("status = %+v, want 1 failed of 2 received", s)
	}
}

func TestPersistentQueueListenerPausePreservesDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const ackWait = 50 * time.Millisecond
	const queueName = "orders"
	b, err := natsjs.New(natsjs.Options{StoreDir: t.TempDir(), AckWait: ackWait})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := b.Ensure(ctx, queue.QueueDef{Name: queueName, MaxDeliveries: 2}); err != nil {
		t.Fatal(err)
	}
	l, err := FromStart(ctx, flow.StartConfig{Type: "queue", Queue: queueName}, FactoryDeps{Queues: b})
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 1)
	if err := l.Start(ctx, func(_ context.Context, in *docs.Set) (*engine.Result, error) {
		defer func() { _ = in.Close() }()
		select {
		case delivered <- in.Docs()[0].ID:
		default:
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Stop(ctx); err != nil {
			t.Error(err)
		}
	}()

	if err := l.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, queueName, queue.Message{ID: "held", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-delivered:
		t.Fatalf("message %q reached the handler while paused", id)
	case <-time.After(6 * ackWait):
	}
	st, err := b.Stats(ctx, queueName)
	if err != nil {
		t.Fatal(err)
	}
	if st.Depth != 1 || st.DeadLetterDepth != 0 {
		t.Fatalf("paused queue stats = %+v, want one queued message and no dead letters", st)
	}
	if err := l.Resume(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-delivered:
		if id != "held" {
			t.Fatalf("delivered %q, want held", id)
		}
	case <-ctx.Done():
		t.Fatal("resume did not deliver the queued message")
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		st, err = b.Stats(ctx, queueName)
		if err != nil {
			t.Fatal(err)
		}
		if st.Depth == 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("resumed delivery was not acknowledged")
		}
	}
	select {
	case <-time.After(6 * ackWait):
	case <-ctx.Done():
		t.Fatal("timed out waiting past the ACK retry window")
	}
	st, err = b.Stats(ctx, queueName)
	if err != nil {
		t.Fatal(err)
	}
	if st.Depth != 0 || st.Redeliveries != 0 || st.DeadLetterDepth != 0 {
		t.Fatalf("resumed queue stats = %+v, want no pending deliveries, redeliveries, or dead letters", st)
	}
	if got := l.Status().Received; got != 1 {
		t.Fatalf("received %d deliveries, want one", got)
	}
}

type pauseConfirmationBackend struct {
	queue.QueueBackend
	err error
}

func (b pauseConfirmationBackend) Subscribe(ctx context.Context, name string, opts queue.SubOpts, h queue.Handler) (queue.Subscription, error) {
	sub, err := b.QueueBackend.Subscribe(ctx, name, opts, h)
	if err != nil {
		return nil, err
	}
	return pauseConfirmationSubscription{sub, b.err}, nil
}

type pauseConfirmationSubscription struct {
	queue.Subscription
	err error
}

func (s pauseConfirmationSubscription) Pause() error {
	if err := s.Subscription.Pause(); err != nil {
		return err
	}
	// Simulate a broker confirmation error after the local detach.
	return s.err
}

func TestPersistentQueueListenerResumeAfterFailedPause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := natsjs.New(natsjs.Options{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	const queueName = "orders"
	if err := b.Ensure(ctx, queue.QueueDef{Name: queueName}); err != nil {
		t.Fatal(err)
	}
	confirmationErr := errors.New("pause confirmation failed")
	backend := pauseConfirmationBackend{QueueBackend: b, err: confirmationErr}
	l, err := FromStart(ctx, flow.StartConfig{Type: "queue", Queue: queueName}, FactoryDeps{Queues: backend})
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 1)
	if err := l.Start(ctx, func(_ context.Context, in *docs.Set) (*engine.Result, error) {
		defer func() { _ = in.Close() }()
		delivered <- in.Docs()[0].ID
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Stop(ctx) }()

	if err := l.Pause(); !errors.Is(err, confirmationErr) {
		t.Fatalf("Pause() = %v, want %v", err, confirmationErr)
	}
	if state := l.Status().State; state != StatePaused {
		t.Fatalf("state after failed pause = %s, want paused", state)
	}
	if err := b.Publish(ctx, queueName, queue.Message{ID: "held", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Resume(); err != nil {
		t.Fatal(err)
	}
	if state := l.Status().State; state != StateRunning {
		t.Fatalf("state after resume = %s, want running", state)
	}
	select {
	case id := <-delivered:
		if id != "held" {
			t.Fatalf("delivered %q, want held", id)
		}
	case <-ctx.Done():
		t.Fatal("resume did not reattach the subscription")
	}
}
