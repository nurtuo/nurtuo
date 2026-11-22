<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Release signing, installers and the tagged release

Status: draft. Date: 2026-11-22.

## TLDR

The snapshot dry-run builds unsigned runtime archives only. This spec adds the tagged release
workflow: cosign keyless signatures (GitHub OIDC) over checksums, a Windows MSI, a notarized macOS
package, the `nurtuo-control-plane` executable built with `bun build --compile`, and npm packages
with provenance, so that SECURITY.md's "signed with Sigstore cosign" becomes true.

## Status and links

| | |
|---|---|
| Pull requests | none yet; builds on https://github.com/nurtuo/nurtuo/pull/16 |
| ADRs | to write: "release trust chain (cosign keyless identity, Apple/Windows code signing)" |
| Code (today) | `.goreleaser.yaml` (`signs:` block, skipped), `Makefile` `snapshot` (`--skip=sign`), `.github/workflows/release-snapshot.yml`, TODO(§7.1) comments in all three |
| User docs | [ARCHITECTURE.md §7.1](../ARCHITECTURE.md), [SECURITY.md](../../SECURITY.md) "Release integrity", [release-and-packaging](implemented/2026-11-21-release-and-packaging.md) |

## Problem and goals

Users cannot verify where a `nurtuo` binary came from, Windows and macOS users get no installer
(SmartScreen and Gatekeeper block unsigned binaries), and `nurtuo standalone` needs a control plane
executable that is not built at all.

- Goals:
  1. A workflow on tag `v*` that publishes a draft GitHub release with every artifact in
     ARCHITECTURE.md §7.1 for the runtime and control plane.
  2. Keyless cosign signatures with a verifiable identity
     (`https://github.com/nurtuo/nurtuo/.github/workflows/release.yml@refs/tags/v*`).
  3. MSI (WiX) for Windows amd64/arm64, signed; macOS `.pkg` signed and notarized.
  4. `nurtuo-control-plane` for 6 targets via `bun build --compile`, UI assets embedded, same version.
  5. npm packages for the Apache packages (`@nurtuo/flow-schema`, `flow-validate`, `proto`,
     `client`, `cli`) with `pnpm publish --provenance`.
  6. Snapshot dry-run keeps working and exercises everything except publishing and secrets.
- Non-goals: bridge artifacts (`bridge-jvm` OCI, Python/.NET in M3), container images (optional),
  auto-update, package repositories (apt/yum/Homebrew/winget) beyond attaching files to the release.

## Design (proposed)

### Workflow `.github/workflows/release.yml`

Trigger `push: tags: ["v*"]`; `permissions: contents: write, id-token: write, attestations: write`
on the jobs that need them only; actions pinned by SHA; environment `release` with required
reviewers so a tag alone cannot publish.

| Job | Runner | Produces |
|---|---|---|
| `runtime` | ubuntu | `goreleaser release --clean` (no `--skip=sign`): archives, deb/rpm, SBOMs, `checksums.txt`, `checksums.txt.sigstore.json` |
| `control-plane` | ubuntu | `bun build --compile --target=bun-<os>-<arch>` × 6 of `core/control-plane` with `core/web` assets embedded; archives added to `checksums.txt` before signing |
| `msi` | windows | WiX v4 MSI per arch: installs `nurtuo.exe` and `nurtuo-control-plane.exe`, registers the service via `nurtuo service install`; Authenticode signature |
| `macos-pkg` | macos | `pkgbuild`/`productbuild`, `codesign`, `notarytool submit --wait`, `stapler staple` |
| `npm` | ubuntu | changesets version check, `pnpm publish --provenance --access public` for the 5 Apache packages |
| `verify` | ubuntu | downloads the draft release and runs the commands users run (below) |

Signing order: all archives and installers are built first, `checksums.txt` covers every file, then
cosign signs `checksums.txt` once (the existing `signs:` block). SBOMs are attached per archive and
also attested with `actions/attest-sbom`.

### User verification (documented in SECURITY.md)

```sh
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/nurtuo/nurtuo/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

### Snapshot alignment

`make snapshot` and `release-snapshot.yml` add the `control-plane` build and an unsigned MSI build
(WiX runs on `windows-latest`), so every artifact except signatures is produced nightly. The
`--skip=sign` stays for snapshots; a separate nightly step signs with cosign keyless on `main` only
to keep the OIDC path tested.

### Secrets

Apple Developer ID certificate and notary API key, Windows code-signing certificate (or Azure
Trusted Signing), and the npm token (or npm trusted publishing via OIDC) live in the `release`
environment only.

## Contracts

| Contract | Stability when shipped |
|---|---|
| Signing identity (workflow path and issuer) | stable; changing the workflow file name breaks every documented verify command |
| Artifact names (`nurtuo_<v>_<os>_<arch>.*`, `nurtuo-control-plane_<v>_<os>_<arch>.*`, `nurtuo-<v>-<arch>.msi`, `nurtuo-<v>.pkg`) | stable |
| npm package names and the `nurtuoctl` bin | stable |

## Compatibility

New artifacts only. The archive layout of `nurtuo_*` stays as in the snapshot.

## Security

- Keyless signing ties artifacts to this repository and workflow; no long-lived signing key exists.
- The `release` environment requires reviewer approval and is limited to tags matching `v*`.
- `id-token: write` is granted per job, not workflow-wide; third-party actions in signing jobs are
  SHA-pinned.
- MSI installs the service as `LocalService`, not `LocalSystem`, unless the user opts in.

## Testing (acceptance)

| Test | Acceptance criterion |
|---|---|
| `verify` job | `cosign verify-blob` with the identity above succeeds; `sha256sum -c` passes for every file |
| `verify` job, negative | verification with `--certificate-identity` of another workflow fails |
| MSI smoke (windows runner) | `msiexec /i nurtuo-*.msi /qn`, `nurtuo version` prints the tag, `sc query nurtuo` shows the service, uninstall removes it |
| macOS smoke | `spctl --assess --type install nurtuo-*.pkg` accepts; `stapler validate` passes |
| control plane smoke | `nurtuo-control-plane` starts with embedded PostgreSQL off and answers `GET /livez` (see [observability](2026-11-22-observability-logging-telemetry-health.md)) |
| npm | `npm view @nurtuo/cli dist.attestations` shows provenance; `npx @nurtuo/cli --version` prints the tag |
| snapshot | nightly `release-snapshot` produces 6 control plane archives and 2 MSIs in addition to today's set |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Bun's `--compile` does not support a native module the control plane uses (for example embedded PostgreSQL binaries), so the executable fails at start on one target. | high | control plane | Spike before the workflow: build and start on all 6 targets; fall back to Node SEA if needed. |
| 2 | Apple notarization takes 5–60 minutes or is rejected; the release job times out. | medium | macOS | `notarytool --wait` with a 90-minute timeout; the release stays a draft until `verify` passes. |
| 3 | A leaked npm token publishes a malicious `@nurtuo/cli`. | high | supply chain | npm trusted publishing (OIDC) instead of a token; provenance required. |
| 4 | The `@nurtuo` npm organisation does not exist yet (ARCHITECTURE.md §7.1 "deferred"). | medium | npm | Create the org before the first tag, or skip the `npm` job until then. |
| 5 | Windows code-signing certificates require a hardware token or a cloud HSM; a self-hosted step breaks reproducibility. | medium | Windows | Azure Trusted Signing from the hosted runner. |

## Improvement opportunities

- SLSA build provenance (`actions/attest-build-provenance`) for every archive.
- Homebrew tap and winget manifest generated from the release.
- The runtime verifies bridge signatures with the same cosign identity (SECURITY.md lists this as planned).

## References

- Open Mercato: `open-mercato-develop-snapshot-release`.
- As built: [release-and-packaging](implemented/2026-11-21-release-and-packaging.md).

## Changelog

- 2026-11-22: created as draft.
