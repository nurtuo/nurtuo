<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Release and packaging (snapshot dry-run)

Status: implemented. Date: 2026-11-21.

## TLDR

`make snapshot` runs GoReleaser in snapshot mode and builds the `nurtuo` runtime for six targets
with checksums, one Syft SPDX SBOM per archive and deb/rpm packages. The `release-snapshot` workflow
runs it on relevant pull requests, nightly and on demand, and asserts the artifact set. Nothing is
signed or published: there is no tagged release workflow yet.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/16 (squash commit `3d3d506`) |
| ADRs | — |
| Issues | https://github.com/nurtuo/nurtuo/issues/11 |
| Code | `.goreleaser.yaml`, `Makefile` (`snapshot`), `.github/workflows/release-snapshot.yml`, `core/runtime/internal/version`, `core/deploy/systemd/nurtuo.service` |
| User docs | [ARCHITECTURE.md §7.1](../../ARCHITECTURE.md), [SECURITY.md](../../../SECURITY.md) "Release integrity" |

## Problem and goals

Release tooling that is only exercised on the day of the first tag will fail on that day.

- Goals: a reproducible local and CI dry-run of the runtime release; static, trimmed binaries with
  injected version metadata; checksums and SBOMs; Linux packages with the systemd unit.
- Non-goals (all TODO(§7.1), see the draft [release-signing-and-installers](../2026-11-22-release-signing-and-installers.md)):
  cosign signing, MSI, macOS notarization, the `nurtuo-control-plane` executable, bridges, npm
  packages, container images, publishing a GitHub release.

## Design (as built)

### `.goreleaser.yaml` (version 2, `project_name: nurtuo`)

- `snapshot.version_template: "{{ .Version }}-SNAPSHOT-{{ .ShortCommit }}"` (SemVer-shaped so nfpm accepts it).
- One build `nurtuo`: `dir: core/runtime`, `main: ./cmd/nurtuo`, `CGO_ENABLED=0`,
  `goos: [linux, windows, darwin]`, `goarch: [amd64, arm64]`, `-trimpath`, `ldflags -s -w` and
  `-X …/internal/version.{Version,Commit,Date}`.
- Archives: `tar.gz`, `zip` on Windows, with `LICENSE`, `LICENSING.md`, `README.md`.
- `checksum.name_template: checksums.txt`.
- `sboms`: Syft, `artifacts: archive`, document `{{ .ArtifactName }}.spdx.json`.
- `signs`: `cosign sign-blob --bundle=${artifact}.sigstore.json` over the checksum file
  (skipped by `--skip=sign`).
- `nfpms`: `deb` and `rpm`, `license: AGPL-3.0-only`, maintainer `contact@nurtuo.com`, installs
  `core/deploy/systemd/nurtuo.service` to `/lib/systemd/system/nurtuo.service`.
- `release.draft: true` for `nurtuo/nurtuo` (unused in snapshot mode).

### `make snapshot`

`goreleaser release --snapshot --clean --skip=sign` into `dist/`. Needs GoReleaser v2 and Syft on
`PATH`.

### Workflow `release-snapshot.yml`

Triggers: `pull_request` touching `.goreleaser*`, `Makefile`, `core/runtime/**` or the workflow;
`schedule` `0 4 * * *`; `workflow_dispatch`. `permissions: contents: read`, `timeout-minutes: 30`,
actions pinned by commit SHA, `persist-credentials: false`. Installs GoReleaser `v2.20.3` and Syft
`1.57.2` (tarball verified against a pinned sha256 per architecture), runs `make snapshot`, then the
step "Check snapshot artifacts" asserts: `dist/checksums.txt` exists; exactly 6 archives, 6 SBOMs,
2 debs, 2 rpms; one archive per OS/arch; the SBOM contains `"spdxVersion"`; the linux/amd64 binary
is `statically linked` and `go version -m` shows `CGO_ENABLED=0` and `-trimpath=true`. `dist/` is
uploaded as `nurtuo-snapshot-<sha>` for 7 days.

## Contracts

| Contract | Stability |
|---|---|
| Archive names `nurtuo_<version>_<os>_<arch>.tar.gz|zip`, `checksums.txt`, `*.spdx.json` | experimental (no release yet) |
| Package name `nurtuo`, unit path `/lib/systemd/system/nurtuo.service` | experimental |
| `internal/version` variables `Version`, `Commit`, `Date` (defaults `dev`, `none`, `unknown`) | internal |

## Security

- Binaries are static and built with `-trimpath`; build info is checked in CI.
- Syft download is checksum-verified; actions are SHA-pinned.
- No signature is produced, so nothing proves an archive came from this repository (gap 1).

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `snapshot` job, step "Check snapshot artifacts" | artifact counts, names, SBOM format, static linking, trimpath | `make snapshot` then the shell from the workflow | GoReleaser v2.20.3 and Syft 1.57.2 |
| cross-compile loop in the `go` job of `ci.yml` | all 6 targets compile | CI | duplicates the snapshot build on every pull request |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | SECURITY.md says release binaries "are signed with Sigstore cosign (keyless)" and tells users to run `cosign verify-blob`, but the only release path is `--skip=sign` and there is no tag workflow. A user who downloads a future hand-made release cannot verify it, or verifies nothing because no `.sigstore.json` exists. | high | release integrity | [release-signing-and-installers](../2026-11-22-release-signing-and-installers.md); until then reword SECURITY.md to "will be signed". |
| 2 | There is no workflow triggered by a `v*` tag. Cutting `v0.1.0` today means running GoReleaser by hand on a laptop with a personal token. | high | release | Tag workflow in the draft spec above. |
| 3 | The deb/rpm installs the systemd unit (`User=nurtuo`, `ExecStart=/usr/bin/nurtuo run --config /etc/nurtuo/runtime.yaml`) but the nfpm config has no `scripts`, creates no `nurtuo` user and ships no `/etc/nurtuo/runtime.yaml`. `apt install ./nurtuo_*.deb && systemctl start nurtuo` fails with "user nurtuo not found", then with a missing config. | medium | packaging | Add nfpm `preinstall`/`postinstall` scripts (system user, `systemctl daemon-reload`) and a `config|noreplace` sample; add an install smoke test in a container. |
| 4 | `nurtuo-control-plane`, bridges and Windows/macOS installers are not built. `nurtuo standalone` supervises a control plane that is not shipped with the runtime archive. | high | packaging | Draft spec above. |
| 5 | The snapshot never runs the built binary beyond `file` and `go version -m`. A binary that panics on start (for example a missing embedded asset) passes. | medium | release QA | Run `nurtuo version` from the linux/amd64 archive and compare with the snapshot version. |
| 6 | Docker compose (`core/deploy/dev/compose.yaml`), Helm (`core/deploy/helm`) and container images were never exercised (PR-STACK.md). A user following those files is the first to run them. | medium | deploy | Lint the chart (`helm lint`, `helm template`) and `docker compose config` in CI, or mark them unsupported. |
| 7 | The `go` job in `ci.yml` cross-compiles all 6 targets on every pull request and the snapshot rebuilds them on runtime changes. | low | CI cost | Drop the loop from `ci.yml` once the snapshot path filter covers all Go modules the runtime imports (`connectors/`, `sdk/go/`, `core/deploy/bundle/` are not in the filter today). |

## Improvement opportunities

- Reproducible builds: set `mod_timestamp` and `-buildid=` so two snapshot runs produce identical
  archives and the checksum is meaningful across builders.
- Publish the snapshot from `main` as a "nightly" pre-release once signing works.

## References

- Open Mercato: `open-mercato-develop-snapshot-release`, `release-upgrade-companion-automation`.

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
