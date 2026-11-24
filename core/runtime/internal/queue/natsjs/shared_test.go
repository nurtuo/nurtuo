// SPDX-License-Identifier: AGPL-3.0-only

package natsjs

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
)

func TestSharedPersistentConsumer(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "new durable"
		if legacy {
			name = "existing ungrouped durable"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			opts := Options{StoreDir: t.TempDir()}
			const name, durable = "jobs", "nurtuo-jobs"
			def := queue.QueueDef{Name: name}
			b, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = b.Close() }()
			if err := b.Ensure(ctx, def); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if _, err := b.js.AddConsumer(streamName(name), &nats.ConsumerConfig{
					Durable: durable, DeliverSubject: nats.NewInbox(), FilterSubject: subject(name),
					AckPolicy: nats.AckExplicitPolicy, AckWait: b.ackWait, MaxAckPending: 1,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Publish(ctx, name, queue.Message{ID: "pending"}); err != nil {
				t.Fatal(err)
			}
			first, second := make(chan string, 2), make(chan string, 2)
			subscribe := func(got chan<- string) queue.Subscription {
				t.Helper()
				s, err := b.Subscribe(ctx, name, queue.SubOpts{Durable: durable}, func(ctx context.Context, d queue.Delivery) {
					if err := d.Ack(ctx); err != nil {
						t.Errorf("ack: %v", err)
					}
					got <- d.Message().ID
				})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			one := subscribe(first)
			two := subscribe(second)
			check := func(got, want string) {
				t.Helper()
				if got != want {
					t.Fatalf("delivery = %q, want %q", got, want)
				}
			}
			select {
			case id := <-first:
				check(id, "pending")
			case id := <-second:
				check(id, "pending")
			case <-ctx.Done():
				t.Fatal("shared consumer did not receive the queued message")
			}
			if err := one.Close(); err != nil {
				t.Fatal(err)
			}
			if err := b.Publish(ctx, name, queue.Message{ID: "remaining"}); err != nil {
				t.Fatal(err)
			}
			select {
			case id := <-second:
				check(id, "remaining")
			case <-ctx.Done():
				t.Fatal("remaining subscriber stopped receiving")
			}
			if err := two.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := b.js.ConsumerInfo(streamName(name), durable); err != nil {
				t.Fatalf("closing subscribers removed the durable: %v", err)
			}
		})
	}
}

func TestPerKeyOrderingAcrossSharedListeners(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const name = "jobs"
	b, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if err := b.Ensure(ctx, queue.QueueDef{Name: name, Ordering: queue.OrderingPerKey, Concurrency: 4}); err != nil {
		t.Fatal(err)
	}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstAck := make(chan error, 1)
	type result struct {
		id       string
		listener int
		err      error
	}
	got := make(chan result, 2)
	opts := queue.SubOpts{Durable: "shared"}
	first, err := b.Subscribe(ctx, name, opts, func(hctx context.Context, d queue.Delivery) {
		if d.Message().ID != "first" {
			got <- result{d.Message().ID, 1, d.Ack(hctx)}
			return
		}
		close(firstStarted)
		select {
		case <-releaseFirst:
			firstAck <- d.Ack(hctx)
		case <-ctx.Done():
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(id, key string) {
		t.Helper()
		if err := b.Publish(ctx, name, queue.Message{ID: id, Key: key}); err != nil {
			t.Fatal(err)
		}
	}
	publish("first", "a")
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("first listener did not start")
	}

	if _, err := b.Subscribe(ctx, name, opts, func(hctx context.Context, d queue.Delivery) {
		got <- result{d.Message().ID, 2, d.Ack(hctx)}
	}); err != nil {
		t.Fatal(err)
	}
	publish("same-key", "a")
	publish("other-key", "b")
	expect := func(want string) result {
		t.Helper()
		select {
		case r := <-got:
			if r.id != want || r.err != nil {
				t.Fatalf("delivery = %q, ack = %v, want %q", r.id, r.err, want)
			}
			return r
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %q", want)
			return result{}
		}
	}
	expect("other-key")
	if err := first.Pause(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		t.Fatalf("delivery %q ran while its key was busy", r.id)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case err := <-firstAck:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("first listener did not ack")
	}
	if r := expect("same-key"); r.listener != 2 {
		t.Fatalf("same-key delivery ran on listener %d, want 2", r.listener)
	}
}
