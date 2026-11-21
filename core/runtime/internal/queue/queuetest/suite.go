// SPDX-License-Identifier: AGPL-3.0-only

// Package queuetest is the shared conformance suite for queue.QueueBackend implementations.
package queuetest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
)

// Factory returns a fresh backend; the suite closes it.
type Factory func(t *testing.T) queue.QueueBackend

const wait = 10 * time.Second

// Run executes every conformance test against backends built by f.
func Run(t *testing.T, f Factory) {
	tests := map[string]func(*testing.T, queue.QueueBackend){
		"PublishSubscribeAck":      publishSubscribeAck,
		"UnknownQueue":             unknownQueue,
		"NakRedeliversWithAttempt": nakRedelivers,
		"MaxDeliveriesToDLQ":       maxDeliveriesDLQ,
		"TermToDLQAndReplay":       termAndReplay,
		"StrictOrdering":           strictOrdering,
		"PerKeyOrdering":           perKeyOrdering,
		"ConcurrencyLimit":         concurrencyLimit,
		"PauseResume":              pauseResume,
		"StatsDepth":               statsDepth,
		"RemoveQueue":              removeQueue,
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			b := f(t)
			t.Cleanup(func() { _ = b.Close() })
			fn(t, b)
		})
	}
}

func ensure(t *testing.T, b queue.QueueBackend, def queue.QueueDef) {
	t.Helper()
	if err := b.Ensure(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	if err := b.Ensure(context.Background(), def); err != nil {
		t.Fatalf("Ensure not idempotent: %v", err)
	}
}

func publish(t *testing.T, b queue.QueueBackend, name string, msgs ...queue.Message) {
	t.Helper()
	for _, m := range msgs {
		if err := b.Publish(context.Background(), name, m); err != nil {
			t.Fatal(err)
		}
	}
}

func sub(t *testing.T, b queue.QueueBackend, name string, opts queue.SubOpts, h queue.Handler) queue.Subscription {
	t.Helper()
	s, err := b.Subscribe(context.Background(), name, opts, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatalf("timeout waiting for %s", what)
		panic("unreachable")
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func publishSubscribeAck(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "orders.inbound"})
	got := make(chan queue.Message, 1)
	sub(t, b, "orders.inbound", queue.SubOpts{Durable: "l1"}, func(ctx context.Context, d queue.Delivery) {
		got <- d.Message()
		_ = d.Ack(ctx)
	})
	publish(t, b, "orders.inbound", queue.Message{ID: "m1", Key: "k", Properties: map[string]string{"a": "b"}, Data: []byte("hello")})
	m := recv(t, got, "message")
	if m.ID != "m1" || m.Key != "k" || string(m.Data) != "hello" || m.Properties["a"] != "b" {
		t.Fatalf("message mismatch: %+v", m)
	}
	eventually(t, "depth 0", func() bool {
		s, err := b.Stats(context.Background(), "orders.inbound")
		return err == nil && s.Depth == 0
	})
}

func unknownQueue(t *testing.T, b queue.QueueBackend) {
	if err := b.Publish(context.Background(), "nope", queue.Message{Data: []byte("x")}); err == nil {
		t.Fatal("expected error publishing to unknown queue")
	}
}

func nakRedelivers(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "retry", MaxDeliveries: 5, Backoff: []time.Duration{10 * time.Millisecond}})
	attempts := make(chan int, 10)
	sub(t, b, "retry", queue.SubOpts{Durable: "l1"}, func(ctx context.Context, d queue.Delivery) {
		attempts <- d.Attempt()
		if d.Attempt() < 3 {
			_ = d.Nak(ctx, 0)
			return
		}
		_ = d.Ack(ctx)
	})
	publish(t, b, "retry", queue.Message{ID: "r1", Data: []byte("x")})
	for want := 1; want <= 3; want++ {
		if got := recv(t, attempts, fmt.Sprintf("attempt %d", want)); got != want {
			t.Fatalf("attempt = %d, want %d", got, want)
		}
	}
}

func maxDeliveriesDLQ(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "dlqtest", MaxDeliveries: 3, Backoff: []time.Duration{5 * time.Millisecond}})
	var calls atomic.Int32
	sub(t, b, "dlqtest", queue.SubOpts{Durable: "l1"}, func(ctx context.Context, d queue.Delivery) {
		calls.Add(1)
		_ = d.Nak(ctx, 0)
	})
	publish(t, b, "dlqtest", queue.Message{ID: "d1", Key: "kk", Data: []byte("boom")})
	var dls []queue.DeadLetter
	eventually(t, "dead letter", func() bool {
		var err error
		dls, err = b.ListDeadLetters(context.Background(), "dlqtest", 10)
		return err == nil && len(dls) == 1
	})
	if n := calls.Load(); n != 3 {
		t.Fatalf("handler calls = %d, want 3", n)
	}
	if dls[0].Message.ID != "d1" || string(dls[0].Message.Data) != "boom" || dls[0].Message.Key != "kk" {
		t.Fatalf("dead letter mismatch: %+v", dls[0])
	}
	s, _ := b.Stats(context.Background(), "dlqtest")
	if s.DeadLetterDepth != 1 {
		t.Fatalf("DeadLetterDepth = %d", s.DeadLetterDepth)
	}
}

func termAndReplay(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "term", MaxDeliveries: 3})
	var fail atomic.Bool
	fail.Store(true)
	done := make(chan string, 1)
	sub(t, b, "term", queue.SubOpts{Durable: "l1"}, func(ctx context.Context, d queue.Delivery) {
		if fail.Load() {
			_ = d.Term(ctx)
			return
		}
		_ = d.Ack(ctx)
		done <- string(d.Message().Data)
	})
	publish(t, b, "term", queue.Message{ID: "t1", Data: []byte("poison")})
	var dls []queue.DeadLetter
	eventually(t, "dead letter", func() bool {
		dls, _ = b.ListDeadLetters(context.Background(), "term", 10)
		return len(dls) == 1
	})
	fail.Store(false)
	if err := b.ReplayDeadLetter(context.Background(), "term", dls[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, done, "replayed message"); got != "poison" {
		t.Fatalf("replayed %q", got)
	}
	eventually(t, "dlq empty", func() bool {
		dls, _ = b.ListDeadLetters(context.Background(), "term", 10)
		return len(dls) == 0
	})
	if err := b.ReplayDeadLetter(context.Background(), "term", "999999"); err == nil {
		t.Fatal("expected error replaying unknown dead letter")
	}
}

func strictOrdering(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "strict", Ordering: queue.OrderingStrict, Concurrency: 8})
	const n = 20
	var cur, maxCur atomic.Int32
	var mu sync.Mutex
	var order []string
	done := make(chan struct{})
	sub(t, b, "strict", queue.SubOpts{Durable: "l1", Concurrency: 8}, func(ctx context.Context, d queue.Delivery) {
		c := cur.Add(1)
		for {
			m := maxCur.Load()
			if c <= m || maxCur.CompareAndSwap(m, c) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		order = append(order, string(d.Message().Data))
		last := len(order) == n
		mu.Unlock()
		cur.Add(-1)
		_ = d.Ack(ctx)
		if last {
			close(done)
		}
	})
	for i := 0; i < n; i++ {
		publish(t, b, "strict", queue.Message{Data: []byte(fmt.Sprint(i))})
	}
	recv(t, done, "all messages")
	if maxCur.Load() != 1 {
		t.Fatalf("max concurrency = %d, want 1", maxCur.Load())
	}
	for i, v := range order {
		if v != fmt.Sprint(i) {
			t.Fatalf("order[%d] = %s", i, v)
		}
	}
}

func perKeyOrdering(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "perkey", Ordering: queue.OrderingPerKey, Concurrency: 4})
	const perK = 10
	keys := []string{"a", "b", "c"}
	var mu sync.Mutex
	seq := map[string][]int{}
	active := map[string]int{}
	var violated atomic.Bool
	var count atomic.Int32
	done := make(chan struct{})
	sub(t, b, "perkey", queue.SubOpts{Durable: "l1", Concurrency: 4}, func(ctx context.Context, d queue.Delivery) {
		k := d.Message().Key
		var i int
		_, _ = fmt.Sscan(string(d.Message().Data), &i)
		mu.Lock()
		active[k]++
		if active[k] > 1 {
			violated.Store(true)
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		seq[k] = append(seq[k], i)
		active[k]--
		mu.Unlock()
		_ = d.Ack(ctx)
		if count.Add(1) == perK*int32(len(keys)) {
			close(done)
		}
	})
	for i := 0; i < perK; i++ {
		for _, k := range keys {
			publish(t, b, "perkey", queue.Message{Key: k, Data: []byte(fmt.Sprint(i))})
		}
	}
	recv(t, done, "all messages")
	if violated.Load() {
		t.Fatal("two messages of one key ran concurrently")
	}
	for _, k := range keys {
		for i, v := range seq[k] {
			if v != i {
				t.Fatalf("key %s out of order: %v", k, seq[k])
			}
		}
	}
}

func concurrencyLimit(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "conc", Concurrency: 3})
	const n = 12
	var cur, maxCur, count atomic.Int32
	done := make(chan struct{})
	sub(t, b, "conc", queue.SubOpts{Durable: "l1", Concurrency: 3}, func(ctx context.Context, d queue.Delivery) {
		c := cur.Add(1)
		for {
			m := maxCur.Load()
			if c <= m || maxCur.CompareAndSwap(m, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		_ = d.Ack(ctx)
		if count.Add(1) == n {
			close(done)
		}
	})
	for i := 0; i < n; i++ {
		publish(t, b, "conc", queue.Message{Data: []byte("x")})
	}
	recv(t, done, "all messages")
	if m := maxCur.Load(); m > 3 || m < 2 {
		t.Fatalf("max concurrency = %d, want 2..3", m)
	}
}

func pauseResume(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "pr"})
	got := make(chan string, 10)
	s := sub(t, b, "pr", queue.SubOpts{Durable: "l1"}, func(ctx context.Context, d queue.Delivery) {
		got <- string(d.Message().Data)
		_ = d.Ack(ctx)
	})
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	publish(t, b, "pr", queue.Message{Data: []byte("while-paused")})
	select {
	case v := <-got:
		t.Fatalf("delivered %q while paused", v)
	case <-time.After(300 * time.Millisecond):
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if v := recv(t, got, "message after resume"); v != "while-paused" {
		t.Fatalf("got %q", v)
	}
}

func statsDepth(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "stats"})
	publish(t, b, "stats", queue.Message{Data: []byte("1")}, queue.Message{Data: []byte("2")}, queue.Message{Data: []byte("3")})
	eventually(t, "depth 3", func() bool {
		s, err := b.Stats(context.Background(), "stats")
		return err == nil && s.Depth == 3
	})
}

func removeQueue(t *testing.T, b queue.QueueBackend) {
	ensure(t, b, queue.QueueDef{Name: "gone"})
	if err := b.Remove(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(context.Background(), "gone", queue.Message{Data: []byte("x")}); err == nil {
		t.Fatal("publish to removed queue should fail")
	}
}
