// SPDX-License-Identifier: AGPL-3.0-only

// Package queue defines the QueueBackend abstraction used by the engine for deployable
// queues and push listeners. Implementations: natsjs (default, embedded), kafka, postgres.
// Hot-path rule: no database access per message.
package queue

import (
	"context"
	"time"
)

// Ordering of deliveries within a queue.
type Ordering string

const (
	OrderingNone   Ordering = "none"
	OrderingPerKey Ordering = "per-key"
	OrderingStrict Ordering = "strict"
)

// QueueDef is the deployed queue component body.
type QueueDef struct {
	Name          string
	Backend       string // internal | kafka | postgres
	MaxAge        time.Duration
	MaxBytes      int64
	MaxDeliveries int
	Backoff       []time.Duration
	DeadLetter    string
	Ordering      Ordering
	Concurrency   int
}

// Message is one queued document.
type Message struct {
	ID         string
	Key        string
	Properties map[string]string
	Data       []byte
}

// Delivery is a pushed message with explicit settlement.
type Delivery interface {
	Message() Message
	Attempt() int
	Ack(ctx context.Context) error
	Nak(ctx context.Context, delay time.Duration) error
	Term(ctx context.Context) error // send to dead letter
}

// Handler processes one delivery; it is called as soon as the backend pushes a message.
type Handler func(ctx context.Context, d Delivery)

// SubOpts configures a push subscription.
type SubOpts struct {
	Durable     string
	Concurrency int
}

// Subscription is an open push subscription.
type Subscription interface {
	Pause() error
	Resume() error
	Close() error
}

// QueueStats feeds listener status in the console.
type QueueStats struct {
	Depth            uint64
	OldestMessageAge time.Duration
	Redeliveries     uint64
	DeadLetterDepth  uint64
}

// QueueBackend is the only way the engine talks to queues.
type QueueBackend interface {
	Ensure(ctx context.Context, def QueueDef) error // create/update on deploy (idempotent)
	Remove(ctx context.Context, name string) error  // drain and remove on undeploy
	Publish(ctx context.Context, name string, msg Message) error
	Subscribe(ctx context.Context, name string, opts SubOpts, h Handler) (Subscription, error) // push, never poll
	Stats(ctx context.Context, name string) (QueueStats, error)
}
