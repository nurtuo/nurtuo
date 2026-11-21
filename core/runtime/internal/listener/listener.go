// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"context"
	"errors"
	"sync"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// State is a listener lifecycle state.
type State string

// Listener states.
const (
	StateStopped State = "stopped"
	StateRunning State = "running"
	StatePaused  State = "paused"
)

// Status is a point-in-time view of a listener.
type Status struct {
	State    State
	Addr     string // bound address, when applicable
	Received int64  // deliveries accepted
	Failed   int64  // deliveries whose flow run failed
}

// Handler runs one delivery (a set of documents) through a flow. It owns in. The caller of a Handler
// owns and must Close the Result's Returned and Failed sets.
type Handler func(ctx context.Context, in *docs.Set) (*engine.Result, error)

// Listener is a push trigger that feeds a Handler.
type Listener interface {
	// Start begins listening. It returns once the listener is ready (bound/subscribed).
	Start(ctx context.Context, h Handler) error
	// Stop shuts the listener down gracefully, waiting for in-flight deliveries until ctx expires.
	Stop(ctx context.Context) error
	// Pause stops accepting new work without releasing resources. Resume undoes it.
	Pause() error
	Resume() error
	Status() Status
}

// ErrState is returned for lifecycle calls invalid in the current state.
var ErrState = errors.New("listener: invalid state")

// FlowHandler returns a Handler running f on eng with a fresh Process per delivery.
func FlowHandler(eng *engine.Engine, f *flow.Flow) Handler {
	return func(ctx context.Context, in *docs.Set) (*engine.Result, error) {
		return eng.Run(ctx, f, in, nil)
	}
}

// Source is the minimal abstraction a subscribe-based transport (queue, AMQP, Kafka...) implements
// to become a Listener via NewSourceListener. Consume blocks, calling deliver for each message batch,
// until ctx is cancelled. deliver returns nil when the flow succeeded (ack) and an error otherwise (nack).
type Source interface {
	Consume(ctx context.Context, deliver func(ctx context.Context, in *docs.Set) error) error
}

type sourceListener struct {
	src Source

	mu     sync.Mutex
	state  State
	cancel context.CancelFunc
	done   chan struct{}
	gate   chan struct{} // closed while deliveries may proceed
	recv   int64
	failed int64
}

// NewSourceListener adapts a Source to a Listener. Pause holds back further deliveries (messages
// already in flight finish); Resume releases them.
func NewSourceListener(src Source) Listener {
	return &sourceListener{src: src, state: StateStopped}
}

func (l *sourceListener) Start(ctx context.Context, h Handler) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != StateStopped {
		return ErrState
	}
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.cancel, l.done = cancel, make(chan struct{})
	l.gate = make(chan struct{})
	close(l.gate)
	l.state = StateRunning
	done := l.done
	deliver := func(dctx context.Context, in *docs.Set) error {
		l.mu.Lock()
		gate := l.gate
		l.mu.Unlock()
		select {
		case <-gate:
		case <-dctx.Done():
			_ = in.Close()
			return dctx.Err()
		}
		l.mu.Lock()
		l.recv++
		l.mu.Unlock()
		res, err := h(dctx, in)
		if res != nil {
			if res.Failed != nil && res.Failed.Len() > 0 && err == nil {
				err = errors.New("listener: flow produced failed documents")
			}
			closeResult(res)
		}
		if err != nil {
			l.mu.Lock()
			l.failed++
			l.mu.Unlock()
		}
		return err
	}
	go func() {
		defer close(done)
		_ = l.src.Consume(cctx, deliver)
	}()
	return nil
}

func (l *sourceListener) Stop(ctx context.Context) error {
	l.mu.Lock()
	if l.state == StateStopped {
		l.mu.Unlock()
		return nil
	}
	l.state = StateStopped
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *sourceListener) Pause() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case StatePaused:
		return nil
	case StateRunning:
		l.gate = make(chan struct{})
		l.state = StatePaused
		return nil
	}
	return ErrState
}

func (l *sourceListener) Resume() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case StateRunning:
		return nil
	case StatePaused:
		close(l.gate)
		l.state = StateRunning
		return nil
	}
	return ErrState
}

func (l *sourceListener) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Status{State: l.state, Received: l.recv, Failed: l.failed}
}

func closeResult(r *engine.Result) {
	if r.Returned != nil {
		_ = r.Returned.Close()
	}
	if r.Failed != nil {
		_ = r.Failed.Close()
	}
}
