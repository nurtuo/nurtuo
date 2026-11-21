// SPDX-License-Identifier: AGPL-3.0-only

package docs

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
)

// Func processes one document. Return values:
//   - (doc, nil): doc is the result (may be the input, mutated, or a new document).
//   - (nil, nil): the document is dropped.
//   - (_, err):   the input document is marked errored; other documents are unaffected.
//
// A panic is recovered and treated as an error.
type Func func(ctx context.Context, d *Document) (*Document, error)

// Options tunes Apply.
type Options struct {
	Concurrency int    // <=0 means GOMAXPROCS
	Step        string // recorded in error detail
}

// Apply runs fn over every OK document of in with bounded concurrency and returns
// a new set in the same order. Non-OK documents pass through untouched. Per-document
// failures never abort the set. If ctx is cancelled, unstarted documents are marked
// errored with the context error and ctx.Err() is returned alongside the set.
//
// Ownership: input documents replaced by a different output are released (their
// payload reference is dropped). Therefore, if fn returns a new document that
// shares the input's payload, it must Retain the payload (or use Document.Clone)
// before returning; otherwise the payload is freed under the returned document.
// Errors returned by fn are never mutated, so they may be shared across calls.
func Apply(ctx context.Context, in *Set, opt Options, fn Func) (*Set, error) {
	n := in.Len()
	out := make([]*Document, n)
	conc := opt.Concurrency
	if conc <= 0 {
		conc = runtime.GOMAXPROCS(0)
	}
	if conc > n {
		conc = n
	}
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				out[i] = runOne(ctx, in.docs[i], opt.Step, fn)
			}
		}()
	}
	cancelled := false
feed:
	for i := 0; i < n; i++ {
		d := in.docs[i]
		if d.status != StatusOK {
			out[i] = d
			continue
		}
		select {
		case idx <- i:
		case <-ctx.Done():
			cancelled = true
			for j := i; j < n; j++ {
				out[j] = in.docs[j]
				if out[j].status == StatusOK {
					out[j].Fail(&Error{Step: opt.Step, Message: "not processed", Cause: ctx.Err()})
				}
			}
			break feed
		}
	}
	close(idx)
	wg.Wait()
	res := &Set{docs: out}
	if cancelled {
		return res, ctx.Err()
	}
	return res, nil
}

func runOne(ctx context.Context, d *Document, step string, fn Func) (res *Document) {
	defer func() {
		if r := recover(); r != nil {
			d.Fail(&Error{Step: step, Code: "panic", Message: fmt.Sprint(r)})
			res = d
		}
	}()
	o, err := fn(ctx, d)
	switch {
	case err != nil:
		if o != nil && o != d {
			_ = o.Release()
		}
		var e *Error
		if errors.As(err, &e) {
			if e.Step == "" {
				// fn may return an *Error shared across documents; never mutate it.
				c := *e
				c.Step = step
				e = &c
			}
			d.Fail(e)
		} else {
			d.Fail(&Error{Step: step, Message: err.Error(), Cause: err})
		}
		return d
	case o == nil:
		d.Drop()
		return d
	case o != d:
		_ = d.Release()
	}
	return o
}
