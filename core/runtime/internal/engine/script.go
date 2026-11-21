// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/maps"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// ScriptSource resolves the `script` name of a script step to WebAssembly module bytes. The
// control plane's component repository will implement it; ScriptModules is the in-memory one.
type ScriptSource interface {
	LoadScript(ctx context.Context, name string) ([]byte, error)
}

// ScriptModules is an in-memory ScriptSource. Safe for concurrent use.
type ScriptModules struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewScriptModules returns an empty registry.
func NewScriptModules() *ScriptModules { return &ScriptModules{m: map[string][]byte{}} }

// Register adds or replaces a module.
func (s *ScriptModules) Register(name string, wasmBytes []byte) {
	s.mu.Lock()
	s.m[name] = wasmBytes
	s.mu.Unlock()
}

// LoadScript implements ScriptSource.
func (s *ScriptModules) LoadScript(_ context.Context, name string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.m[name]
	if !ok {
		return nil, fmt.Errorf("script %q is not registered", name)
	}
	return b, nil
}

// ScriptRunner is the StepRunner of the `script` step kind. For every OK document it runs the
// step's WASM module in a fresh sandboxed instance (see package wasm for the ABI and limits):
//
//   - zero output documents: the document is dropped;
//   - one or more outputs: the input is replaced by them (output props override the input's;
//     ids are "<id>" for a single output and "<id>.<n>" for several);
//   - any failure (trap, timeout, memory limit, denied capability, malformed output, error
//     reported by the script) fails only that document with code "script.<code>" and the other
//     documents of the set are unaffected.
//
// A step-level problem (unknown script, invalid module, unknown capability) fails the step, like
// a map that does not compile. Non-OK input documents pass through.
type ScriptRunner struct {
	Engine *wasm.Engine
	Source ScriptSource
	// Concurrency bounds parallel guest invocations per step execution (<=0: GOMAXPROCS).
	Concurrency int
}

// NewScriptRunner returns a runner using e to execute modules loaded from src.
func NewScriptRunner(e *wasm.Engine, src ScriptSource) *ScriptRunner {
	return &ScriptRunner{Engine: e, Source: src}
}

// RegisterScriptRunner installs sr as the runner of `script` steps.
func (r *Registry) RegisterScriptRunner(sr *ScriptRunner) { r.Register(flow.KindScript, sr) }

// Run implements StepRunner.
func (r *ScriptRunner) Run(ctx context.Context, sc *StepContext, in *docs.Set) (*docs.Set, error) {
	cfg := sc.Config.(flow.ScriptConfig)
	stepErr := func(code string, err error) error {
		return &docs.Error{Step: sc.Step.ID, Code: "script." + code, Message: err.Error(), Cause: err}
	}
	var caps []wasm.Capability
	for _, c := range cfg.Capabilities {
		switch wasm.Capability(c) {
		case wasm.CapClock, wasm.CapRandom:
			caps = append(caps, wasm.Capability(c))
		default:
			return in, stepErr("capability_unknown", fmt.Errorf("unknown capability %q", c))
		}
	}
	src, err := r.Source.LoadScript(ctx, cfg.Script)
	if err != nil {
		return in, stepErr("load", err)
	}
	mod, err := r.Engine.Compile(ctx, src, wasm.CompileOptions{MemoryBytes: uint64(cfg.MemoryMiB) << 20})
	if err != nil {
		return in, stepErr(wasmCode(err), err)
	}
	ro := wasm.RunOptions{Capabilities: caps, Name: sc.Step.ID}
	if cfg.TimeoutMs > 0 {
		ro.Timeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}

	n := in.Len()
	results := make([][]*docs.Document, n)
	conc := r.Concurrency
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
				results[i] = r.runOne(ctx, sc, mod, ro, in.At(i))
			}
		}()
	}
	for i := 0; i < n; i++ {
		idx <- i
	}
	close(idx)
	wg.Wait()

	out := docs.NewSet()
	for _, rs := range results {
		for _, d := range rs {
			out.Add(d)
		}
	}
	return out, ctx.Err()
}

func (r *ScriptRunner) runOne(ctx context.Context, sc *StepContext, mod *wasm.Module, ro wasm.RunOptions, d *docs.Document) (res []*docs.Document) {
	if d.Status() != docs.StatusOK {
		return []*docs.Document{d}
	}
	defer func() {
		if p := recover(); p != nil {
			d.Fail(&docs.Error{Step: sc.Step.ID, Code: "script.internal", Message: fmt.Sprint(p)})
			res = []*docs.Document{d}
		}
	}()
	fail := func(code string, err error) []*docs.Document {
		d.Fail(&docs.Error{Step: sc.Step.ID, Code: "script." + code, Message: err.Error(), Cause: err})
		return []*docs.Document{d}
	}
	var payload []byte
	if p := d.Payload(); p != nil {
		b, err := p.Bytes()
		if err != nil {
			return fail("payload", err)
		}
		payload = b
	}
	outs, err := mod.Run(ctx, wasm.Document{Props: d.Props, Payload: payload}, ro)
	if err != nil {
		return fail(wasmCode(err), err)
	}
	if len(outs) == 0 {
		d.Drop()
		return []*docs.Document{d}
	}
	res = make([]*docs.Document, 0, len(outs))
	for i, o := range outs {
		p, err := sc.Spool.FromBytes(o.Payload)
		if err != nil {
			for _, x := range res {
				_ = x.Release()
			}
			return fail("payload", err)
		}
		id := d.ID
		if len(outs) > 1 {
			id = fmt.Sprintf("%s.%d", d.ID, i+1)
		}
		res = append(res, docs.New(id, mergeProps(d.Props, o.Props), p))
	}
	_ = d.Release()
	return res
}

func wasmCode(err error) string {
	var we *wasm.Error
	if errors.As(err, &we) {
		return we.Code
	}
	return "internal"
}

// ScriptStepFor adapts a compiled module to the payload-only maps.ScriptStep interface: it runs the
// module over input and returns the payload of the first output document (nil if there is none).
func ScriptStepFor(m *wasm.Module, ro wasm.RunOptions) maps.ScriptStep { return moduleStep{m, ro} }

type moduleStep struct {
	m  *wasm.Module
	ro wasm.RunOptions
}

func (s moduleStep) Run(ctx context.Context, input []byte) ([]byte, error) {
	outs, err := s.m.Run(ctx, wasm.Document{Payload: input}, s.ro)
	if err != nil || len(outs) == 0 {
		return nil, err
	}
	return outs[0].Payload, nil
}
