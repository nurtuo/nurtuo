// SPDX-License-Identifier: AGPL-3.0-only

package natsjs

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
)

func TestPublishSurvivesAbruptExit(t *testing.T) {
	const storeEnv = "NURTUO_TEST_ABRUPT_QUEUE_STORE"
	def := queue.QueueDef{Name: "jobs"}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if dir := os.Getenv(storeEnv); dir != "" {
		b, err := New(Options{StoreDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if !b.srv.JetStreamConfig().SyncAlways {
			t.Fatal("persistent store must sync writes before acknowledging publishes")
		}
		if err := b.Ensure(ctx, def); err != nil {
			t.Fatal(err)
		}
		if err := b.Publish(ctx, def.Name, queue.Message{ID: "published", Data: []byte("payload")}); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Exit immediately after the publish acknowledgment without closing the broker.
	}

	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPublishSurvivesAbruptExit$")
	cmd.Env = append(os.Environ(), storeEnv+"="+dir, "GORACE=atexit_sleep_ms=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("publisher process: %v\n%s", err, out)
	}
	b, err := New(Options{StoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if err := b.Ensure(ctx, def); err != nil {
		t.Fatal(err)
	}
	got := make(chan queue.Message, 1)
	if _, err := b.Subscribe(ctx, def.Name, queue.SubOpts{}, func(ctx context.Context, d queue.Delivery) {
		_ = d.Ack(ctx)
		got <- d.Message()
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if msg.ID != "published" || string(msg.Data) != "payload" {
			t.Fatalf("recovered message: %+v", msg)
		}
	case <-ctx.Done():
		t.Fatal("published message did not survive process exit")
	}
}

func TestShutdownCancelsAndRecoversInFlightDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	opts := Options{StoreDir: t.TempDir(), AckWait: 500 * time.Millisecond}
	def := queue.QueueDef{Name: "jobs", Ordering: queue.OrderingPerKey}
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if err := b.Ensure(ctx, def); err != nil {
		t.Fatal(err)
	}

	started, finished := make(chan struct{}), make(chan struct{})
	var first sync.Once
	if _, err := b.Subscribe(ctx, def.Name, queue.SubOpts{}, func(ctx context.Context, _ queue.Delivery) {
		first.Do(func() {
			close(started)
			<-ctx.Done() // As with a paused source gate, only cancellation releases this handler.
			close(finished)
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, def.Name, queue.Message{ID: "in-flight", Key: "job"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("delivery never started")
	}
	if err := b.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before the canceled handler exited")
	}

	reopened, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Ensure(ctx, def); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	if _, err := reopened.Subscribe(ctx, def.Name, queue.SubOpts{}, func(ctx context.Context, d queue.Delivery) {
		if err := d.Ack(ctx); err != nil {
			t.Error(err)
		}
		got <- d.Message().ID
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-got:
		if id != "in-flight" {
			t.Fatalf("recovered %q, want in-flight", id)
		}
	case <-ctx.Done():
		t.Fatal("unacknowledged delivery did not recover")
	}
}
