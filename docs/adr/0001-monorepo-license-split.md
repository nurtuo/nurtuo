# ADR 0001: One monorepo, license split by top-level directory

Date: 2026-11-21. Status: accepted.

**Context.** Nurtuo ships AGPL-3.0 server code and Apache-2.0 SDKs, connectors, bridges, CLI and
contracts. Contracts (proto, flow schema) change together with their Go and TS consumers.

**Decision.** One repository `nurtuo/nurtuo`. AGPL code lives under `core/`; Apache-2.0 code lives
in `proto/`, `sdk/`, `connectors/`, `bridges/`, `cli/`, `examples/`, `docs/`, each with a `LICENSE`.
Apache code never depends on `core/`; CI enforces this plus SPDX headers and third-party allow-lists.

**Consequences.** Atomic cross-language changes and one CI. Any Apache directory can later be
extracted to its own repository without refactoring. Go uses three modules in a `go.work`.
