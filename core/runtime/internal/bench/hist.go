// SPDX-License-Identifier: AGPL-3.0-only

// Package bench is the performance harness for the p99 targets (platform overhead < 10 ms,
// queue pickup < 5 ms). See docs/perf.md.
package bench

import (
	"math/bits"
	"sort"
	"sync"
	"time"
)

// subBits gives 2^subBits sub-buckets per power of two (<1% relative error).
const subBits = 7

// Histogram is a small HDR-style latency histogram: values in nanoseconds, log-linear buckets,
// bounded memory, safe for concurrent Record.
type Histogram struct {
	mu     sync.Mutex
	counts map[int]int64
	n      int64
	max    int64
}

// NewHistogram returns an empty histogram.
func NewHistogram() *Histogram { return &Histogram{counts: map[int]int64{}} }

func bucketOf(v int64) int {
	if v < 1<<subBits {
		return int(v)
	}
	shift := bits.Len64(uint64(v)) - 1 - subBits
	return (shift+1)<<subBits + int(v>>uint(shift)) - 1<<subBits
}

// bucketHigh returns the highest value that maps to bucket b.
func bucketHigh(b int) int64 {
	if b < 1<<subBits {
		return int64(b)
	}
	shift := b>>subBits - 1
	sub := int64(b&(1<<subBits-1)) + 1<<subBits
	return (sub+1)<<uint(shift) - 1
}

// Record adds one observation.
func (h *Histogram) Record(d time.Duration) {
	v := int64(d)
	if v < 0 {
		v = 0
	}
	h.mu.Lock()
	h.counts[bucketOf(v)]++
	h.n++
	if v > h.max {
		h.max = v
	}
	h.mu.Unlock()
}

// Count returns the number of observations.
func (h *Histogram) Count() int64 { h.mu.Lock(); defer h.mu.Unlock(); return h.n }

// Quantile returns the value at quantile q in [0,1] (upper bound of its bucket, capped at max).
func (h *Histogram) Quantile(q float64) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n == 0 {
		return 0
	}
	target := int64(q*float64(h.n) + 0.999999)
	if target < 1 {
		target = 1
	}
	keys := make([]int, 0, len(h.counts))
	for k := range h.counts {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var cum int64
	for _, k := range keys {
		cum += h.counts[k]
		if cum >= target {
			v := bucketHigh(k)
			if v > h.max {
				v = h.max
			}
			return time.Duration(v)
		}
	}
	return time.Duration(h.max)
}

// Summary is the JSON-serialisable view of a histogram (milliseconds).
type Summary struct {
	Count int64   `json:"count"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Summary reports p50/p95/p99/max.
func (h *Histogram) Summary() Summary {
	h.mu.Lock()
	mx := h.max
	h.mu.Unlock()
	return Summary{Count: h.Count(), P50Ms: ms(h.Quantile(.50)), P95Ms: ms(h.Quantile(.95)), P99Ms: ms(h.Quantile(.99)), MaxMs: ms(time.Duration(mx))}
}
