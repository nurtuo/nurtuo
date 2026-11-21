// SPDX-License-Identifier: AGPL-3.0-only

// Package wasm is the Nurtuo script sandbox: it runs untrusted WebAssembly guests under wazero
// (pure Go, no cgo) with hard limits and no ambient authority.
//
// # Guest ABI (version 1)
//
// A guest module must export:
//
//	memory                          linear memory
//	alloc(size i32) -> ptr i32      returns the address of size writable bytes, or 0 on failure
//	process(ptr i32, len i32) -> i64
//
// The host calls alloc(n), copies an input frame into memory at the returned address, then calls
// process(ptr, len). process returns (outPtr << 32) | outLen locating an output frame in guest
// memory; a result of 0 (or outLen == 0) means "no output documents" (the document is dropped).
// Optionally the module may export `_initialize` (WASI reactor style), which is called once per
// instance before alloc.
//
// Frames use the same encoding in both directions, all integers little-endian u32:
//
//	frame    := count | document*count
//	document := propsLen | props | payloadLen | payload
//
// props is a JSON object whose values are strings (document properties); payload is the raw body.
// The input frame always contains exactly one document. An output frame may contain any number of
// documents; each inherits the input document's props, overridden by its own.
//
// # Host imports (module "nurtuo", all optional; imported only if the guest declares them)
//
//	log(level i32, ptr i32, len i32)    level 0 debug, 1 info, 2 warn, 3 error; message is UTF-8
//	error(ptr i32, len i32)             fail this document with the given message (script_error);
//	                                    the guest should return afterwards
//	clock_ms() -> i64                   Unix milliseconds; capability "clock"
//	random_get(ptr i32, len i32)        fill memory with random bytes; capability "random"
//
// A guest that calls a capability it was not granted traps and its document fails with
// CodeCapabilityDenied. wasi_snapshot_preview1 is provided with no filesystem, no environment, no
// arguments, no sockets and discarded stdio, so TinyGo/Rust guests built for WASI link and run; its
// clock and random sources are a fixed deterministic value / absent unless the matching
// capability is granted.
package wasm

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Document is one document as seen by a guest: string properties and a raw payload.
type Document struct {
	Props   map[string]string
	Payload []byte
}

// ErrFrame is wrapped by all frame decoding errors.
var ErrFrame = errors.New("wasm: malformed frame")

// EncodeFrame serialises docs in ABI frame format.
func EncodeFrame(docs []Document) ([]byte, error) {
	size := 4
	props := make([][]byte, len(docs))
	for i, d := range docs {
		p := d.Props
		if p == nil {
			p = map[string]string{}
		}
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		props[i] = b
		size += 8 + len(b) + len(d.Payload)
	}
	out := make([]byte, 0, size)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(docs)))
	for i, d := range docs {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(props[i])))
		out = append(out, props[i]...)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(d.Payload)))
		out = append(out, d.Payload...)
	}
	return out, nil
}

// DecodeFrame parses an ABI frame. Returned payloads are copies and do not alias b.
func DecodeFrame(b []byte) ([]Document, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrame, len(b))
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	// every document needs at least 8 bytes; reject absurd counts before allocating
	if uint64(n)*8 > uint64(len(b)) {
		return nil, fmt.Errorf("%w: document count %d exceeds frame size", ErrFrame, n)
	}
	docs := make([]Document, 0, n)
	for i := uint32(0); i < n; i++ {
		pl, rest, err := lenPrefixed(b)
		if err != nil {
			return nil, fmt.Errorf("%w: document %d props: %v", ErrFrame, i, err)
		}
		var props map[string]string
		if len(pl) > 0 {
			if err := json.Unmarshal(pl, &props); err != nil {
				return nil, fmt.Errorf("%w: document %d props: %v", ErrFrame, i, err)
			}
		}
		pay, rest, err := lenPrefixed(rest)
		if err != nil {
			return nil, fmt.Errorf("%w: document %d payload: %v", ErrFrame, i, err)
		}
		docs = append(docs, Document{Props: props, Payload: append([]byte(nil), pay...)})
		b = rest
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrFrame, len(b))
	}
	return docs, nil
}

func lenPrefixed(b []byte) (item, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errors.New("truncated length")
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if uint64(n) > uint64(len(b)) {
		return nil, nil, fmt.Errorf("length %d exceeds remaining %d bytes", n, len(b))
	}
	return b[:n], b[n:], nil
}
