---
title: "Run golangci-lint locally, as CI does, before pushing Go changes"
packages: ["core", "sdk", "connectors"]
areas: ["ci", "testing"]
topics: ["golangci-lint", "errcheck", "staticcheck", "go-check"]
---
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Run golangci-lint locally, as CI does, before pushing Go changes

## Context
The required CI job `go` (`.github/workflows/ci.yml`) runs `go vet`, `go test -race`, a cross-compile and then `golangci/golangci-lint-action@v9` with `args: "./core/runtime/... ./sdk/go/... ./connectors/..."`. The same invocation is `make lint`'s first line. The repo has no `.golangci.yml`, so default linters apply (errcheck, staticcheck, govet, ineffassign, unused). `.mise.toml` installs `golangci-lint` via `"go:github.com/golangci/golangci-lint/v2/cmd/golangci-lint" = "latest"`; the CI action sets no `version`, so it also tracks the action's default. Neither is an exact pin. The binary is not on `PATH` in agent environments unless `mise install` (`make bootstrap`) was run.

## Problem
Three PRs in a row were pushed with the `go` check red because of golangci-lint, although `go vet` and `go test` passed locally. All three were in test files:

- [PR #51](https://github.com/nurtuo/nurtuo/pull/51): errcheck on `defer l.Close()` in `core/runtime/cmd/nurtuo/flowrun_sftp_test.go:457`.
- [PR #71](https://github.com/nurtuo/nurtuo/pull/71): errcheck on a bare `<-done` in `core/runtime/internal/runtime/placeholder_test.go:191`. The follow-up `_ = <-done` then failed staticcheck S1005.
- [PR #75](https://github.com/nurtuo/nurtuo/pull/75): errcheck in `core/runtime/internal/gateway/client_test.go:390`.

Agents ran only `go vet` and `go test`, because the linter was not installed.

## Rule
- Before pushing any Go change, you MUST run golangci-lint with the CI invocation and fix every finding: `golangci-lint run ./core/runtime/... ./sdk/go/... ./connectors/...` (or `make lint`, which also runs the TS and buf checks). Run it from the repo root, which holds `go.work`.
- Use the CI-equivalent version. If `golangci-lint` is not on `PATH`, run `mise install` (or `make bootstrap`) and then `mise exec -- golangci-lint run ./core/runtime/... ./sdk/go/... ./connectors/...`. Do not skip the lint because the tool is missing.
- Test files count: errcheck and staticcheck apply to `_test.go` too.
- Do not silence errcheck with `_ = <-ch`: staticcheck S1005 rejects it. Handle the value (use it, assert on it, or `t.Fatal` on an unexpected one). For `defer x.Close()`, wrap it: `defer func() { _ = x.Close() }()` or check the error in `t.Cleanup`.
- If you change Go code in a module outside the three globs above (for example `core/deploy/bundle`), run the linter there too, and say so in the PR.

## How to verify
`golangci-lint run ./core/runtime/... ./sdk/go/... ./connectors/...` exits 0, and the `go` check on the PR is green.
