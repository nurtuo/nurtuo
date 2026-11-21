<!-- SPDX-License-Identifier: Apache-2.0 -->
# Documentation

Nurtuo is in early development. These pages describe what the code does today and mark what was
not verified or is not wired together yet. Verified means run in a development environment
without Java, .NET, Docker or a real PostgreSQL; those parts are marked as such.

## Guides

| Page | For |
|---|---|
| [getting-started.md](getting-started.md) | Development setup on Windows, macOS and Linux, with and without `make` |
| [concepts.md](concepts.md) | Flows, steps, ports, documents, connectors, queues, listeners, runtime, control plane, bundles |
| [flow-reference.md](flow-reference.md) | Every step kind and its config fields, with a validated example each (generated) |
| [flow-validation.md](flow-validation.md) | The validation rules, diagnostic codes, ports and the CommandBus |
| [connector-authoring.md](connector-authoring.md) | Writing a Go connector and running the contract test suite |
| [operations.md](operations.md) | Installing the runtime as a service, standalone mode, config file, data directory, logs, upgrade, backup, troubleshooting |
| [security.md](security.md) | Threat model sketch, mTLS, bundle signing, secrets, what is and is not verified |

## Connectors

[http](connectors/http.md), [sftp](connectors/sftp.md), [postgres](connectors/postgres.md),
[nats](connectors/nats.md): operations, connection settings, tracked properties, status mapping,
security notes and limits.

## Design

- [ARCHITECTURE.md](ARCHITECTURE.md): repository layout, build, CI, components and data flows
  (a proposal document; sections describing future components are not implemented)
- [perf.md](perf.md): the performance harness and measured results
- Architecture decision records in [adr/](adr/):
  [0001 monorepo license split](adr/0001-monorepo-license-split.md),
  [0002 edges as step:port map](adr/0002-edges-as-step-port-map.md),
  [0003 cycles are errors](adr/0003-cycles-are-errors.md),
  [0004 shared schema interpreter and corpus](adr/0004-shared-schema-interpreter-and-corpus.md),
  [0005 set-at-a-time engine](adr/0005-set-at-a-time-engine-with-per-document-status.md),
  [0006 bundle format and signature](adr/0006-bundle-format-and-domain-separated-signature.md),
  [0007 queue DLQ via advisory](adr/0007-queue-dlq-via-advisory.md),
  [0008 listener Source abstraction](adr/0008-listener-source-abstraction.md),
  [0009 outbound-only gateway](adr/0009-outbound-only-runtime-gateway.md),
  [0010 connections, secrets and connector registry](adr/0010-connections-and-secrets.md)

## Maintaining these docs

- `flow-reference.md` is generated: `node scripts/gen-flow-reference.mjs` rewrites it from
  `sdk/ts/flow-schema/schema/flow.v1.schema.json` and `examples/flows/reference/*.flow.json`;
  `--check` fails when it is stale and `--validate` re-validates every example (build
  `@nurtuo/flow-validate` first). The Go side checks the same examples in
  `sdk/go/flow/reference_examples_test.go`.
- `node scripts/check-docs-links.mjs` checks relative links and heading anchors in all Markdown files.
- `docs/` is Apache-2.0 (see [LICENSING.md](../LICENSING.md)); put new Markdown under an SPDX comment
  as these pages do.
