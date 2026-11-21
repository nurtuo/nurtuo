// SPDX-License-Identifier: AGPL-3.0-only

// Package wasmtest is a tiny WebAssembly binary assembler plus a set of hand-assembled guest
// modules implementing the Nurtuo script ABI (see package wasm). It exists because the build
// environment has no TinyGo/Rust/AssemblyScript toolchain: every test module is produced at test
// time by the Go code in this package, so there are no opaque .wasm blobs in the repository.
//
// The assembler supports exactly what the guests need: function types, function imports, one
// memory, mutable i32 globals, exports, passive-free active data segments, and a code builder
// with the handful of instructions used (see Code).
package wasmtest

import "encoding/binary"

// Value types.
const (
	I32 byte = 0x7f
	I64 byte = 0x7e
)

// Export kinds.
const (
	KindFunc   byte = 0x00
	KindMemory byte = 0x02
)

type funcType struct{ params, results []byte }

type importFn struct {
	mod, name string
	typ       uint32
}

type function struct {
	typ    uint32
	locals []byte // one valtype per local (beyond params)
	body   []byte
}

type export struct {
	name string
	kind byte
	idx  uint32
}

type dataSeg struct {
	offset uint32
	data   []byte
}

// Module accumulates sections and renders them with Bytes.
type Module struct {
	types   []funcType
	imports []importFn
	funcs   []function
	exports []export
	data    []dataSeg
	globals []int32 // mutable i32 globals with initial values

	memMin, memMax uint32
	hasMem, hasMax bool
}

// New returns an empty module.
func New() *Module { return &Module{} }

func (m *Module) typeIndex(params, results []byte) uint32 {
	for i, t := range m.types {
		if string(t.params) == string(params) && string(t.results) == string(results) {
			return uint32(i)
		}
	}
	m.types = append(m.types, funcType{params, results})
	return uint32(len(m.types) - 1)
}

// Memory declares the linear memory (pages of 64 KiB). max < 0 means no maximum.
func (m *Module) Memory(min uint32, max int64) *Module {
	m.hasMem, m.memMin = true, min
	if max >= 0 {
		m.hasMax, m.memMax = true, uint32(max)
	}
	return m
}

// Global adds a mutable i32 global and returns its index.
func (m *Module) Global(init int32) uint32 {
	m.globals = append(m.globals, init)
	return uint32(len(m.globals) - 1)
}

// Import declares an imported function and returns its function index. All imports must be
// declared before any Func call (function indexes of defined functions follow the imports).
func (m *Module) Import(mod, name string, params, results []byte) uint32 {
	if len(m.funcs) > 0 {
		panic("wasmtest: Import after Func")
	}
	m.imports = append(m.imports, importFn{mod, name, m.typeIndex(params, results)})
	return uint32(len(m.imports) - 1)
}

// Func defines a function and returns its function index. locals lists the types of extra locals
// (their indexes follow the parameters). body is the instruction sequence without the final end.
func (m *Module) Func(params, results, locals []byte, body *Code) uint32 {
	m.funcs = append(m.funcs, function{m.typeIndex(params, results), locals, body.b})
	return uint32(len(m.imports) + len(m.funcs) - 1)
}

// Export exports a function (KindFunc) or the memory (KindMemory, idx 0).
func (m *Module) Export(name string, kind byte, idx uint32) *Module {
	m.exports = append(m.exports, export{name, kind, idx})
	return m
}

// Data places bytes at a fixed offset of memory 0 at instantiation.
func (m *Module) Data(offset uint32, b []byte) *Module {
	m.data = append(m.data, dataSeg{offset, b})
	return m
}

func uleb(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

func sleb(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

func vec(items [][]byte) []byte {
	out := uleb(uint64(len(items)))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func name(s string) []byte { return append(uleb(uint64(len(s))), s...) }

func section(id byte, payload []byte) []byte {
	return append(append([]byte{id}, uleb(uint64(len(payload)))...), payload...)
}

// Bytes renders the binary module.
func (m *Module) Bytes() []byte {
	out := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}

	var ts [][]byte
	for _, t := range m.types {
		e := []byte{0x60}
		e = append(e, uleb(uint64(len(t.params)))...)
		e = append(e, t.params...)
		e = append(e, uleb(uint64(len(t.results)))...)
		e = append(e, t.results...)
		ts = append(ts, e)
	}
	out = append(out, section(1, vec(ts))...)

	if len(m.imports) > 0 {
		var is [][]byte
		for _, im := range m.imports {
			e := append(name(im.mod), name(im.name)...)
			e = append(e, 0x00)
			e = append(e, uleb(uint64(im.typ))...)
			is = append(is, e)
		}
		out = append(out, section(2, vec(is))...)
	}

	var fs [][]byte
	for _, f := range m.funcs {
		fs = append(fs, uleb(uint64(f.typ)))
	}
	out = append(out, section(3, vec(fs))...)

	if m.hasMem {
		e := []byte{0x00}
		if m.hasMax {
			e[0] = 0x01
		}
		e = append(e, uleb(uint64(m.memMin))...)
		if m.hasMax {
			e = append(e, uleb(uint64(m.memMax))...)
		}
		out = append(out, section(5, vec([][]byte{e}))...)
	}

	if len(m.globals) > 0 {
		var gs [][]byte
		for _, g := range m.globals {
			e := []byte{I32, 0x01, 0x41}
			e = append(e, sleb(int64(g))...)
			e = append(e, 0x0b)
			gs = append(gs, e)
		}
		out = append(out, section(6, vec(gs))...)
	}

	var es [][]byte
	for _, e := range m.exports {
		x := append(name(e.name), e.kind)
		x = append(x, uleb(uint64(e.idx))...)
		es = append(es, x)
	}
	out = append(out, section(7, vec(es))...)

	var cs [][]byte
	for _, f := range m.funcs {
		var body []byte
		if len(f.locals) == 0 {
			body = uleb(0)
		} else {
			// one local entry per local keeps this trivial: (count=1, type) each
			body = uleb(uint64(len(f.locals)))
			for _, l := range f.locals {
				body = append(body, 0x01, l)
			}
		}
		body = append(body, f.body...)
		body = append(body, 0x0b)
		cs = append(cs, append(uleb(uint64(len(body))), body...))
	}
	out = append(out, section(10, vec(cs))...)

	if len(m.data) > 0 {
		var ds [][]byte
		for _, d := range m.data {
			e := []byte{0x00, 0x41}
			e = append(e, sleb(int64(d.offset))...)
			e = append(e, 0x0b)
			e = append(e, uleb(uint64(len(d.data)))...)
			e = append(e, d.data...)
			ds = append(ds, e)
		}
		out = append(out, section(11, vec(ds))...)
	}
	return out
}

// Code builds a function body. Methods return the receiver so calls can be chained.
type Code struct{ b []byte }

// C returns an empty Code builder.
func C() *Code { return &Code{} }

func (c *Code) op(b ...byte) *Code { c.b = append(c.b, b...); return c }

// I32Const pushes a constant.
func (c *Code) I32Const(v int32) *Code { return c.op(0x41).op(sleb(int64(v))...) }

// I64Const pushes a constant.
func (c *Code) I64Const(v int64) *Code { return c.op(0x42).op(sleb(v)...) }

// LocalGet pushes local i.
func (c *Code) LocalGet(i uint32) *Code { return c.op(0x20).op(uleb(uint64(i))...) }

// LocalSet pops into local i.
func (c *Code) LocalSet(i uint32) *Code { return c.op(0x21).op(uleb(uint64(i))...) }

// LocalTee stores the top of stack in local i, keeping it on the stack.
func (c *Code) LocalTee(i uint32) *Code { return c.op(0x22).op(uleb(uint64(i))...) }

// GlobalGet pushes global i.
func (c *Code) GlobalGet(i uint32) *Code { return c.op(0x23).op(uleb(uint64(i))...) }

// GlobalSet pops into global i.
func (c *Code) GlobalSet(i uint32) *Code { return c.op(0x24).op(uleb(uint64(i))...) }

// Call calls function idx.
func (c *Code) Call(idx uint32) *Code { return c.op(0x10).op(uleb(uint64(idx))...) }

// Drop pops one value.
func (c *Code) Drop() *Code { return c.op(0x1a) }

// Unreachable traps.
func (c *Code) Unreachable() *Code { return c.op(0x00) }

// Return returns from the function.
func (c *Code) Return() *Code { return c.op(0x0f) }

// Loop opens a loop block (no result); close with End.
func (c *Code) Loop() *Code { return c.op(0x03, 0x40) }

// Block opens a block (no result); close with End.
func (c *Code) Block() *Code { return c.op(0x02, 0x40) }

// If opens an if (no result); close with End.
func (c *Code) If() *Code { return c.op(0x04, 0x40) }

// Else starts the else arm.
func (c *Code) Else() *Code { return c.op(0x05) }

// End closes a block, loop or if.
func (c *Code) End() *Code { return c.op(0x0b) }

// Br branches to the label depth.
func (c *Code) Br(depth uint32) *Code { return c.op(0x0c).op(uleb(uint64(depth))...) }

// BrIf branches when the popped i32 is non-zero.
func (c *Code) BrIf(depth uint32) *Code { return c.op(0x0d).op(uleb(uint64(depth))...) }

// I32Load8U loads an unsigned byte from the address on the stack.
func (c *Code) I32Load8U(offset uint32) *Code { return c.op(0x2d, 0x00).op(uleb(uint64(offset))...) }

// I32Load loads a little-endian i32 (alignment 1, always valid).
func (c *Code) I32Load(offset uint32) *Code { return c.op(0x28, 0x00).op(uleb(uint64(offset))...) }

// I32Store stores [addr, value] (alignment 1).
func (c *Code) I32Store(offset uint32) *Code { return c.op(0x36, 0x00).op(uleb(uint64(offset))...) }

// I32Store8 stores the low byte of value at addr.
func (c *Code) I32Store8(offset uint32) *Code { return c.op(0x3a, 0x00).op(uleb(uint64(offset))...) }

// MemorySize pushes the memory size in pages.
func (c *Code) MemorySize() *Code { return c.op(0x3f, 0x00) }

// MemoryGrow pops a page delta and pushes the previous size or -1.
func (c *Code) MemoryGrow() *Code { return c.op(0x40, 0x00) }

// MemoryCopy pops [dst, src, n] and copies (bulk-memory).
func (c *Code) MemoryCopy() *Code { return c.op(0xfc, 0x0a, 0x00, 0x00) }

// Simple numeric ops.
func (c *Code) I32Add() *Code  { return c.op(0x6a) }
func (c *Code) I32Sub() *Code  { return c.op(0x6b) }
func (c *Code) I32Mul() *Code  { return c.op(0x6c) }
func (c *Code) I32And() *Code  { return c.op(0x71) }
func (c *Code) I32Shl() *Code  { return c.op(0x74) }
func (c *Code) I32ShrU() *Code { return c.op(0x76) }
func (c *Code) I32Eqz() *Code  { return c.op(0x45) }
func (c *Code) I32Eq() *Code   { return c.op(0x46) }
func (c *Code) I32Ne() *Code   { return c.op(0x47) }
func (c *Code) I32LtU() *Code  { return c.op(0x49) }
func (c *Code) I32GtU() *Code  { return c.op(0x4b) }
func (c *Code) I32GeU() *Code  { return c.op(0x4f) }

// I64 ops used to pack (ptr, len).
func (c *Code) I64ExtendI32U() *Code { return c.op(0xad) }
func (c *Code) I64Shl() *Code        { return c.op(0x86) }
func (c *Code) I64Or() *Code         { return c.op(0x84) }

// LE32 returns v as 4 little-endian bytes (for building Data segments).
func LE32(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}
