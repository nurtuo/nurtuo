// SPDX-License-Identifier: AGPL-3.0-only

package bench

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// LoadConfig controls how work is offered.
type LoadConfig struct {
	N           int // total operations
	Concurrency int // worker goroutines (>=1)
	Rate        int // target ops/sec across all workers; 0 = closed loop (as fast as possible)
}

type job struct {
	i  int
	at time.Time
}

// drive runs op N times. With Rate>0 operations have an intended start time (open loop) and the
// recorded latency is measured from that intended time, so queueing delay is not hidden
// (coordinated omission). With Rate==0 latency is the op duration alone.
func drive(ctx context.Context, lc LoadConfig, h *Histogram, op func(ctx context.Context, i int) error) int64 {
	if lc.Concurrency < 1 {
		lc.Concurrency = 1
	}
	jobs := make(chan job, lc.Concurrency)
	var wg sync.WaitGroup
	var errs atomic.Int64
	for w := 0; w < lc.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				start := j.at
				if lc.Rate == 0 {
					start = time.Now()
				}
				err := op(ctx, j.i)
				h.Record(time.Since(start))
				if err != nil {
					errs.Add(1)
				}
			}
		}()
	}
	t0 := time.Now()
	for i := 0; i < lc.N && ctx.Err() == nil; i++ {
		at := time.Now()
		if lc.Rate > 0 {
			at = t0.Add(time.Duration(float64(i) / float64(lc.Rate) * float64(time.Second)))
			if d := time.Until(at); d > 0 {
				time.Sleep(d)
			}
		}
		jobs <- job{i, at}
	}
	close(jobs)
	wg.Wait()
	return errs.Load()
}
