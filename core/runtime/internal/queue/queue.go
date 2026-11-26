// SPDX-License-Identifier: AGPL-3.0-only

// Package queue defines the QueueBackend abstraction used by the engine for deployable
// queues and push listeners. Implementations: natsjs (default, embedded), kafka, postgres.
// Hot-path rule: no database access per message.
package queue

import (
	"context"
	"errors"
	"time"
)

// Ordering of deliveries within a queue.
type Ordering string

const (
	OrderingNone   Ordering = "none"
	OrderingPerKey Ordering = "per-key"
	OrderingStrict Ordering = "strict"
)

// Retention decides when the backend discards a stored message.
type Retention string

const (
	// RetentionWorkQueue removes a message once acked (default).
	RetentionWorkQueue Retention = "workqueue"
	// RetentionLimits keeps messages until MaxAge/MaxBytes limits are hit.
	RetentionLimits Retention = "limits"
	// RetentionInterest keeps messages only while a subscriber exists and has not acked.
	RetentionInterest Retention = "interest"
)

// DefaultMaxDeliveries is used when QueueDef.MaxDeliveries is zero.
const DefaultMaxDeliveries = 5

// QueueDef is the deployed queue component body.
type QueueDef struct {
	Name          string
	Backend       string    // internal | kafka | postgres
	Retention     Retention // default workqueue
	MaxAge        time.Duration
	MaxBytes      int64
	MaxDeliveries int
	Backoff       []time.Duration
	DeadLetter    string // informational name; the DLQ is always derived from Name
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
	// Pause suspends local delivery even if it returns an error confirming
	// the detach with the broker. Resume can reattach after either result.
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

// DeadLetter is a message parked after retries were exhausted or Term was called.
type DeadLetter struct {
	ID       string // opaque, stable identifier used for replay
	Message  Message
	Reason   string
	Attempts int
	At       time.Time
}

// ErrNotFound is returned for unknown queues or dead letters.
var ErrNotFound = errors.New("queue: not found")

// QueueBackend is the only way the engine talks to queues.
type QueueBackend interface {
	Ensure(ctx context.Context, def QueueDef) error // create/update on deploy (idempotent)
	Remove(ctx context.Context, name string) error  // drain and remove on undeploy
	Publish(ctx context.Context, name string, msg Message) error
	Subscribe(ctx context.Context, name string, opts SubOpts, h Handler) (Subscription, error) // push, never poll
	Stats(ctx context.Context, name string) (QueueStats, error)
	// ListDeadLetters returns up to limit parked messages, oldest first.
	ListDeadLetters(ctx context.Context, name string, limit int) ([]DeadLetter, error)
	// ReplayDeadLetter republishes the dead letter to the queue and removes it from the DLQ.
	ReplayDeadLetter(ctx context.Context, name, id string) error
	Close() error
}
