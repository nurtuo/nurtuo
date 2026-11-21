# SPDX-License-Identifier: AGPL-3.0-only
GO_PKGS := github.com/nurtuo/nurtuo/...

.PHONY: bootstrap gen build test lint bench dev boundaries licenses snapshot

bootstrap:            ## install pinned toolchain and dependencies
	mise install
	pnpm install
	go work sync

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

dev:                  ## one command: dev services + control plane + canvas + runtime
	docker compose -f core/deploy/dev/compose.yaml up -d postgres
	pnpm turbo run dev & go run ./core/runtime/cmd/nurtuo run --dev; wait

snapshot:             ## local release build (no publish)
	goreleaser release --snapshot --clean
