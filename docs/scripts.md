<!-- SPDX-License-Identifier: Apache-2.0 -->
# Script step (WASM)

The `script` step runs a WebAssembly module over every document. Modules run under
[wazero](https://wazero.io) (pure Go, no cgo) in a fresh instance per document, with hard limits
and no ambient authority. Implementation: `core/runtime/internal/engine/script/wasm` (sandbox) and
`core/runtime/internal/engine/script.go` (`ScriptRunner`).

## Flow step

```json
{ "id": "tidy", "kind": "script",
  "config": { "script": "normalize-names", "timeoutMs": 2000, "memoryMiB": 16, "capabilities": ["clock"] } }
```

| Field | Default | Meaning |
|---|---|---|
| `script` | required | name of a registered module, resolved through `engine.ScriptSource` |
| `timeoutMs` | 10000 | wall-clock limit per document (instantiate + alloc + process) |
| `memoryMiB` | 32 | cap on the guest's linear memory |
| `capabilities` | none | any of `clock`, `random` |

Port: `default`. Per document: zero outputs drops it, N outputs replace it (ids `<id>.<n>` when N
> 1, output props override input props). A failure fails only that document with code
`script.<code>`; the rest of the set is processed normally. A missing or invalid module fails the
step (like a map that does not compile).

| Code | Cause |
|---|---|
| `timeout` | time limit, or the caller's context ended |
| `trap` | `unreachable`, out-of-bounds access, divide by zero, stack overflow, ... |
| `memory_limit` | declared minimum memory above the cap, or the guest could not allocate the input within the cap |
| `capability_denied` | the guest called `clock_ms`/`random_get` without the grant |
| `script_error` | the guest called `nurtuo.error(msg)` |
| `guest_exit` | the guest called WASI `proc_exit` |
| `output_invalid`, `output_too_large` | `process` returned a bad pointer or malformed/oversized frame |
| `input` | input frame above the 64 MiB default |
| `invalid_module`, `abi`, `load` | module is not valid wasm / violates the ABI / not registered (step-level) |

## Sandbox guarantees

* No filesystem, environment, arguments, sockets or stdio: the module only gets
  `wasi_snapshot_preview1` with nothing mounted (so TinyGo/Rust WASI guests link) and the `nurtuo`
  host module below. Any other import makes the module invalid.
* Clock and randomness are deterministic/absent unless granted (`clock`, `random`).
* Time is limited with a context deadline and wazero's `WithCloseOnContextDone`, which interrupts
  even a tight loop (wazero has no instruction "fuel"; this is the CPU bound). Memory is limited by
  `WithMemoryLimitPages` (a guest `memory.grow` past the cap returns -1).
* One instance per document; nothing survives between documents and concurrent calls share no
  state. Compiled code is cached by content hash and memory limit (`CompileOptions`).
* Guest log calls are rate limited (100 per document) and truncated (4 KiB).

## ABI v1

Guest exports: `memory`, `alloc(size i32) -> i32` (0 = failure) and
`process(ptr i32, len i32) -> i64`; optionally `_initialize` (reactor start, called once per
instance). The host calls `alloc(n)`, writes the input frame, calls `process`, and reads the
output frame at `(result >> 32, result & 0xffffffff)`. A result of 0 means "no documents".

Frame (little-endian u32 lengths), used for input (exactly one document) and output (any number):

```
frame    := count | document*
document := propsLen | props JSON object (string -> string) | payloadLen | payload
```

Host imports, module `nurtuo` (declare only what you use):

| Import | Signature | Notes |
|---|---|---|
| `log` | `(level i32, ptr i32, len i32)` | 0 debug, 1 info, 2 warn, 3 error |
| `error` | `(ptr i32, len i32)` | fail this document; return from `process` afterwards |
| `clock_ms` | `() -> i64` | Unix ms, needs `clock` |
| `random_get` | `(ptr i32, len i32)` | needs `random` |

## Guest SDK sketches

These are sketches of the shape of a guest; they were not compiled in this repository (no guest
toolchain is installed in CI yet). The sandbox tests use hand-assembled modules instead (below).
Each guest needs a bump allocator (the instance is discarded after one call, so never free), a
frame parser and a frame writer.

### TinyGo

```go
package main

import ("encoding/binary"; "unsafe")

//go:wasmimport nurtuo log
func hostLog(level, ptr, n uint32)

var heap [][]byte // keep allocations alive for the GC

//export alloc
func alloc(n uint32) uint32 {
	b := make([]byte, n); heap = append(heap, b)
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

//export process
func process(ptr, n uint32) uint64 {
	in := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), n)
	propsLen := binary.LittleEndian.Uint32(in[4:])
	props := in[8 : 8+propsLen]
	payload := in[8+propsLen+4:]
	// ... transform payload ...
	out := make([]byte, 0, 4+4+len(props)+4+len(payload))
	out = binary.LittleEndian.AppendUint32(out, 1)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(props)))
	out = append(out, props...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	heap = append(heap, out)
	return uint64(uintptr(unsafe.Pointer(&out[0])))<<32 | uint64(len(out))
}

func main() {}
```

Build as a WASI reactor, e.g. `tinygo build -o script.wasm -target=wasi -buildmode=c-shared .`
(reactor export of `_initialize`; check your TinyGo version's flag).

### Rust

```rust
#[no_mangle] pub extern "C" fn alloc(n: usize) -> *mut u8 {
    let mut v = Vec::<u8>::with_capacity(n); let p = v.as_mut_ptr(); std::mem::forget(v); p
}
#[no_mangle] pub extern "C" fn process(ptr: *const u8, n: usize) -> u64 {
    let input = unsafe { std::slice::from_raw_parts(ptr, n) };
    let out: Vec<u8> = transform(input); // parse frame, build output frame
    let p = out.as_ptr() as u64; let l = out.len() as u64; std::mem::forget(out);
    (p << 32) | l
}
```

`cargo build --target wasm32-wasip1 --release` with `crate-type = ["cdylib"]`. Imports are
declared with `#[link(wasm_import_module = "nurtuo")] extern "C" { fn log(level: i32, ptr: *const u8, len: i32); }`.

### AssemblyScript

```ts
@external("nurtuo", "log") declare function hostLog(level: i32, ptr: usize, len: i32): void;
export function alloc(n: i32): usize { return heap.alloc(n); }
export function process(ptr: usize, n: i32): i64 {
  // parse frame at ptr, build output frame in a new ArrayBuffer
  const out = new ArrayBuffer(n);
  memory.copy(changetype<usize>(out), ptr, n); // identity transform
  return (<i64>changetype<usize>(out) << 32) | <i64>n;
}
```

Compile with `asc script.ts -o script.wasm --runtime stub --exportRuntime` (the stub runtime never
frees, which suits one instance per call).

## Test modules

No guest toolchain exists in the build environment, so
`script/wasm/wasmtest` contains a minimal WebAssembly binary assembler (`asm.go`: type, import,
function, memory, global, export, data and code sections plus the few opcodes needed) and
hand-assembled guests in `guests.go` (echo, uppercase, fan-out, drop, trap, infinite loop, bad
output, host `log`/`error`/`clock_ms`/`random_get` callers). Modules are produced at test time, so
no opaque `.wasm` bytes are checked in. To add a guest, write it with `wasmtest.New()` and
`wasmtest.C()` and append a test next to the others.

## Performance (i5-14400F, Windows, `go test -bench`, indicative)

| Benchmark | Result |
|---|---|
| Compile, cold (tiny module) | ~0.4 ms |
| Compile, cached | ~2 us |
| Run, 16 B payload (instantiate + alloc + process + decode) | ~30-45 us, ~98 KB / 60 allocs |
| Run, 1 MiB payload (toy byte-loop guest) | ~25 ms |
| Deadline machinery overhead (`WithCloseOnContextDone` + timeout ctx) | ~10% on a trivial call |

Fresh-instance-per-document costs about 30 us and one memory page of allocation per call; parallel
scaling was only modest in a quick run, so an opt-in instance pool is the first optimisation to
try (at the price of cross-document guest state) if script steps become hot.
