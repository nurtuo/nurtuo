// SPDX-License-Identifier: AGPL-3.0-only

package natsjs_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/natsjs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/queuetest"
)

func TestConformance(t *testing.T) {
	queuetest.Run(t, func(t *testing.T) queue.QueueBackend {
		b, err := natsjs.New(natsjs.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func TestRetentionModes(t *testing.T) {
	b, err := natsjs.New(natsjs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	for _, r := range []queue.Retention{queue.RetentionLimits, queue.RetentionWorkQueue, queue.RetentionInterest} {
		name := "ret." + string(r)
		if err := b.Ensure(ctx, queue.QueueDef{Name: name, Retention: r, MaxAge: time.Hour, MaxBytes: 1 << 20}); err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		got := make(chan struct{}, 1)
		s, err := b.Subscribe(ctx, name, queue.SubOpts{Durable: "d"}, func(ctx context.Context, d queue.Delivery) {
			_ = d.Ack(ctx)
			got <- struct{}{}
		})
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		if err := b.Publish(ctx, name, queue.Message{Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no delivery", r)
		}
		_ = s.Close()
	}
}

func TestMaxDeliveriesAdvisoryDeadLetters(t *testing.T) {
	b, err := natsjs.New(natsjs.Options{AckWait: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	if err := b.Ensure(ctx, queue.QueueDef{Name: "crashy", MaxDeliveries: 2}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	// The handler never settles the delivery (simulates a crash/hang): only the server's
	// ack-wait timeout and max-deliver exhaustion can end it.
	s, err := b.Subscribe(ctx, "crashy", queue.SubOpts{Durable: "d"}, func(context.Context, queue.Delivery) {
		calls.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := b.Publish(ctx, "crashy", queue.Message{ID: "m1", Key: "k", Data: []byte("payload")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	var dls []queue.DeadLetter
	for time.Now().Before(deadline) {
		dls, _ = b.ListDeadLetters(ctx, "crashy", 10)
		if len(dls) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(dls) != 1 {
		t.Fatalf("dead letters = %d, want 1 (handler calls %d)", len(dls), calls.Load())
	}
	dl := dls[0]
	if string(dl.Message.Data) != "payload" || dl.Message.ID != "m1" || dl.Message.Key != "k" {
		t.Fatalf("dead letter message = %+v", dl.Message)
	}
	if !strings.Contains(dl.Reason, "max deliveries") || dl.Attempts != 2 || calls.Load() != 2 {
		t.Fatalf("reason=%q attempts=%d calls=%d", dl.Reason, dl.Attempts, calls.Load())
	}
	st, _ := b.Stats(ctx, "crashy")
	if st.DeadLetterDepth != 1 {
		t.Fatalf("stats = %+v", st)
	}
}
