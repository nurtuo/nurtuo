# Nurtuo

**Open-source, AI-native integration platform.** Build, test, deploy and run integrations
on a visual canvas or entirely through an AI agent (MCP), on a fast Go runtime that installs
as a single binary next to your systems, with or without Docker.

- **Canvas + AI, one model.** The canvas and the AI agent edit the same compact JSON flow
  through the same commands, validated by the same library.
- **Fast runtime.** One static Go binary, outbound-only hybrid agent (like a Boomi Atom),
  Windows service / systemd / launchd, standalone or distributed.
- **Deployable queues with push listeners.** Embedded NATS JetStream, no polling.
  Targets: platform overhead p99 < 10 ms, queue pickup p99 < 5 ms (verified by the benchmark harness).
- **Enterprise reach.** Native Go connectors plus optional bridges (JVM, Python, .NET) for SAP,
  JMS/IBM MQ, EDI/AS2 and more, over one gRPC connector contract.
- **Everything free.** SSO/SAML, SCIM, RBAC, clusters and HA included. No paid edition, no feature gating.

> Status: early development. Nothing here is production-ready yet.

## Licensing

Nurtuo is open source under two licenses, split by directory (see [LICENSING.md](LICENSING.md)):

| Directory | License | What |
|---|---|---|
| `core/` (and repository root files) | **AGPL-3.0** | Runtime and supervisor, control plane, MCP server, AI layer, canvas UI, deploy packaging |
| `proto/`, `sdk/`, `connectors/`, `bridges/`, `cli/`, `examples/`, `docs/` | **Apache-2.0** | Contracts, SDKs, flow schema and validator, connectors, bridges, CLI |

Running Nurtuo unmodified, on-premises or in your cloud, has no obligations beyond keeping
notices. If you offer a **modified** Nurtuo server to others over a network, AGPL-3.0 requires
you to publish your changes. Connectors and tools built on the Apache-2.0 SDKs can use any license.

Nurtuo is funded by the hosted Nurtuo Cloud and by implementation services.
"Nurtuo" is a trademark; forks must use a different name.

## Getting started (development)

```sh
make bootstrap   # pinned toolchain via mise, pnpm install, go work sync
make dev         # local stack: Postgres, control plane, canvas, runtime
```

Without `make` (PowerShell steps, required `protoc-gen-go` and `protoc-gen-go-grpc` versions, C
compiler for `-race`) see [docs/getting-started.md](docs/getting-started.md). Note that `nurtuo run`
is not implemented yet, so `make dev` does not work end to end.

## Documentation

Start at the [documentation index](docs/README.md):

- [Getting started](docs/getting-started.md), [concepts](docs/concepts.md),
  [flow reference](docs/flow-reference.md), [flow validation rules](docs/flow-validation.md)
- Connectors: [http](docs/connectors/http.md), [sftp](docs/connectors/sftp.md),
  [postgres](docs/connectors/postgres.md), [nats](docs/connectors/nats.md);
  [writing a connector](docs/connector-authoring.md)
- [Operations](docs/operations.md), [security](docs/security.md), [performance](docs/perf.md)
- [Architecture](docs/ARCHITECTURE.md) and [decision records](docs/adr/0001-monorepo-license-split.md)
- [CONTRIBUTING.md](CONTRIBUTING.md)

## Release snapshot

`make snapshot` builds the `nurtuo` runtime the way a release would and writes
the result to `dist/`. It runs GoReleaser in snapshot mode
(`goreleaser release --snapshot --clean --skip=sign`) for six targets — linux,
windows, and darwin, each on amd64 and arm64 — with `CGO_ENABLED=0`,
`-trimpath`, checksums, a Syft SPDX SBOM per archive, and deb/rpm packages.
Nothing is published.

Install [GoReleaser](https://goreleaser.com) v2 and [Syft](https://github.com/anchore/syft)
on your `PATH`, then:

```sh
make snapshot
```

`make bootstrap` installs GoReleaser through mise. Install Syft yourself, for
example:

```sh
curl -sSfL https://raw.githubusercontent.com/anchore/syft/v1.57.2/install.sh | sh -s -- -b "$HOME/.local/bin" v1.57.2
```

Snapshot mode leaves these for the tagged release (see
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) §7.1):

- cosign keyless signing (`make snapshot` passes `--skip=sign`)
- MSI (WiX) and macOS notarization
- `bun build --compile` of `nurtuo-control-plane`

GitHub Actions runs the same target in
[`.github/workflows/release-snapshot.yml`](.github/workflows/release-snapshot.yml)
on pull requests that touch the release config, every night, and on manual
dispatch. The workflow uploads `dist/`.
