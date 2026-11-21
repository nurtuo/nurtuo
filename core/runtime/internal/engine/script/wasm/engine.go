// SPDX-License-Identifier: AGPL-3.0-only

package wasm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// Capability is an opt-in privilege of a script. Nothing is granted by default.
type Capability string

// Known capabilities.
const (
	// CapClock exposes the real wall clock (nurtuo.clock_ms and WASI clock_time_get).
	CapClock Capability = "clock"
	// CapRandom exposes real randomness (nurtuo.random_get and WASI random_get).
	CapRandom Capability = "random"
)

// Defaults applied by CompileOptions and RunOptions for zero values.
const (
	DefaultMemoryBytes = 32 << 20
	DefaultTimeout     = 10 * time.Second
	DefaultMaxInput    = 64 << 20
	DefaultMaxOutput   = 64 << 20
	DefaultMaxLogs     = 100
	maxLogBytes        = 4096
	pageSize           = 65536
	maxPages           = 65536
)

// EngineOptions configures an Engine.
type EngineOptions struct {
	// Logger receives guest log calls; nil discards them.
	Logger *slog.Logger
	// Now is the clock given to guests holding CapClock. Nil means time.Now. Tests inject a fixed
	// clock here to make clock-dependent guests deterministic.
	Now func() time.Time
	// Rand is the entropy source for guests holding CapRandom. Nil means crypto/rand.
	Rand io.Reader
}

// CompileOptions are fixed at compile time because wazero applies the memory limit per runtime.
type CompileOptions struct {
	// MemoryBytes caps the linear memory of every instance (rounded down to 64 KiB pages, at
	// most 4 GiB). 0 selects DefaultMemoryBytes. A module whose declared minimum memory exceeds
	// the cap fails to compile/instantiate with CodeMemoryLimit.
	MemoryBytes uint64
}

// RunOptions are per-invocation limits and grants.
type RunOptions struct {
	// Timeout bounds the whole invocation (instantiate, alloc, process) wall-clock time. It is
	// enforced by wazero through the context deadline (CloseOnContextDone): the guest is
	// interrupted even inside a tight loop. 0 selects DefaultTimeout; negative disables it
	// (the caller's own context still applies).
	Timeout time.Duration
	// Capabilities lists the privileges granted to the guest.
	Capabilities []Capability
	// MaxInput and MaxOutput bound the encoded frames (0: defaults).
	MaxInput, MaxOutput int64
	// MaxLogs bounds nurtuo.log calls per invocation; extra calls are dropped (0: default).
	MaxLogs int
	// Name is attached to log records (for example the step id).
	Name string
}

// Engine owns the wazero runtimes and the compile caches. It is safe for concurrent use. Each
// distinct memory limit gets its own wazero.Runtime (the limit is a runtime property); they share
// one wazero.CompilationCache so the machine code for a module is generated once.
type Engine struct {
	opts  EngineOptions
	cache wazero.CompilationCache

	mu      sync.Mutex
	runtime map[uint32]*runtimeEntry
	closed  bool
}

type runtimeEntry struct {
	r    wazero.Runtime
	mods map[[sha256.Size]byte]*Module
}

// NewEngine returns an Engine. Close it to release compiled code.
func NewEngine(opts EngineOptions) *Engine {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	return &Engine{opts: opts, cache: wazero.NewCompilationCache(), runtime: map[uint32]*runtimeEntry{}}
}

// Close releases all runtimes and the compilation cache. Running invocations are interrupted.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	var errs []error
	for _, re := range e.runtime {
		errs = append(errs, re.r.Close(ctx))
	}
	e.runtime = map[uint32]*runtimeEntry{}
	errs = append(errs, e.cache.Close(ctx))
	return errors.Join(errs...)
}

func pagesFor(bytes uint64) uint32 {
	if bytes == 0 {
		bytes = DefaultMemoryBytes
	}
	p := bytes / pageSize
	if p == 0 {
		p = 1
	}
	if p > maxPages {
		p = maxPages
	}
	return uint32(p)
}

// Compile validates and compiles wasmBytes, caching by content hash and memory limit: compiling
// the same bytes again with the same options returns the same *Module without recompiling.
func (e *Engine) Compile(ctx context.Context, wasmBytes []byte, co CompileOptions) (*Module, error) {
	pages := pagesFor(co.MemoryBytes)
	sum := sha256.Sum256(wasmBytes)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, &Error{Code: CodeInternal, Message: "engine closed"}
	}
	re := e.runtime[pages]
	if re == nil {
		cfg := wazero.NewRuntimeConfig().
			WithCloseOnContextDone(true).
			WithMemoryLimitPages(pages).
			WithCompilationCache(e.cache)
		r := wazero.NewRuntimeWithConfig(ctx, cfg)
		if err := e.instantiateHost(ctx, r); err != nil {
			_ = r.Close(ctx)
			return nil, &Error{Code: CodeInternal, Message: "instantiate host modules", Cause: err}
		}
		re = &runtimeEntry{r: r, mods: map[[sha256.Size]byte]*Module{}}
		e.runtime[pages] = re
	}
	if m, ok := re.mods[sum]; ok {
		return m, nil
	}
	cm, err := re.r.CompileModule(ctx, wasmBytes)
	if err != nil {
		code := CodeInvalidModule
		if strings.Contains(err.Error(), "memory") {
			code = CodeMemoryLimit
		}
		return nil, &Error{Code: code, Message: "compile: " + err.Error(), Cause: err}
	}
	m, err := newModule(e, re.r, cm, pages)
	if err != nil {
		_ = cm.Close(ctx)
		return nil, err
	}
	re.mods[sum] = m
	return m, nil
}

// Module is a compiled, validated guest. Run may be called concurrently from many goroutines.
type Module struct {
	e        *Engine
	r        wazero.Runtime
	compiled wazero.CompiledModule
	pages    uint32
	hasInit  bool
}

func newModule(e *Engine, r wazero.Runtime, cm wazero.CompiledModule, pages uint32) (*Module, error) {
	for _, f := range cm.ImportedFunctions() {
		mod, name, _ := f.Import()
		switch mod {
		case hostModuleName:
			if _, ok := hostFuncs[name]; !ok {
				return nil, &Error{Code: CodeInvalidModule, Message: fmt.Sprintf("unknown host import %s.%s", mod, name)}
			}
		case wasi_snapshot_preview1.ModuleName:
		default:
			return nil, &Error{Code: CodeInvalidModule, Message: fmt.Sprintf("unsupported import %s.%s (only %q and %q are provided)", mod, name, hostModuleName, wasi_snapshot_preview1.ModuleName)}
		}
	}
	if len(cm.ImportedMemories()) > 0 {
		return nil, &Error{Code: CodeInvalidModule, Message: "modules must define their own memory"}
	}
	exp := cm.ExportedFunctions()
	if !sigIs(exp["alloc"], []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}) {
		return nil, &Error{Code: CodeABI, Message: "module must export alloc(i32) -> i32"}
	}
	if !sigIs(exp["process"], []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI64}) {
		return nil, &Error{Code: CodeABI, Message: "module must export process(i32, i32) -> i64"}
	}
	if _, ok := cm.ExportedMemories()["memory"]; !ok {
		return nil, &Error{Code: CodeABI, Message: "module must export memory"}
	}
	_, hasInit := exp["_initialize"]
	return &Module{e: e, r: r, compiled: cm, pages: pages, hasInit: hasInit}, nil
}

func sigIs(d api.FunctionDefinition, params, results []api.ValueType) bool {
	if d == nil {
		return false
	}
	eq := func(a, b []api.ValueType) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	return eq(d.ParamTypes(), params) && eq(d.ResultTypes(), results)
}

// MemoryLimitBytes reports the effective memory cap of instances of this module.
func (m *Module) MemoryLimitBytes() uint64 { return uint64(m.pages) * pageSize }

// Run invokes the guest on one document in a fresh instance (nothing leaks between calls, and
// concurrent calls share no state). The returned error is always an *Error.
func (m *Module) Run(ctx context.Context, in Document, ro RunOptions) (out []Document, err error) {
	if ro.Timeout == 0 {
		ro.Timeout = DefaultTimeout
	}
	if ro.MaxInput <= 0 {
		ro.MaxInput = DefaultMaxInput
	}
	if ro.MaxOutput <= 0 {
		ro.MaxOutput = DefaultMaxOutput
	}
	if ro.MaxLogs <= 0 {
		ro.MaxLogs = DefaultMaxLogs
	}
	frame, ferr := EncodeFrame([]Document{in})
	if ferr != nil {
		return nil, &Error{Code: CodeInput, Message: ferr.Error(), Cause: ferr}
	}
	if int64(len(frame)) > ro.MaxInput {
		return nil, &Error{Code: CodeInput, Message: fmt.Sprintf("input of %d bytes exceeds limit %d", len(frame), ro.MaxInput)}
	}
	if ro.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ro.Timeout)
		defer cancel()
	}
	st := &invocation{
		ctx: ctx, logger: m.e.opts.Logger, name: ro.Name, maxLogs: ro.MaxLogs,
		caps: map[Capability]bool{}, now: m.e.opts.Now, rand: m.e.opts.Rand,
	}
	for _, c := range ro.Capabilities {
		st.caps[c] = true
	}
	ctx = withInvocation(ctx, st)

	mc := wazero.NewModuleConfig().WithName("").WithStartFunctions()
	if m.hasInit {
		mc = mc.WithStartFunctions("_initialize")
	}
	if st.caps[CapClock] {
		now := m.e.opts.Now
		mc = mc.
			WithWalltime(func() (int64, int32) { t := now(); return t.Unix(), int32(t.Nanosecond()) }, sys.ClockResolution(time.Microsecond.Nanoseconds())).
			WithNanotime(func() int64 { return now().UnixNano() }, sys.ClockResolution(time.Microsecond.Nanoseconds()))
	}
	if st.caps[CapRandom] {
		mc = mc.WithRandSource(m.e.opts.Rand)
	}

	mod, ierr := m.r.InstantiateModule(ctx, m.compiled, mc)
	if ierr != nil {
		return nil, classify(ctx, st, "instantiate", ierr)
	}
	defer func() { _ = mod.Close(context.WithoutCancel(ctx)) }()

	allocRes, aerr := mod.ExportedFunction("alloc").Call(ctx, uint64(len(frame)))
	if aerr != nil {
		return nil, classify(ctx, st, "alloc", aerr)
	}
	ptr := uint32(allocRes[0])
	mem := mod.Memory()
	if ptr == 0 || !mem.Write(ptr, frame) {
		return nil, &Error{Code: CodeMemoryLimit, Message: fmt.Sprintf("guest could not allocate %d bytes for the input (memory limit %d bytes)", len(frame), m.MemoryLimitBytes())}
	}
	res, perr := mod.ExportedFunction("process").Call(ctx, uint64(ptr), uint64(len(frame)))
	if perr != nil {
		return nil, classify(ctx, st, "process", perr)
	}
	if st.failure != "" {
		return nil, &Error{Code: CodeScript, Message: st.failure}
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0])
	if outLen == 0 {
		return nil, nil
	}
	if int64(outLen) > ro.MaxOutput {
		return nil, &Error{Code: CodeOutputTooLarge, Message: fmt.Sprintf("output of %d bytes exceeds limit %d", outLen, ro.MaxOutput)}
	}
	raw, ok := mem.Read(outPtr, outLen)
	if !ok {
		return nil, &Error{Code: CodeOutput, Message: fmt.Sprintf("process returned out-of-range output %d+%d", outPtr, outLen)}
	}
	docs, derr := DecodeFrame(raw)
	if derr != nil {
		return nil, &Error{Code: CodeOutput, Message: derr.Error(), Cause: derr}
	}
	return docs, nil
}
