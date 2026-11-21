// SPDX-License-Identifier: AGPL-3.0-only

package wasm

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const hostModuleName = "nurtuo"

// invocation is the per-call state host functions reach through the context. It is only touched
// from the goroutine running the guest, so it needs no locking.
type invocation struct {
	ctx     context.Context
	logger  *slog.Logger
	name    string
	maxLogs int
	logs    int
	caps    map[Capability]bool
	now     func() time.Time
	rand    io.Reader

	failure string // message set through nurtuo.error
	denied  string // capability name of the first denied call
}

type invKey struct{}

func withInvocation(ctx context.Context, st *invocation) context.Context {
	return context.WithValue(ctx, invKey{}, st)
}

func invFrom(ctx context.Context) *invocation { return ctx.Value(invKey{}).(*invocation) }

// hostFuncs is the set of names accepted under the "nurtuo" import module.
var hostFuncs = map[string]struct{}{"log": {}, "error": {}, "clock_ms": {}, "random_get": {}}

// deny records the denial and traps the guest. wazero recovers the panic into an error from Call.
func (st *invocation) deny(c Capability) {
	if st.denied == "" {
		st.denied = string(c)
	}
	panic("capability denied: " + string(c))
}

func readString(m api.Module, ptr, n uint32, limit uint32) string {
	if n > limit {
		n = limit
	}
	b, ok := m.Memory().Read(ptr, n)
	if !ok {
		return ""
	}
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}

// instantiateHost registers the "nurtuo" host module and WASI (no fs/env/args/sockets, stdio
// discarded) in r.
func (e *Engine) instantiateHost(ctx context.Context, r wazero.Runtime) error {
	_, err := r.NewHostModuleBuilder(hostModuleName).
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, s []uint64) {
		st := invFrom(ctx)
		if st.logs >= st.maxLogs {
			return
		}
		st.logs++
		if st.logger == nil {
			return
		}
		lvl := slog.LevelInfo
		switch int32(s[0]) {
		case 0:
			lvl = slog.LevelDebug
		case 2:
			lvl = slog.LevelWarn
		case 3:
			lvl = slog.LevelError
		}
		st.logger.Log(st.ctx, lvl, readString(m, uint32(s[1]), uint32(s[2]), maxLogBytes), "script", st.name)
	}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, nil).Export("log").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, s []uint64) {
		st := invFrom(ctx)
		if st.failure == "" {
			st.failure = readString(m, uint32(s[0]), uint32(s[1]), maxLogBytes)
			if st.failure == "" {
				st.failure = "script reported an error"
			}
		}
	}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, nil).Export("error").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, _ api.Module, s []uint64) {
		st := invFrom(ctx)
		if !st.caps[CapClock] {
			st.deny(CapClock)
		}
		s[0] = uint64(st.now().UnixMilli())
	}), nil, []api.ValueType{api.ValueTypeI64}).Export("clock_ms").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, s []uint64) {
		st := invFrom(ctx)
		if !st.caps[CapRandom] {
			st.deny(CapRandom)
		}
		b, ok := m.Memory().Read(uint32(s[0]), uint32(s[1]))
		if !ok {
			panic("random_get: out of range")
		}
		if _, err := io.ReadFull(st.rand, b); err != nil {
			panic("random_get: " + err.Error())
		}
	}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, nil).Export("random_get").
		Instantiate(ctx)
	if err != nil {
		return err
	}
	_, err = wasi_snapshot_preview1.Instantiate(ctx, r)
	return err
}
