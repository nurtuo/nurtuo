// SPDX-License-Identifier: AGPL-3.0-only

// Package bench is the performance harness for the M1 acceptance targets:
//   - platform overhead p99 < 10 ms (internal queue -> Map -> mock HTTP), and
//   - internal queue pickup p99 < 5 ms (publish -> handler start).
//
// Results are reported as p50/p99 per release in CI.
package bench

import "testing"

func BenchmarkQueuePickup(b *testing.B) {
	b.Skip("TODO(M1): embedded JetStream publish -> push handler latency")
}

func BenchmarkFlowOverhead(b *testing.B) {
	b.Skip("TODO(M1): queue -> Map -> mock HTTP end to end")
}
