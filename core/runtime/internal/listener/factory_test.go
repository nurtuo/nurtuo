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
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

func TestFromStartHTTP(t *testing.T) {
	l, err := FromStart(flow.StartConfig{Type: "listener", Connector: "http", Path: "/in", Method: "put"}, FactoryDeps{})
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
	if _, err := FromStart(flow.StartConfig{Type: "listener", Connector: "sftp", Path: "/x"}, FactoryDeps{}); err == nil {
		t.Fatal("non-http connector must be rejected")
	}
	if _, err := FromStart(flow.StartConfig{Type: "listener", Connector: "http", Path: "no-slash"}, FactoryDeps{}); err == nil {
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
	l, err := FromStart(flow.StartConfig{Type: "queue", Queue: "orders"}, FactoryDeps{Queues: mb})
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
