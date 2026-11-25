// SPDX-License-Identifier: AGPL-3.0-only

package natsjs

import (
	"context"
	"os"
	"os/exec"
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
	defer b.Close()
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
