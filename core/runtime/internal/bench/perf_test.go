// SPDX-License-Identifier: AGPL-3.0-only

package bench

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/natsjs"
)

// Budgets from the README (p99). Override with flags to tighten or relax for slow runners.
const (
	DefaultOverheadBudget = 10 * time.Millisecond
	DefaultPickupBudget   = 5 * time.Millisecond
)

var (
	fOut      = flag.String("perf.out", "", "write JSON results to this file")
	fN        = flag.Int("perf.n", 5000, "documents/messages per scenario")
	fConc     = flag.Int("perf.concurrency", 8, "concurrent workers")
	fRate     = flag.Int("perf.rate", 2000, "target ops/sec (0 = closed loop)")
	fOverhead = flag.Duration("perf.overhead-budget", DefaultOverheadBudget, "engine overhead p99 budget")
	fPickup   = flag.Duration("perf.pickup-budget", DefaultPickupBudget, "queue pickup p99 budget")
	fRepeats  = flag.Int("perf.repeats", 3, "measurements per scenario; the gate uses the median p99")
)

type report struct {
	GeneratedAt string   `json:"generated_at"`
	Budgets     budgets  `json:"budgets_ms"`
	Results     []Result `json:"results"`
	Pass        bool     `json:"pass"`
}

type budgets struct {
	Overhead float64 `json:"engine_overhead_p99"`
	Pickup   float64 `json:"queue_pickup_p99"`
}

// median returns the median of xs (the mean of the two middle values for an even count).
func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// perfRun is one measurement of every scenario.
type perfRun struct{ ov, floor, pk Result }

func measure(ctx context.Context, t *testing.T, lc LoadConfig) perfRun {
	t.Helper()
	ov, err := RunEngineOverhead(ctx, lc)
	if err != nil {
		t.Fatal(err)
	}
	floor, err := RunQueuePickup(ctx, NewMemoryPickup(1024), lc) // harness/scheduler floor, informational
	if err != nil {
		t.Fatal(err)
	}
	floor.Scenario = "queue_pickup_memory_floor"

	// Measure the existing pickup budget with JetStream's temporary store and
	// periodic disk sync. A configured state directory syncs each write.
	js, err := natsjs.New(natsjs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.Close() }()
	qp, err := NewQueuePickup(ctx, js, "bench.pickup", *fConc)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := RunQueuePickup(ctx, qp, lc)
	if err != nil {
		t.Fatal(err)
	}
	pk.Scenario = "queue_pickup_natsjs"
	return perfRun{ov, floor, pk}
}

// TestPerfBudget fails when the p99 of a gated scenario exceeds its budget in the MEDIAN of
// -perf.repeats (default 3) measurements, so one noisy run on a busy machine cannot fail it. Errors
// in any run still fail. It measures latency and must not share the machine with other packages'
// tests, so it only runs when NURTUO_PERF=1 (CI: its own step, `go test -p 1`, see docs/perf.md).
// Run: NURTUO_PERF=1 go test ./internal/bench -run TestPerfBudget -count=1 -perf.out=perf.json
func TestPerfBudget(t *testing.T) {
	if os.Getenv("NURTUO_PERF") != "1" {
		t.Skip("set NURTUO_PERF=1 to run the latency budget test (see docs/perf.md)")
	}
	ctx := context.Background()
	lc := LoadConfig{N: *fN, Concurrency: *fConc, Rate: *fRate}
	repeats := max(*fRepeats, 1)
	rep := report{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Budgets: budgets{ms(*fOverhead), ms(*fPickup)}, Pass: true}

	var ovP99, pkP99 []float64
	var ovLast, floorLast, pkLast Result
	for i := 1; i <= repeats; i++ {
		r := measure(ctx, t, lc)
		t.Logf("run %d/%d: engine_overhead p99=%.3fms, queue_pickup_natsjs p99=%.3fms, memory_floor p99=%.3fms (informational)",
			i, repeats, r.ov.Latency.P99Ms, r.pk.Latency.P99Ms, r.floor.Latency.P99Ms)
		for _, x := range []Result{r.ov, r.pk} {
			if x.Errors > 0 {
				rep.Pass = false
				t.Errorf("run %d: %s: %d errors", i, x.Scenario, x.Errors)
			}
		}
		ovP99 = append(ovP99, r.ov.Latency.P99Ms)
		pkP99 = append(pkP99, r.pk.Latency.P99Ms)
		ovLast, floorLast, pkLast = r.ov, r.floor, r.pk
	}
	rep.Results = []Result{ovLast, floorLast, pkLast}
	check := func(name string, p99s []float64, budget time.Duration) {
		m := median(p99s)
		t.Logf("%s: p99 per run %v ms, median %.3fms (budget %.1fms)", name, p99s, m, ms(budget))
		if m > ms(budget) {
			rep.Pass = false
			t.Errorf("%s: median p99 %.3fms of %d runs exceeds budget %.1fms", name, m, len(p99s), ms(budget))
		}
	}
	check(ovLast.Scenario, ovP99, *fOverhead)
	check(pkLast.Scenario, pkP99, *fPickup)

	if *fOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.MkdirAll(filepath.Dir(*fOut), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(*fOut, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{7}, 7}, {[]float64{9, 1, 2}, 2}, {[]float64{4, 1, 2, 3}, 2.5}, {[]float64{1, 1, 50}, 1}} {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestHistogramQuantiles(t *testing.T) {
	h := NewHistogram()
	for i := 1; i <= 1000; i++ {
		h.Record(time.Duration(i) * time.Microsecond)
	}
	s := h.Summary()
	near := func(got, want float64) bool { return got > want*0.98 && got < want*1.02 }
	if !near(s.P50Ms, 0.5) || !near(s.P95Ms, 0.95) || !near(s.P99Ms, 0.99) || !near(s.MaxMs, 1.0) || s.Count != 1000 {
		t.Fatalf("bad summary: %+v", s)
	}
}
