// SPDX-License-Identifier: AGPL-3.0-only

package wasm_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/script/wasm/wasmtest"
)

func newEngine(t testing.TB, opts wasm.EngineOptions) *wasm.Engine {
	t.Helper()
	e := wasm.NewEngine(opts)
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

func compile(t testing.TB, e *wasm.Engine, b []byte, mem uint64) *wasm.Module {
	t.Helper()
	m, err := e.Compile(context.Background(), b, wasm.CompileOptions{MemoryBytes: mem})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return m
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var we *wasm.Error
	if !errors.As(err, &we) || we.Code != code {
		t.Fatalf("want error code %q, got %v", code, err)
	}
}

var in = wasm.Document{Props: map[string]string{"k": "v"}, Payload: []byte("hello wasm")}

func TestFrameRoundTrip(t *testing.T) {
	docs := []wasm.Document{in, {Payload: nil}, {Props: map[string]string{"a": "ü"}, Payload: bytes.Repeat([]byte{0, 1, 2}, 1000)}}
	b, err := wasm.EncodeFrame(docs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := wasm.DecodeFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got[0].Payload) != "hello wasm" || got[0].Props["k"] != "v" || got[2].Props["a"] != "ü" || len(got[2].Payload) != 3000 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestDecodeFrameRejectsGarbage(t *testing.T) {
	huge := binary.LittleEndian.AppendUint32(nil, 0xffffffff)
	for name, b := range map[string][]byte{
		"short":    {1, 2},
		"huge n":   huge,
		"truncate": {1, 0, 0, 0, 5, 0, 0, 0, '{'},
		"trailing": append(binary.LittleEndian.AppendUint32(nil, 0), 9),
	} {
		if _, err := wasm.DecodeFrame(b); !errors.Is(err, wasm.ErrFrame) {
			t.Errorf("%s: want ErrFrame, got %v", name, err)
		}
	}
}

func TestEchoSuccess(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	m := compile(t, e, wasmtest.Echo(1, -1), 0)
	out, err := m.Run(context.Background(), in, wasm.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || string(out[0].Payload) != "hello wasm" || out[0].Props["k"] != "v" {
		t.Fatalf("out = %+v", out)
	}
}

func TestUpperTransformsPayload(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	m := compile(t, e, wasmtest.Upper(), 0)
	out, err := m.Run(context.Background(), in, wasm.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0].Payload) != "HELLO WASM" || out[0].Props["k"] != "v" {
		t.Fatalf("out = %+v", out[0])
	}
}

func TestFanOutAndDrop(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	out, err := compile(t, e, wasmtest.Dup(), 0).Run(context.Background(), in, wasm.RunOptions{})
	if err != nil || len(out) != 2 || string(out[1].Payload) != "hello wasm" {
		t.Fatalf("dup: %v %+v", err, out)
	}
	out, err = compile(t, e, wasmtest.Drop(), 0).Run(context.Background(), in, wasm.RunOptions{})
	if err != nil || len(out) != 0 {
		t.Fatalf("drop: %v %+v", err, out)
	}
}

func TestTrap(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	m := compile(t, e, wasmtest.Trap(), 0)
	_, err := m.Run(context.Background(), in, wasm.RunOptions{})
	wantCode(t, err, wasm.CodeTrap)
	// the engine survives a trap: a good module still runs afterwards
	if _, err := compile(t, e, wasmtest.Echo(1, -1), 0).Run(context.Background(), in, wasm.RunOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestGuestReportedError(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	_, err := compile(t, e, wasmtest.Fail(), 0).Run(context.Background(), in, wasm.RunOptions{})
	wantCode(t, err, wasm.CodeScript)
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("message lost: %v", err)
	}
}

func TestInfiniteLoopTimesOut(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	m := compile(t, e, wasmtest.Spin(), 0)
	start := time.Now()
	_, err := m.Run(context.Background(), in, wasm.RunOptions{Timeout: 100 * time.Millisecond})
	wantCode(t, err, wasm.CodeTimeout)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	// the module is reusable after a timeout
	if _, err := compile(t, e, wasmtest.Echo(1, -1), 0).Run(context.Background(), in, wasm.RunOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCallerCancellation(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	m := compile(t, e, wasmtest.Spin(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := m.Run(ctx, in, wasm.RunOptions{Timeout: -1})
	wantCode(t, err, wasm.CodeTimeout)
}

func TestMemoryLimit(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	// declared minimum (2000 pages ~ 125 MiB) above the 16 MiB cap
	_, err := e.Compile(context.Background(), wasmtest.Echo(2000, -1), wasm.CompileOptions{MemoryBytes: 16 << 20})
	if err == nil {
		m := compile(t, e, wasmtest.Echo(2000, -1), 16<<20)
		_, err = m.Run(context.Background(), in, wasm.RunOptions{})
	}
	wantCode(t, err, wasm.CodeMemoryLimit)

	// an input that cannot fit in the cap makes the guest's alloc fail
	m := compile(t, e, wasmtest.Echo(1, -1), 8<<20)
	big := wasm.Document{Payload: bytes.Repeat([]byte("x"), 12<<20)}
	_, err = m.Run(context.Background(), big, wasm.RunOptions{})
	wantCode(t, err, wasm.CodeMemoryLimit)
}

func TestLargePayload(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	m := compile(t, e, wasmtest.Upper(), 128<<20)
	payload := bytes.Repeat([]byte("abcdefgh"), 4<<20) // 32 MiB
	out, err := m.Run(context.Background(), wasm.Document{Payload: payload}, wasm.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].Payload) != len(payload) || !bytes.HasPrefix(out[0].Payload, []byte("ABCDEFGHABCDEFGH")) || !bytes.HasSuffix(out[0].Payload, []byte("ABCDEFGH")) {
		t.Fatalf("large payload transformed wrongly (len %d)", len(out[0].Payload))
	}
}

func TestInputAndOutputLimits(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	m := compile(t, e, wasmtest.Echo(1, -1), 0)
	_, err := m.Run(context.Background(), wasm.Document{Payload: make([]byte, 1000)}, wasm.RunOptions{MaxInput: 100})
	wantCode(t, err, wasm.CodeInput)
	_, err = m.Run(context.Background(), wasm.Document{Payload: make([]byte, 1000)}, wasm.RunOptions{MaxOutput: 100})
	wantCode(t, err, wasm.CodeOutputTooLarge)
	_, err = compile(t, e, wasmtest.BadOutput(), 0).Run(context.Background(), in, wasm.RunOptions{})
	wantCode(t, err, wasm.CodeOutput)
}

func TestCapabilities(t *testing.T) {
	fixed := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	e := newEngine(t, wasm.EngineOptions{Now: func() time.Time { return fixed }, Rand: bytes.NewReader(make([]byte, 1<<10))})
	ctx := context.Background()

	for name, mod := range map[string][]byte{"clock": wasmtest.ClockEcho(), "random": wasmtest.RandomEcho()} {
		m := compile(t, e, mod, 0)
		_, err := m.Run(ctx, in, wasm.RunOptions{})
		wantCode(t, err, wasm.CodeCapabilityDenied)
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: message should name the capability: %v", name, err)
		}
		if _, err := m.Run(ctx, in, wasm.RunOptions{Capabilities: []wasm.Capability{wasm.Capability(name)}}); err != nil {
			t.Errorf("%s granted: %v", name, err)
		}
		// granting the other capability does not help
		other := wasm.CapClock
		if name == "clock" {
			other = wasm.CapRandom
		}
		_, err = m.Run(ctx, in, wasm.RunOptions{Capabilities: []wasm.Capability{other}})
		wantCode(t, err, wasm.CodeCapabilityDenied)
	}

	out, err := compile(t, e, wasmtest.ClockProp(), 0).Run(ctx, in, wasm.RunOptions{Capabilities: []wasm.Capability{wasm.CapClock}})
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(binary.LittleEndian.Uint64(out[0].Payload)); got != fixed.UnixMilli() {
		t.Fatalf("clock = %d, want %d", got, fixed.UnixMilli())
	}
}

func TestInvalidAndABIViolations(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	_, err := e.Compile(context.Background(), []byte("not wasm"), wasm.CompileOptions{})
	wantCode(t, err, wasm.CodeInvalidModule)
	_, err = e.Compile(context.Background(), wasmtest.NoProcess(), wasm.CompileOptions{})
	wantCode(t, err, wasm.CodeABI)
}

func TestCompileCache(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	a := compile(t, e, wasmtest.Echo(1, -1), 0)
	b := compile(t, e, wasmtest.Echo(1, -1), 0)
	if a != b {
		t.Fatal("same bytes and limits should return the cached module")
	}
	if c := compile(t, e, wasmtest.Echo(1, -1), 4<<20); c == a {
		t.Fatal("different memory limit must not share a module")
	}
	if got := a.MemoryLimitBytes(); got != wasm.DefaultMemoryBytes {
		t.Fatalf("default limit = %d", got)
	}
}

func TestGuestLogging(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newEngine(t, wasm.EngineOptions{Logger: lg})
	if _, err := compile(t, e, wasmtest.LogEcho(), 0).Run(context.Background(), in, wasm.RunOptions{Name: "step1"}); err != nil {
		t.Fatal(err)
	}
	if s := buf.String(); !strings.Contains(s, "hello from guest") || !strings.Contains(s, "script=step1") {
		t.Fatalf("log output: %q", s)
	}
}

func TestConcurrentCalls(t *testing.T) {
	e := newEngine(t, wasm.EngineOptions{})
	up := compile(t, e, wasmtest.Upper(), 0)
	trap := compile(t, e, wasmtest.Trap(), 0)
	spin := compile(t, e, wasmtest.Spin(), 0)
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc := wasm.Document{Props: map[string]string{"i": fmt.Sprint(i)}, Payload: []byte(fmt.Sprintf("doc-%d", i))}
			switch i % 10 {
			case 7:
				if _, err := trap.Run(context.Background(), doc, wasm.RunOptions{}); err == nil {
					errs <- errors.New("trap did not fail")
				}
			case 3:
				if _, err := spin.Run(context.Background(), doc, wasm.RunOptions{Timeout: 50 * time.Millisecond}); err == nil {
					errs <- errors.New("spin did not time out")
				}
			default:
				out, err := up.Run(context.Background(), doc, wasm.RunOptions{})
				if err != nil {
					errs <- err
					return
				}
				if want := strings.ToUpper(string(doc.Payload)); string(out[0].Payload) != want || out[0].Props["i"] != fmt.Sprint(i) {
					errs <- fmt.Errorf("doc %d: got %q props %v", i, out[0].Payload, out[0].Props)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
