// SPDX-License-Identifier: AGPL-3.0-only

package wasm_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm/wasmtest"
)

// BenchmarkCompile measures a cold compile (distinct module bytes each time are not needed: a
// fresh Engine has an empty cache) against the cached path.
func BenchmarkCompile(b *testing.B) {
	mod := wasmtest.Upper()
	b.Run("cold", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e := wasm.NewEngine(wasm.EngineOptions{})
			if _, err := e.Compile(context.Background(), mod, wasm.CompileOptions{}); err != nil {
				b.Fatal(err)
			}
			_ = e.Close(context.Background())
		}
	})
	b.Run("cached", func(b *testing.B) {
		e := wasm.NewEngine(wasm.EngineOptions{})
		defer func() { _ = e.Close(context.Background()) }()
		for i := 0; i < b.N; i++ {
			if _, err := e.Compile(context.Background(), mod, wasm.CompileOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkRun measures one per-invocation instantiate + alloc + process + decode.
func BenchmarkRun(b *testing.B) {
	for _, size := range []int{16, 4 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("payload=%dB", size), func(b *testing.B) {
			e := wasm.NewEngine(wasm.EngineOptions{})
			defer func() { _ = e.Close(context.Background()) }()
			m, err := e.Compile(context.Background(), wasmtest.Upper(), wasm.CompileOptions{})
			if err != nil {
				b.Fatal(err)
			}
			doc := wasm.Document{Props: map[string]string{"k": "v"}, Payload: bytes.Repeat([]byte("a"), size)}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := m.Run(context.Background(), doc, wasm.RunOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRunParallel shows scaling of independent per-call instances across cores.
func BenchmarkRunParallel(b *testing.B) {
	e := wasm.NewEngine(wasm.EngineOptions{})
	defer func() { _ = e.Close(context.Background()) }()
	m, err := e.Compile(context.Background(), wasmtest.Upper(), wasm.CompileOptions{})
	if err != nil {
		b.Fatal(err)
	}
	doc := wasm.Document{Payload: []byte("some payload of modest size")}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := m.Run(context.Background(), doc, wasm.RunOptions{}); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkTimeoutOverhead compares calls with and without the deadline machinery.
func BenchmarkTimeoutOverhead(b *testing.B) {
	e := wasm.NewEngine(wasm.EngineOptions{})
	defer func() { _ = e.Close(context.Background()) }()
	m, err := e.Compile(context.Background(), wasmtest.Echo(1, -1), wasm.CompileOptions{})
	if err != nil {
		b.Fatal(err)
	}
	doc := wasm.Document{Payload: []byte("x")}
	for name, ro := range map[string]wasm.RunOptions{"deadline": {}, "none": {Timeout: -1}} {
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := m.Run(context.Background(), doc, ro); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
