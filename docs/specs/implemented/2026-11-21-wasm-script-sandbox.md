<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: WASM script sandbox and `script` step

Status: implemented (written as built). Date: 2026-11-21.

## TLDR

The `script` step runs an untrusted WebAssembly module per document under wazero (pure Go), with
no filesystem, environment, arguments or sockets, a 32 MiB memory cap and a 10 s timeout by
default, and opt-in `clock` and `random` capabilities. Failures are isolated per document. On a
deployed runtime every script step fails today, because bundles cannot carry modules and the
runtime registers an empty module registry.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/23 (squash commit `54ca076`); runtime registration in https://github.com/nurtuo/nurtuo/pull/25 (`ee4d733`) |
| ADRs | — |
| Issues | — |
| Code | `core/runtime/internal/engine/script/wasm/` (`engine.go`, `abi.go`, `host.go`, `errors.go`, test guests in `wasmtest/`), `core/runtime/internal/engine/script.go` (`ScriptRunner`, `ScriptModules`) |
| User docs | [flow-reference.md](../../flow-reference.md#script), [scripts.md](../../scripts.md), [security.md](../../security.md) |

## Problem and goals

Some transformations do not fit JSONata (binary formats, custom parsing, existing libraries in
Rust or TinyGo). Users must be able to ship code that runs inside the runtime without being able to
read files, reach the network, see secrets or take the process down.

- Goals: no ambient authority; hard memory and time limits that interrupt tight loops; per-document
  isolation (fresh instance per invocation); a small, language-neutral ABI; compile caching.
- Non-goals (as built): module delivery and signing (see the draft
  [bundle-artifact-delivery](../2026-11-22-bundle-artifact-delivery.md)); host calls to connectors,
  secrets or HTTP; WASI preview 2 / component model.

## Design (as built)

**Engine** (`engine.go`). `wasm.NewEngine(EngineOptions{Now, Rand, Logger})` owns wazero runtimes
(one per memory limit) and a shared `wazero.CompilationCache`. `Engine.Compile(ctx, bytes,
CompileOptions{MemoryBytes})` validates the module against the ABI and returns a cached `*Module`
for the same bytes and limit (`TestCompileCache`). Runtimes are created with
`WithCloseOnContextDone(true)` so a deadline interrupts a guest even inside a loop, and
`WithMemoryLimitPages`. Defaults: `DefaultMemoryBytes` 32 MiB, `DefaultTimeout` 10 s,
`DefaultMaxInput` and `DefaultMaxOutput` 64 MiB, `DefaultMaxLogs` 100 per invocation.

**ABI v1** (`abi.go`). The guest exports `memory`, `alloc(size i32) -> i32` and
`process(ptr i32, len i32) -> i64` (returns `outPtr<<32 | outLen`; 0 means no output), optionally
`_initialize`. Frames are `count | (propsLen | propsJSON | payloadLen | payload)*count`,
little-endian u32. Input is exactly one document; output may be zero (drop) or many documents,
each inheriting the input props with its own overriding them. `DecodeFrame` rejects counts larger
than the frame and trailing bytes.

**Host imports** (`host.go`, module `nurtuo`, only bound if declared): `log(level, ptr, len)`,
`error(ptr, len)` (fails the document with `script_error`), `clock_ms()` (capability `clock`),
`random_get(ptr, len)` (capability `random`). Using an ungranted capability traps with
`capability_denied`. `wasi_snapshot_preview1` is provided with no filesystem, environment,
arguments or sockets and discarded stdio; its clock and randomness are fixed or absent unless the
capability is granted.

**Errors** (`errors.go`). `*wasm.Error{Code, Message, Cause}` with codes `timeout`, `trap`,
`memory_limit`, `capability_denied`, `script_error`, `guest_exit`, `input`, `output_invalid`,
`output_too_large`, `invalid_module`, `abi`, `internal`.

**Step runner** (`script.go`). `ScriptRunner{Engine, Source ScriptSource, Concurrency}`, installed
with `Registry.RegisterScriptRunner`. Per step execution: check capabilities (`clock`, `random`;
anything else fails the step with `script.capability_unknown`), `Source.LoadScript(name)`
(`script.load`), `Engine.Compile` with `memoryMiB`, then run every OK document in its own instance
with up to `GOMAXPROCS` workers, `timeoutMs` as the timeout. Zero outputs drop the document; one
output keeps the id; several get ids `<id>.<n>`. Any failure fails only that document with
`script.<code>`; a panic becomes `script.internal`. `ScriptModules` is the in-memory
`ScriptSource`.

**Runtime.** `RunWith` registers `NewScriptRunner(wasm.NewEngine(...), engine.NewScriptModules())`
with nothing registered in it (`core/runtime/internal/runtime/run.go`), so every deployed `script`
step fails with `script.load` ("script \"x\" is not registered").

## Contracts

| Contract | Stability |
|---|---|
| Flow config `script`, `timeoutMs` (1..600000), `memoryMiB` (1..4096), `capabilities` (`clock`, `random`) | stable (schema) |
| Guest ABI v1 (exports, frame format, `nurtuo` host imports) | experimental: the ABI version is not negotiated or declared by the guest |
| Error codes `script.<code>` on failed documents | experimental |
| Go API `wasm.Engine`, `Module`, `CompileOptions`, `RunOptions`, `ScriptRunner`, `ScriptSource` | internal |

## Security

- No ambient authority: no filesystem, environment, args, sockets; stdio discarded. Real clock and
  randomness only by explicit capability.
- Fresh instance per document; no state survives between documents or flows.
- Memory is capped per instance; time is capped per invocation and enforced by context
  cancellation inside loops (`TestInfiniteLoopTimesOut`).
- Guest-controlled sizes are checked before allocation (`DecodeFrame`, `MaxInput`, `MaxOutput`).
- wazero runs without cgo, so a guest bug cannot corrupt host memory short of a wazero bug.
- Limits are author-controlled up to the schema maximum (4 GiB, 10 min) with no runtime-wide
  ceiling (gap 2).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestFrameRoundTrip`, `TestDecodeFrameRejectsGarbage` (`script/wasm/engine_test.go`) | ABI frames | `cd core/runtime && GOWORK=off go test -race ./internal/engine/script/...` | guests are hand-assembled in `wasmtest/asm.go`, no toolchain needed |
| `TestEchoSuccess`, `TestUpperTransformsPayload`, `TestFanOutAndDrop`, `TestLargePayload` | data path | same | |
| `TestTrap`, `TestGuestReportedError`, `TestInfiniteLoopTimesOut`, `TestCallerCancellation`, `TestMemoryLimit`, `TestInputAndOutputLimits`, `TestCapabilities`, `TestInvalidAndABIViolations` | limits and isolation | same | |
| `TestCompileCache`, `TestGuestLogging`, `TestConcurrentCalls` | cache, logs cap, concurrency | same | |
| `TestScriptStep*` (`engine/script_test.go`) | step semantics, per-document isolation, error codes, capability grant, fan-out, config errors, 300 concurrent documents, large payload spill, schema validation | `cd core/runtime && GOWORK=off go test -race ./internal/engine/...` | `BenchmarkScriptStep`; sandbox benchmarks in `script/wasm/bench_test.go` |

No test runs a module built by a real TinyGo or Rust toolchain.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Modules cannot be delivered: bundles carry only `flow` and `connection` components, and `RunWith` registers an empty `ScriptModules`. Every `script` step on a deployed runtime fails all documents with `script.load`. | high | runtime, bundles | draft [bundle-artifact-delivery](../2026-11-22-bundle-artifact-delivery.md) |
| 2 | A flow author sets `memoryMiB: 4096` and `timeoutMs: 600000`; the runner starts up to `GOMAXPROCS` instances at once (16 on a 16-core host), so one step can reserve up to 64 GiB of guest memory and hold workers for 10 minutes. There is no runtime-wide ceiling. | high | sandbox, runtime config | runtime config `script.max_memory_mib`, `script.max_timeout_ms`, and a global instance semaphore |
| 3 | The ABI version is implicit. A future ABI v2 cannot tell a v1 guest from a v2 guest and would misread frames. | medium | ABI | require an exported `nurtuo_abi_version` global or function before ABI v2 |
| 4 | Guest `log` calls are discarded on a deployed runtime: `RunWith` creates `wasm.NewEngine(wasm.EngineOptions{})` with a nil `Logger`. A script author debugging a failing step sees only the `script.<code>` error. When a logger is wired, 100 messages of up to 4,096 bytes (`maxLogBytes`) per document may carry payload data into logs. | medium | runtime wiring, host | pass the runtime logger (through the secret redactor) and add the flow id to each record |
| 5 | Only hand-assembled test guests are exercised; TinyGo and Rust WASI guests (the documented use case in [scripts.md](../../scripts.md)) were never built or run here, so their WASI imports may hit `invalid_module`. | medium | tests | add a CI job that builds and runs one TinyGo and one Rust example guest |
| 6 | Module identity is a name only (`script` string). Two deployments with different modules under the same name cannot coexist once delivery exists. | medium | design | address by content digest in the artifact delivery draft |

## Improvement opportunities

- Precompile modules at deploy time and fail the deployment on `invalid_module` or `abi` instead of
  on the first document.
- Expose per-step sandbox metrics (instances, timeouts, memory-limit hits).

## References

- Open Mercato: no spec with a similar scope; their `security-checklist.md` style applies to the
  sandbox review.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
