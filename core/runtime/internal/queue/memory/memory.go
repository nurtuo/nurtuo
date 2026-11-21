// SPDX-License-Identifier: AGPL-3.0-only

// Package memory is an in-process queue.QueueBackend for tests. It implements work-queue
// semantics only (messages are removed once acked); retention limits are not enforced.
package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
)

type entry struct {
	msg      queue.Message
	attempt  int
	notAfter time.Time // not deliverable before this time
	inflight bool
	first    time.Time
}

type sub struct {
	h        queue.Handler
	conc     int
	inflight int
	paused   bool
	closed   bool
}

type q struct {
	def     queue.QueueDef
	pending []*entry
	subs    []*sub
	dlq     []queue.DeadLetter
	redeliv uint64
}

// Backend is the in-memory implementation.
type Backend struct {
	mu     sync.Mutex
	queues map[string]*q
	seq    int
	closed bool
}

// New returns an empty backend.
func New() *Backend { return &Backend{queues: map[string]*q{}} }

var _ queue.QueueBackend = (*Backend)(nil)

func (b *Backend) get(name string) (*q, error) {
	qq, ok := b.queues[name]
	if !ok {
		return nil, fmt.Errorf("%w: queue %q", queue.ErrNotFound, name)
	}
	return qq, nil
}

// Ensure creates or updates a queue.
func (b *Backend) Ensure(_ context.Context, def queue.QueueDef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if qq, ok := b.queues[def.Name]; ok {
		qq.def = def
		return nil
	}
	b.queues[def.Name] = &q{def: def}
	return nil
}

// Remove deletes a queue.
func (b *Backend) Remove(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if qq, ok := b.queues[name]; ok {
		for _, s := range qq.subs {
			s.closed = true
		}
		delete(b.queues, name)
	}
	return nil
}

// Publish appends a message.
func (b *Backend) Publish(_ context.Context, name string, msg queue.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	qq, err := b.get(name)
	if err != nil {
		return err
	}
	if msg.ID != "" {
		for _, e := range qq.pending {
			if e.msg.ID == msg.ID {
				return nil // dedupe, like JetStream Msg-Id
			}
		}
	}
	qq.pending = append(qq.pending, &entry{msg: msg, first: time.Now()})
	b.pump(qq)
	return nil
}

// Subscribe registers a push handler.
func (b *Backend) Subscribe(_ context.Context, name string, opts queue.SubOpts, h queue.Handler) (queue.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	qq, err := b.get(name)
	if err != nil {
		return nil, err
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = qq.def.Concurrency
	}
	if conc <= 0 {
		conc = 1
	}
	if qq.def.Ordering == queue.OrderingStrict {
		conc = 1
	}
	s := &sub{h: h, conc: conc}
	qq.subs = append(qq.subs, s)
	b.pump(qq)
	return &subscription{b: b, q: qq, s: s}, nil
}

// pump hands out deliverable messages. Caller holds b.mu.
func (b *Backend) pump(qq *q) {
	if b.closed {
		return
	}
	strict := qq.def.Ordering == queue.OrderingStrict
	perKey := qq.def.Ordering == queue.OrderingPerKey
	now := time.Now()
	busyKeys := map[string]bool{}
	total := 0
	for _, e := range qq.pending {
		if e.inflight {
			total++
			if e.msg.Key != "" {
				busyKeys[e.msg.Key] = true
			}
		}
	}
	var next time.Time
	for _, e := range qq.pending {
		if e.inflight {
			continue
		}
		if strict && total > 0 {
			return
		}
		blocked := false
		if perKey && e.msg.Key != "" && busyKeys[e.msg.Key] {
			blocked = true
		}
		if !blocked && e.notAfter.After(now) {
			blocked = true
			if next.IsZero() || e.notAfter.Before(next) {
				next = e.notAfter
			}
		}
		if blocked {
			if strict {
				break // keep order: do not overtake a delayed message
			}
			if perKey && e.msg.Key != "" {
				busyKeys[e.msg.Key] = true
			}
			continue
		}
		var target *sub
		for _, s := range qq.subs {
			if !s.closed && !s.paused && s.inflight < s.conc {
				target = s
				break
			}
		}
		if target == nil {
			return
		}
		e.inflight = true
		e.attempt++
		if e.attempt > 1 {
			qq.redeliv++
		}
		target.inflight++
		total++
		if e.msg.Key != "" {
			busyKeys[e.msg.Key] = true
		}
		d := &delivery{b: b, q: qq, e: e, s: target}
		go target.h(context.Background(), d)
	}
	if !next.IsZero() {
		time.AfterFunc(time.Until(next)+time.Millisecond, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.pump(qq)
		})
	}
}

type delivery struct {
	b       *Backend
	q       *q
	e       *entry
	s       *sub
	settled bool
}

func (d *delivery) Message() queue.Message { return d.e.msg }
func (d *delivery) Attempt() int           { return d.e.attempt }

func (d *delivery) settle(fn func()) error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	if d.settled {
		return nil
	}
	d.settled = true
	d.s.inflight--
	d.e.inflight = false
	fn()
	d.b.pump(d.q)
	return nil
}

func (d *delivery) remove() {
	for i, e := range d.q.pending {
		if e == d.e {
			d.q.pending = append(d.q.pending[:i], d.q.pending[i+1:]...)
			return
		}
	}
}

func (d *delivery) deadLetter(reason string) {
	d.remove()
	d.b.seq++
	d.q.dlq = append(d.q.dlq, queue.DeadLetter{
		ID: fmt.Sprint(d.b.seq), Message: d.e.msg, Reason: reason, Attempts: d.e.attempt, At: time.Now(),
	})
}

// Ack removes the message.
func (d *delivery) Ack(context.Context) error { return d.settle(d.remove) }

// Nak schedules redelivery or dead-letters after the last attempt.
func (d *delivery) Nak(_ context.Context, delay time.Duration) error {
	return d.settle(func() {
		max := d.q.def.MaxDeliveries
		if max <= 0 {
			max = queue.DefaultMaxDeliveries
		}
		if d.e.attempt >= max {
			d.deadLetter("max deliveries exceeded")
			return
		}
		if delay == 0 && len(d.q.def.Backoff) > 0 {
			i := d.e.attempt - 1
			if i >= len(d.q.def.Backoff) {
				i = len(d.q.def.Backoff) - 1
			}
			delay = d.q.def.Backoff[i]
		}
		d.e.notAfter = time.Now().Add(delay)
	})
}

// Term sends the message to the dead letter list.
func (d *delivery) Term(context.Context) error {
	return d.settle(func() { d.deadLetter("terminated") })
}

type subscription struct {
	b *Backend
	q *q
	s *sub
}

func (s *subscription) Pause() error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.s.paused = true
	return nil
}

func (s *subscription) Resume() error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.s.paused = false
	s.b.pump(s.q)
	return nil
}

func (s *subscription) Close() error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.s.closed = true
	return nil
}

// Stats reports depth and dead letter depth.
func (b *Backend) Stats(_ context.Context, name string) (queue.QueueStats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	qq, err := b.get(name)
	if err != nil {
		return queue.QueueStats{}, err
	}
	st := queue.QueueStats{Depth: uint64(len(qq.pending)), Redeliveries: qq.redeliv, DeadLetterDepth: uint64(len(qq.dlq))}
	if len(qq.pending) > 0 {
		st.OldestMessageAge = time.Since(qq.pending[0].first)
	}
	return st, nil
}

// ListDeadLetters lists parked messages.
func (b *Backend) ListDeadLetters(_ context.Context, name string, limit int) ([]queue.DeadLetter, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	qq, err := b.get(name)
	if err != nil {
		return nil, err
	}
	out := append([]queue.DeadLetter(nil), qq.dlq...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ReplayDeadLetter moves a dead letter back to the queue.
func (b *Backend) ReplayDeadLetter(_ context.Context, name, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	qq, err := b.get(name)
	if err != nil {
		return err
	}
	for i, dl := range qq.dlq {
		if dl.ID == id {
			qq.dlq = append(qq.dlq[:i], qq.dlq[i+1:]...)
			qq.pending = append(qq.pending, &entry{msg: dl.Message, first: time.Now()})
			b.pump(qq)
			return nil
		}
	}
	return fmt.Errorf("%w: dead letter %q", queue.ErrNotFound, id)
}

// Close stops delivery.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}
