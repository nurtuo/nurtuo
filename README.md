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

See [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
