// SPDX-License-Identifier: AGPL-3.0-only

package wasmtest

// Hand-assembled guests for the Nurtuo script ABI v1. All of them share a bump allocator (alloc)
// that grows memory on demand and returns 0 when memory.grow fails, so host-side memory limits
// surface as a failed alloc. Frames are `u32 count | (u32 propsLen | props JSON | u32 payloadLen |
// payload)*`, identical for input (count 1) and output.

const hostModule = "nurtuo"

// guest holds the module under construction and the indexes of its well-known functions.
type guest struct {
	m     *Module
	alloc uint32
	heap  uint32
}

// newGuest declares memory (min pages, max < 0 for none), the heap pointer global, and alloc.
// imports must be declared by the caller through the returned module before newGuest's alloc is
// emitted, so callers pass a setup func that declares imports first.
func newGuest(minPages uint32, maxPages int64, imports func(m *Module)) *guest {
	m := New().Memory(minPages, maxPages)
	if imports != nil {
		imports(m)
	}
	g := &guest{m: m, heap: m.Global(1024)}
	c := C()
	c.GlobalGet(g.heap).LocalSet(1)
	c.LocalGet(1).LocalGet(0).I32Add().LocalSet(2)
	c.LocalGet(2).LocalGet(1).I32LtU().If().I32Const(0).Return().End() // overflow
	c.MemorySize().I32Const(16).I32Shl().LocalSet(3)
	c.LocalGet(2).LocalGet(3).I32GtU().If()
	c.LocalGet(2).LocalGet(3).I32Sub().I32Const(65535).I32Add().I32Const(16).I32ShrU().MemoryGrow().I32Const(-1).I32Eq().If().I32Const(0).Return().End()
	c.End()
	c.LocalGet(2).GlobalSet(g.heap)
	c.LocalGet(1)
	g.alloc = m.Func([]byte{I32}, []byte{I32}, []byte{I32, I32, I32}, c)
	m.Export("alloc", KindFunc, g.alloc)
	m.Export("memory", KindMemory, 0)
	return g
}

// pack leaves (ptr<<32 | len) on the stack for the given locals.
func pack(c *Code, ptr, ln uint32) *Code {
	return c.LocalGet(ptr).I64ExtendI32U().I64Const(32).I64Shl().LocalGet(ln).I64ExtendI32U().I64Or()
}

func (g *guest) process(locals []byte, c *Code) []byte {
	idx := g.m.Func([]byte{I32, I32}, []byte{I64}, locals, c)
	g.m.Export("process", KindFunc, idx)
	return g.m.Bytes()
}

// Echo returns the input frame unchanged. minPages/maxPages configure the declared memory
// (maxPages < 0: unbounded).
func Echo(minPages uint32, maxPages int64) []byte {
	g := newGuest(minPages, maxPages, nil)
	return g.process(nil, pack(C(), 0, 1))
}

// Upper ASCII-uppercases the payload of the input document in place and returns the frame.
func Upper() []byte {
	g := newGuest(1, -1, nil)
	c := C()
	c.LocalGet(0).I32Const(8).I32Add().LocalGet(0).I32Load(4).I32Add().LocalSet(2) // address of payloadLen
	c.LocalGet(2).I32Load(0).LocalSet(3)                                           // payloadLen
	c.LocalGet(2).I32Const(4).I32Add().LocalSet(2)                                 // i = payload start
	c.LocalGet(2).LocalGet(3).I32Add().LocalSet(3)                                 // end
	c.Block().Loop()
	c.LocalGet(2).LocalGet(3).I32GeU().BrIf(1)
	c.LocalGet(2).I32Load8U(0).I32Const(97).I32Sub().I32Const(26).I32LtU().If()
	c.LocalGet(2).LocalGet(2).I32Load8U(0).I32Const(32).I32Sub().I32Store8(0)
	c.End()
	c.LocalGet(2).I32Const(1).I32Add().LocalSet(2)
	c.Br(0)
	c.End().End()
	pack(c, 0, 1)
	return g.process([]byte{I32, I32}, c)
}

// Dup returns two copies of the input document (fan-out).
func Dup() []byte {
	g := newGuest(1, -1, nil)
	c := C()
	c.LocalGet(1).I32Const(4).I32Sub().LocalSet(2) // docLen
	c.LocalGet(2).I32Const(1).I32Shl().I32Const(4).I32Add().Call(g.alloc).LocalSet(3)
	c.LocalGet(3).I32Const(2).I32Store(0)
	c.LocalGet(3).I32Const(4).I32Add().LocalGet(0).I32Const(4).I32Add().LocalGet(2).MemoryCopy()
	c.LocalGet(3).I32Const(4).I32Add().LocalGet(2).I32Add().LocalGet(0).I32Const(4).I32Add().LocalGet(2).MemoryCopy()
	c.LocalGet(2).I32Const(1).I32Shl().I32Const(4).I32Add().LocalSet(2)
	pack(c, 3, 2)
	return g.process([]byte{I32, I32}, c)
}

// Drop returns zero documents.
func Drop() []byte {
	g := newGuest(1, -1, nil)
	return g.process(nil, C().I64Const(0))
}

// Trap executes `unreachable`.
func Trap() []byte {
	g := newGuest(1, -1, nil)
	return g.process(nil, C().Unreachable())
}

// Spin loops forever.
func Spin() []byte {
	g := newGuest(1, -1, nil)
	return g.process(nil, C().Loop().Br(0).End().I64Const(0))
}

// BadOutput returns a pointer outside of memory.
func BadOutput() []byte {
	g := newGuest(1, -1, nil)
	return g.process(nil, C().I64Const(0x7fffff00_00000064))
}

// NoProcess is a module that violates the ABI (no process export).
func NoProcess() []byte {
	g := newGuest(1, -1, nil)
	_ = g
	return g.m.Bytes()
}

// Fail reports the error "boom" through nurtuo.error and returns no output.
func Fail() []byte {
	var errFn uint32
	g := newGuest(1, -1, func(m *Module) { errFn = m.Import(hostModule, "error", []byte{I32, I32}, nil) })
	g.m.Data(16, []byte("boom"))
	return g.process(nil, C().I32Const(16).I32Const(4).Call(errFn).I64Const(0))
}

// LogEcho logs "hello from guest" at INFO and echoes the input.
func LogEcho() []byte {
	var logFn uint32
	g := newGuest(1, -1, func(m *Module) { logFn = m.Import(hostModule, "log", []byte{I32, I32, I32}, nil) })
	msg := []byte("hello from guest")
	g.m.Data(16, msg)
	c := C().I32Const(1).I32Const(16).I32Const(int32(len(msg))).Call(logFn)
	pack(c, 0, 1)
	return g.process(nil, c)
}

// ClockEcho calls nurtuo.clock_ms (capability "clock") and echoes the input.
func ClockEcho() []byte {
	var fn uint32
	g := newGuest(1, -1, func(m *Module) { fn = m.Import(hostModule, "clock_ms", nil, []byte{I64}) })
	c := C().Call(fn).Drop()
	pack(c, 0, 1)
	return g.process(nil, c)
}

// ClockProp calls nurtuo.clock_ms and returns a document whose payload is the 8 little-endian
// bytes of the result (props "{}"), so tests can observe the clock the host provided.
func ClockProp() []byte {
	var fn uint32
	g := newGuest(1, -1, func(m *Module) { fn = m.Import(hostModule, "clock_ms", nil, []byte{I64}) })
	// frame at 256: count=1 | propsLen=2 | "{}" | payloadLen=8 | 8 bytes
	g.m.Data(256, append(append(append(LE32(1), LE32(2)...), '{', '}'), LE32(8)...))
	c := C()
	// i64.store of the clock at 256+4+4+2+4 = 270 (alignment 1): emit raw: addr, value, store.
	c.I32Const(270).Call(fn)
	c.op(0x37, 0x00, 0x00) // i64.store align=0 offset=0
	c.I64Const(256<<32 | 22)
	return g.process(nil, c)
}

// RandomEcho calls nurtuo.random_get (capability "random") for 8 bytes and echoes the input.
func RandomEcho() []byte {
	var fn uint32
	g := newGuest(1, -1, func(m *Module) { fn = m.Import(hostModule, "random_get", []byte{I32, I32}, nil) })
	c := C().I32Const(64).I32Const(8).Call(fn)
	pack(c, 0, 1)
	return g.process(nil, c)
}
