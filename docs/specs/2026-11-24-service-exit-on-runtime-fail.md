<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Exit the OS service when the runtime fails

Status: approved (2026-11-24; closes an already-documented gap, no new decisions). Date: 2026-11-24.

Extends: [runtime-cli-service-standalone](implemented/2026-11-21-runtime-cli-service-standalone.md) (gap 3).

## TLDR

When `run` (`runtime.RunWith`) returns an error inside `program.Start`'s goroutine, the service logs it and exits the process with code 1, so systemd (`Restart=on-failure`) and Windows (`OnFailure=restart`) see a failed service and restart it. Before, the process stayed alive and looked healthy.

## Status and links

| | |
|---|---|
| Pull requests | implementation PR for #59 |
| Issues | https://github.com/nurtuo/nurtuo/issues/59 |
| Code | `core/runtime/cmd/nurtuo/service.go` (`program`, `runService`), `core/runtime/cmd/nurtuo/service_test.go` |

## Problem and goals

Example: the client key is unreadable, `RunWith` returns an error, the goroutine logs it, `svc.Run` keeps blocking, and the OS never restarts the service.

- Goals: a `run` error that occurs on its own terminates the process non-zero.
- Non-goals: changing installers, unit files, Windows manifests, CLI flags, config or protos; retry or backoff logic (the OS restart policy owns it); changing `standalone`.

## Design

`program` gets an `exit func(code int)` field (nil is a no-op, for tests). In the `Start` goroutine, when `run` returns an error: call `log(err)`, then, only if the run context was not cancelled (the error is not part of a `Stop`), call `exit(1)`. `runService` sets `exit = os.Exit`. `svc.Stop` is not used: a clean stop through the service manager is not a failure and would not trigger a restart.

Scope: any `run` error, at start or later, because `RunWith` returns only when it cannot continue. A nil return, or an error after `Stop`, does not exit.

## Contracts

None changed. Exit code 1 on runtime failure is internal behavior.

## Acceptance criteria

- A failing `prg.run` calls `exit` with a non-zero code.
- A clean stop, or an error returned after `Stop` cancelled the context, does not call `exit`.

## Tests

`TestProgramExitsOnRunFailure` and `TestProgramNoExitOnCleanStopOrStopError` in `service_test.go`; `go vet` and `go test -race` in `core/runtime`.
