<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: License boundaries

Status: implemented. Date: 2026-11-21.

## TLDR

`core/**` and root files are AGPL-3.0-only; `proto/`, `sdk/`, `connectors/`, `bridges/`, `cli/`,
`examples/` and `docs/` are Apache-2.0. `scripts/check-boundaries.sh` and the CI `licenses` job
enforce that Apache code never depends on AGPL code, that SPDX headers match the directory, and that
third-party dependencies are on the allow-list for their side.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/19 (squash commit `678663a`), https://github.com/nurtuo/nurtuo/pull/15 (`c69f869`) |
| ADRs | [ADR 0001](../../adr/0001-monorepo-license-split.md) |
| Code | `LICENSING.md`, `LICENSE`, `core/LICENSE`, `sdk/LICENSE`, `examples/LICENSE`, `scripts/check-boundaries.sh`, `.github/licenses/agpl-allow.txt`, `.github/licenses/apache-allow.txt`, `licenses` job in `.github/workflows/ci.yml` |
| User docs | [LICENSING.md](../../../LICENSING.md), [ARCHITECTURE.md §1](../../ARCHITECTURE.md) |

## Problem and goals

The product is AGPL so hosted forks must publish changes, while SDKs, contracts and connectors are
Apache so customers can write closed-source connectors. A single accidental import from `core/`
into `sdk/` or `cli/` would make an Apache artifact a derivative of AGPL code.

- Goals: machine-checked dependency direction; SPDX header on every source file; per-side
  allow-lists for third-party licenses; an SBOM per CI run; a CLA so code can move between sides.
- Non-goals: legal review of individual dependencies; license of user flows (LICENSING.md FAQ
  states they are user data).

## Design (as built)

### Map

The nearest `LICENSE` file decides (LICENSING.md table). Go module roots that sit below a boundary
directory keep a regular-file copy of the boundary license: `core/runtime/LICENSE` and
`core/deploy/bundle/LICENSE` copy `core/LICENSE`, `sdk/go/LICENSE` copies `sdk/LICENSE`,
`examples/connector-minimal/LICENSE` copies `examples/LICENSE` (#15: `go-licenses` stops searching
at the module root, and module zips drop symlinks).

### `scripts/check-boundaries.sh`

1. Go: `sdk/go` and `connectors` must not mention `github.com/nurtuo/nurtuo/core` in `*.go` or `go.mod`.
2. TypeScript: `sdk/ts/*/package.json` and `cli/package.json` must not depend on
   `@nurtuo/control-plane`, `@nurtuo/web`, `@nurtuo/mcp` or `@nurtuo/ai`.
3. No relative `from '../core/…'` import in `*.ts`/`*.tsx` under the Apache directories.
4. SPDX header in the first three lines of every git-tracked `*.go`, `*.ts`, `*.tsx`, `*.proto`
   file matches the directory (`*/gen/*` is skipped).
5. The four LICENSE copies above exist, are regular files and are byte-identical to their source.

### CI `licenses` job

Runs `check-boundaries.sh`, then `go-licenses@v2.0.1 check` for `core/runtime` against
`agpl-allow.txt` and for `sdk/go` and `connectors` against `apache-allow.txt`, then
`license-checker-rseidelsohn --production --excludePrivatePackages --onlyAllow` for `sdk/ts/*` and
`cli` (Apache list) and `core/control-plane`, `core/web`, `core/mcp`, `core/ai` (AGPL list), and
finally an SPDX JSON SBOM with `anchore/sbom-action@v0` uploaded as `sbom.spdx.json`.

### Allow-lists

`apache-allow.txt` has no GPL/AGPL entries; MPL-2.0, EPL-2.0 and LGPL-2.1-or-later only as
unmodified dependencies. `agpl-allow.txt` adds GPL-3.0-or-later, AGPL-3.0-only/-or-later, the
classifier id `AGPL-3.0` (#15, the name `go-licenses` v2.0.1 gives the AGPL text), `Python-2.0` and
`XZ` (public-domain `github.com/xi2/xz` via embedded-postgres).

### CLA

CLA Assistant (cla-assistant.io) posts the `license/cla` status on pull requests (CONTRIBUTING.md).

## Contracts

| Contract | Stability |
|---|---|
| Directory-to-license map in LICENSING.md | stable; a change needs an ADR (ADR 0001) |
| SPDX header rule (`SPDX-License-Identifier: <id>` in the first 3 lines) | stable |
| Allow-list file format (one SPDX id per line, `#` comments) | internal |

## Security

Not a security boundary. Supply-chain relevance: the license steps run `npx --yes
license-checker-rseidelsohn` without a version, so CI downloads whatever version is latest (gap 3).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `scripts/check-boundaries.sh` | rules 1–5 above | `make boundaries` or `bash scripts/check-boundaries.sh` | needs bash and `git ls-files` |
| `go-licenses check` steps | Go third-party licenses | see `licenses` job | needs `go install github.com/google/go-licenses/v2@v2.0.1` |
| `license-checker-rseidelsohn` steps | npm production dependencies | see `licenses` job | after `pnpm install --frozen-lockfile` |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Rule 1 checks only `sdk/go` and `connectors`. The workspace module `examples/connector-minimal` (in `go.work`) is Apache too; an import of `github.com/nurtuo/nurtuo/core/...` there resolves through `go.work` and is not caught. The same applies to any future Apache Go module. | medium | Go boundary | Loop over every `go.work` module under an Apache directory instead of a fixed list. |
| 2 | Rule 4 only checks `*.go`, `*.ts`, `*.tsx`, `*.proto`. Python (`bridges/python`), Java (`bridges/jvm`), C# (`bridges/dotnet`), `*.mjs` scripts and Markdown are unchecked; an AGPL header pasted into a bridge file, or a missing header, passes. | medium | SPDX | Extend the file list to `*.py`, `*.java`, `*.kt`, `*.cs`, `*.mjs`, `*.js` (excluding generated directories such as `_gen/`). |
| 3 | `npx --yes license-checker-rseidelsohn` is unpinned. A breaking or malicious release changes the result or runs arbitrary code in the job. | medium | supply chain | Add it as a pinned devDependency and run it from the lockfile. |
| 4 | Rule 2 greps package names in `package.json` only. An Apache package that imports an AGPL package through a TypeScript path alias or a transitive workspace dependency is not caught. | low | TS boundary | Also scan `import … from "@nurtuo/(control-plane|web|mcp|ai)"` in Apache sources. |
| 5 | `sdk/ts/client/scripts/gen-types.mjs` (Apache) generates `src/schema.generated.ts` from `core/control-plane/openapi.yaml` (AGPL). The generated types are published under Apache. This is interface data, but nothing documents that the OpenAPI document may be used this way. | low | licensing | State in LICENSING.md that `openapi.yaml` and `proto/` are interface definitions usable by Apache code, or move `openapi.yaml` to an Apache directory. |
| 6 | Bridge dependencies (Maven, NuGet, pip) are not license-checked at all; only Go and npm are. | medium | bridges | Add `pip-licenses`, a Gradle license report and `dotnet-project-licenses` steps against `apache-allow.txt`. |
| 7 | The SBOM is uploaded as a CI artifact (retention defaults) and is not tied to a release. | low | SBOM | Covered by the per-archive SBOMs in [release-and-packaging](2026-11-21-release-and-packaging.md). |

## Improvement opportunities

- An `enterprise-contribution-guard` style check that flags pull requests moving files across the
  license boundary, so a maintainer confirms CLA coverage.
- Run `check-boundaries.sh` as a pre-commit hook.

## References

- Open Mercato: `enterprise-contribution-guard` (workflow) and their `LICENSE` split.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
