// SPDX-License-Identifier: AGPL-3.0-only

package docs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
)

func BenchmarkSpoolSmallInMemory(b *testing.B) {
	sp := NewSpool(SpoolConfig{Dir: b.TempDir()})
	data := bytes.Repeat([]byte("x"), 1024)
	b.ReportAllocs()
	b.SetBytes(1024)
	for i := 0; i < b.N; i++ {
		p, _ := sp.NewPayload(bytes.NewReader(data))
		_ = p.Release()
	}
}

func BenchmarkSpoolLargeSpill(b *testing.B) {
	sp := NewSpool(SpoolConfig{Dir: b.TempDir(), DocThreshold: 64 << 10})
	const size = 8 << 20
	b.ReportAllocs()
	b.SetBytes(size)
	for i := 0; i < b.N; i++ {
		p, err := sp.NewPayload(io.LimitReader(zeroReader{}, size))
		if err != nil {
			b.Fatal(err)
		}
		_ = p.Release()
	}
}

// BenchmarkApplyOverhead measures per-document pipeline overhead with a no-op function.
func BenchmarkApplyOverhead(b *testing.B) {
	for _, n := range []int{1, 100, 10000} {
		for _, conc := range []int{1, 8} {
			b.Run(fmt.Sprintf("docs=%d/conc=%d", n, conc), func(b *testing.B) {
				in := NewSet()
				for i := 0; i < n; i++ {
					in.Add(New("d", nil, nil))
				}
				fn := func(_ context.Context, d *Document) (*Document, error) { return d, nil }
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					out, _ := Apply(ctx, in, Options{Concurrency: conc}, fn)
					in = out
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/doc")
			})
		}
	}
}
