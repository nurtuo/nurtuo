<!-- SPDX-License-Identifier: Apache-2.0 -->
# Getting started (development setup)

This page gets a development checkout building and testing on Windows, macOS and Linux, with
and without `make`. Nurtuo is early development: see [Status](#status-what-works-today) before
expecting an end-to-end product.

Commands marked "verified" were run on Windows 11 (PowerShell 5.1 and Git Bash, Go 1.27.4,
Node 24.14, pnpm 10.34.6, buf 1.75.0) while writing this page. Everything else is taken from the
`Makefile`, `.mise.toml` and CI and was not run on every OS.

## Prerequisites

| Tool | Version | Used for |
|---|---|---|
| Go | 1.27 (`.mise.toml`, `go.work`) | runtime, Go SDK, connectors |
| Node.js | 24 (`.mise.toml`); `package.json` allows `>=22` | TypeScript packages |
| pnpm | 10 (`packageManager` pins `pnpm@10.34.6`) | workspace install and scripts |
| buf | any recent v1.x (verified with 1.75.0) | proto lint and code generation |
| protoc-gen-go | **v1.36.12** | Go protobuf code generation |
| protoc-gen-go-grpc | **v1.6.2** | Go gRPC code generation |
| gcc (C compiler) | any | only for `go test -race` (needs `CGO_ENABLED=1`) |
| Git Bash or another POSIX shell | | `scripts/check-boundaries.sh`, `make dev` |
| golangci-lint, go-licenses, goreleaser | latest | optional: `make lint`, license scan, `make snapshot` |

The generated protobuf code in `sdk/go/gen` and `sdk/ts/proto/src/gen` is committed, so
you only need `buf` and the plugins if you change files under `proto/`. Use the plugin
versions above (`.mise.toml` pins them and `make tools` installs them): a different plugin
version can change the generated files. Verified: with `protoc-gen-go
v1.36.12` and `protoc-gen-go-grpc 1.6.2` on `PATH`, `buf lint` and `buf generate` leave the
working tree unchanged.

`CGO_ENABLED=0` is used for release builds (`.goreleaser.yaml`, `make build`, the CI
cross-compile step), so a C compiler is not needed to build the runtime. `go test -race` is
different: the race detector requires cgo. Verified on Windows: with `CGO_ENABLED=0` it fails
with `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`; with `CGO_ENABLED=1` and
`gcc` on `PATH` (WinLibs MinGW) it passes (verified on `sdk/go/flow`). On Linux install `build-essential`/`gcc`; on macOS
install the Xcode command line tools (`xcode-select --install`).

## Option A: mise and make (macOS, Linux, Windows with Git Bash)

[mise](https://mise.jdx.dev) installs the pinned toolchain from `.mise.toml`.

```sh
git clone https://github.com/nurtuo/nurtuo.git
cd nurtuo
make bootstrap     # mise install; pnpm install; go work sync
make build         # CGO_ENABLED=0 go build github.com/nurtuo/nurtuo/... ; pnpm turbo run build
make test          # go test -race ... ; pnpm turbo run test   (needs a C compiler)
make boundaries    # license boundary check (Apache code must not import core/)
```

Other targets: `make gen` (`buf lint` + `buf generate`), `make lint`, `make bench`,
`make snapshot` (local goreleaser build, no publish). Run `make -n <target>` to see what a
target would run.

`make bootstrap` installs `buf` and the Go tools through mise, but see the plugin version note
above.

On Windows, `make` needs GNU Make (for example `winget install ezwinports.make`) and its recipes
are POSIX shell, so run it from Git Bash.

## Option B: without make (PowerShell)

Everything `make` does is a plain command. Run from the repository root.

```powershell
# Refresh PATH after installing tools in the same session
$env:Path = [Environment]::GetEnvironmentVariable('Path','Machine') + ';' + [Environment]::GetEnvironmentVariable('Path','User')

# Toolchain: install Go, Node.js, pnpm and buf with any installer you like (winget, scoop, mise).
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
# %USERPROFILE%\go\bin must be on PATH

pnpm install --frozen-lockfile
go work sync

# build
$env:CGO_ENABLED = '0'
go build github.com/nurtuo/nurtuo/...
pnpm turbo run build

# test (race detector needs cgo and gcc on PATH)
$env:CGO_ENABLED = '1'
go test -race github.com/nurtuo/nurtuo/...
pnpm turbo run test

# generate protobuf code
buf lint
buf generate

# license boundary check (needs bash; Git for Windows provides it)
& "$env:ProgramFiles\Git\bin\bash.exe" ./scripts/check-boundaries.sh
```

Notes:

- A plain `go build ./...` from the root does not span workspace modules; use the
  `github.com/nurtuo/nurtuo/...` pattern from the root, or run `go build ./...` inside a module
  (`core/runtime`, `sdk/go`, `connectors`, `core/deploy/bundle`, `examples/connector-minimal`).
- The `pnpm` PowerShell shim may be blocked by the execution policy; run `pnpm.cmd` instead
  or `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`.
- The bundle package is also a workspace module. Its README says to run its tests with
  `GOWORK=off go test ./...` from `core/deploy/bundle`.

## Try it: validate a flow

The flow validator is the same library the canvas, the MCP server and `nurtuoctl` use. After
`pnpm install` and a build of the four TypeScript packages the CLI needs, validate an example
(verified):

```sh
pnpm --filter @nurtuo/flow-schema --filter @nurtuo/flow-validate --filter @nurtuo/client --filter @nurtuo/cli build
node cli/dist/index.js validate examples/flows/get-employee.flow.json
```

`nurtuoctl validate <flow.json>` prints one line per diagnostic (`severity<TAB>code<TAB>message`)
and exits 1 if there is any error, so a valid flow prints nothing and exits 0. (`nurtuoctl` is
the `bin` of `@nurtuo/cli`; in this checkout invoke it as `node cli/dist/index.js`.) The Go
validator in `sdk/go/flow` produces the same diagnostics; see [flow-validation.md](flow-validation.md).

## Try it: run a flow against a connection file

`nurtuo flow run` runs a flow file in-process, without a control plane. With an SFTP connection file
(see [cli.md](cli.md#nurtuo-flow-run-run-a-flow-file-locally-go-runtime-binary)) and the password in the
environment, this prints the bytes of `/downloads/version.txt`:

```sh
go build -o nurtuo ./core/runtime/cmd/nurtuo
export NURTUO_SECRET_SFTP_PASSWORD='...'
./nurtuo flow run examples/flows/sftp-download.flow.json \
  --connection examples/connections/sftp.connection.json \
  --set path=/downloads/version.txt
```

The SFTP host key is checked against `~/.ssh/known_hosts` (`--known-hosts <file>` to use another file).

## Try it: run a flow from the canvas (experimental)

With a control plane running, the canvas **Build** tab can run the flow you are editing against a
workspace connection and show the result and a colour per step. The sample
`examples/flows/sftp-rebex-demo.flow.json` reads a file from the public Rebex SFTP demo server.

```sh
# terminal 1: control plane (in-memory, no database), with the nurtuo binary it spawns per run
go build -o /tmp/nurtuo ./core/runtime/cmd/nurtuo    # from the repository root
pnpm --filter @nurtuo/control-plane... run build
NURTUO_AUTH_SECRET="$(openssl rand -hex 24)" NURTUO_BOOTSTRAP_EMAIL=dev@example.com \
NURTUO_BOOTSTRAP_PASSWORD="$DEV_CANVAS_PASSWORD" NURTUO_FLOW_RUN_BIN=/tmp/nurtuo \
  node core/control-plane/dist/main.js            # listens on :8080
# terminal 2: canvas (Vite proxies /v1 to :8080; use http://127.0.0.1:5173 so the Secure cookie is kept)
pnpm --filter @nurtuo/web dev
```

Set `DEV_CANVAS_PASSWORD` to a local password before starting the control plane. Log in, open **Connections**, create `rebex-demo` (host `test.rebex.net`, port 22, the user and
password from the Rebex demo page). Click **Fetch host key**, compare the fingerprint with the one
Rebex publishes, then click **Trust this key** and save. Load the sample flow, press **Run** and set
`path=/readme.txt`. Rebex is a manual smoke test. The
password is stored only as a workspace secret (in memory: gone after a control-plane restart) and
never appears in the flow. Test runs allow `sftp` `GET` and `QUERY` only and refuse private
addresses unless `NURTUO_TEST_RUN_ALLOW_PRIVATE=1`.

## Run the tests that matter for docs and contracts

`make docs` runs the last two checks below (verified with `make docs` on Windows). CI runs them
in the `ts` job (CI itself was not run).

```sh
pnpm --filter @nurtuo/flow-schema test                  # schema, commands, conformance corpus
pnpm --filter @nurtuo/flow-validate test
node scripts/gen-flow-reference.mjs --validate --check   # flow-reference.md is current, examples valid
node scripts/check-docs-links.mjs                        # markdown links in README and docs/
```

```powershell
go test github.com/nurtuo/nurtuo/sdk/go/...                   # flow validator, contracttest
go test github.com/nurtuo/nurtuo/examples/connector-minimal   # the example connector passes contracttest
```

## Running the runtime

Build the binary and look at the commands:

```powershell
$env:CGO_ENABLED = '0'
go build -o nurtuo.exe ./core/runtime/cmd/nurtuo
.\nurtuo.exe version
```

Verified output for a local build: `nurtuo dev (none, unknown)`. Release builds inject the
version through `-ldflags` (`.goreleaser.yaml`).

Commands: `run`, `flow run`, `standalone`, `service` and `secret` are implemented; `role` is a stub and prints
`not implemented yet` (exit 1). See [operations.md](operations.md).

## Status: what works today

- **Works and tested:** flow format and validators (TS and Go, shared conformance corpus),
  the engine (set-at-a-time interpreter), embedded NATS JetStream queue backend with DLQ, HTTP
  and queue listeners, the Go connector SDK and contract test suite, the four connectors
  (HTTP, SFTP, PostgreSQL, NATS) registered in the `nurtuo` binary, bundle connections with a
  secret store (`nurtuo secret`), signed bundles verified against `trusted_keys`, the WASM
  `script` step sandbox, the control plane REST API (in-memory or Postgres) with authentication
  and RBAC, the mTLS runtime gateway on both sides (cross-language interop tests), the MCP
  server tools including the opt-in `deploy` tool, the CLI, and the Python and .NET/JVM bridge
  SDKs with the Go launcher (the .NET and JVM bridges were not built or run here).
- **Not wired together yet:** the `nurtuo` binary has no `queue` or `notify` step runner, uses
  in-memory queues in the runtime process (embedded JetStream is not wired into it), cannot yet
  receive WASM script modules through bundles, and cannot configure bridge connectors from
  `runtime.yaml`. `nurtuo standalone` starts the control plane, gateway and runtime together but
  was not run end to end against a real embedded PostgreSQL. Details per area in
  [operations.md](operations.md), [security.md](security.md) and [flow-reference.md](flow-reference.md).

## Next

[concepts.md](concepts.md), [flow-reference.md](flow-reference.md),
[connector-authoring.md](connector-authoring.md), [CONTRIBUTING.md](../CONTRIBUTING.md).
