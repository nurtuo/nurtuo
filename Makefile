# SPDX-License-Identifier: AGPL-3.0-only
GO_PKGS := github.com/nurtuo/nurtuo/...

.PHONY: bootstrap tools gen build test lint bench dev boundaries licenses snapshot docs

bootstrap:            ## install pinned toolchain and dependencies
	mise install
	pnpm install
	go work sync

tools:                ## install the pinned protobuf plugins (versions must match .mise.toml)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2

gen:                  ## regenerate Go + TS code from proto/
	buf lint
	buf generate

build:
	CGO_ENABLED=0 go build $(GO_PKGS)
	pnpm turbo run build

test:
	go test -race $(GO_PKGS)
	pnpm turbo run test

lint:
	golangci-lint run ./core/runtime/... ./sdk/go/... ./connectors/...
	pnpm turbo run lint typecheck
	buf lint

bench:                ## perf targets: pickup p99 < 5 ms, overhead p99 < 10 ms
	go test -run '^$$' -bench . -benchtime 10000x ./core/runtime/test/bench/...

boundaries:
	./scripts/check-boundaries.sh

docs:                 ## docs/flow-reference.md is current, its examples are valid, links resolve
	node scripts/gen-flow-reference.mjs --validate --check
	node scripts/check-docs-links.mjs

dev:                  ## canvas (turbo dev) + standalone runtime with embedded Postgres and control plane
	pnpm turbo run dev & go run ./core/runtime/cmd/nurtuo standalone; wait

snapshot:             ## local release build (no publish)
	goreleaser release --snapshot --clean
