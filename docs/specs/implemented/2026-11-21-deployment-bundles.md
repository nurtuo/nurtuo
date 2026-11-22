<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Deployment bundles `nurtuo.bundle/v1`

Status: implemented (written as built). Date: 2026-11-21.

## TLDR

A deployment bundle is a deterministic, uncompressed USTAR archive whose manifest pins every file
by SHA-256 and is signed with Ed25519 over a domain-separated prefix. The package
`core/deploy/bundle` packs and verifies; the runtime verifies signed bundles only when
`trusted_keys` is configured, and there is no transfer path yet: bundles are placed in `bundle_dir`
out of band because `FetchArtifact` is not implemented.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/22 (squash commit `52d8561`); runtime integration (`SignedBundleSource`) came with https://github.com/nurtuo/nurtuo/pull/25 (`ee4d733`) |
| ADRs | [ADR 0006](../../adr/0006-bundle-format-and-domain-separated-signature.md), [ADR 0009](../../adr/0009-outbound-only-runtime-gateway.md) (artifact transfer) |
| Issues | — |
| Code | `core/deploy/bundle/` (module `github.com/nurtuo/nurtuo/core/deploy/bundle`, AGPL-3.0, stdlib only); runtime use in `core/runtime/internal/runtime/deployer.go` |
| User docs | [core/deploy/bundle/README.md](../../../core/deploy/bundle/README.md), [security.md](../../security.md#bundle-signing-and-verification), [interop.md](../../interop.md#artifacts) |

## Problem and goals

Runtimes sit in customer networks and dial out to the control plane (ADR 0009). A runtime must be
able to prove that the flows it runs are exactly what the control plane signed, and repeated
packaging of the same input must give the same bytes for change detection and audits.

- Goals: tamper-evident container (manifest, files, order, trailing bytes); byte-reproducible
  output; no archive parsing ambiguities (path traversal, duplicate entries, non-regular files);
  bounded memory during verification.
- Non-goals: key distribution, rotation and revocation; compression; encryption of contents;
  transport (see the draft [bundle-artifact-delivery](../2026-11-22-bundle-artifact-delivery.md)).

## Design (as built)

**Layout.** Entries in this exact order (`expectedEntries` in `manifest.go`): `manifest.json`,
`config.json`, `components/<kind>/<id>/<revision>.json` sorted by path, `manifest.sig`. Tar headers
are written with mtime 0, mode 0644, no owners, `tar.FormatUSTAR` (`Pack` in `pack.go`).

**Canonical JSON.** `canonicalJSON` re-encodes every document with sorted keys, no insignificant
whitespace, number literals preserved (`UseNumber`), HTML escaping off, and rejects trailing data.
`rejectDuplicateKeys` walks the token stream first and fails on any repeated object key at any
depth (`ErrInvalid`).

**Manifest.** `Manifest{format, name, createdAt, components[], config, connectors[]}`.
`ComponentEntry` is `{kind, id, revision, path, sha256, size}`; `ConnectorRef` is
`{id, version, digest?}`. Names match `^[a-z0-9][a-z0-9._-]{0,127}$`, revision >= 1, `createdAt` is
supplied by the caller (`Input.CreatedAt`, RFC 3339 UTC): no clock or randomness is read.

**Signature** (`sign.go`). `manifest.sig` is `{"alg":"ed25519","keyId":"<16 hex>","sig":"<base64>"}`
over `"nurtuo-bundle-v1\n" + manifest.json`. `KeyID` is the first 16 hex characters of SHA-256 of
the public key. Keys are hex encoded (`EncodePrivateKey` stores the 32-byte seed).

**Verify** (`verify.go`). Rejects an archive above `MaxBundleSize` (package variable, 256 MiB) before
reading; per entry rejects non-regular types and entries above 64 MiB (`maxEntrySize`), keeps a
running total checked before buffering, rejects duplicate names, then rejects any bytes after the
end-of-archive marker (`ErrNonCanonical`). It checks the signature before trusting the manifest,
then `Format`, `checkManifestShape` (fixed config path, component path derived from kind/id/revision,
strictly sorted components and connectors), the exact entry order, and every hash and size.
Returns `Bundle{Manifest, Files}` without the manifest and signature entries.

**Runtime use.** `SignedBundleSource(dir, keys)` in `core/runtime/internal/runtime/deployer.go`
reads `<dir>/<64 hex>.bundle`, checks that SHA-256 of the archive equals the deployment's
`bundle_digest`, tries `bundle.Verify` with each trusted key, and returns
`{"flows":[...],"connections":[...]}` from components of kind `flow` and `connection`; other
component kinds are ignored. It is selected in `RunWith` (`core/runtime/internal/runtime/run.go`)
when `runtime.yaml` has `trusted_keys` (which requires `bundle_dir`); otherwise
`DirBundleSource` reads plain `<digest>.json` files with no signature check.

## Contracts

| Contract | Stability |
|---|---|
| Archive format `nurtuo.bundle/v1`, entry order, manifest JSON fields, `manifest.sig` JSON | stable: an incompatible change needs `nurtuo.bundle/v2` and a new domain prefix (ADR 0006) |
| Signature domain `"nurtuo-bundle-v1\n"`, `keyId` derivation | stable |
| Go API `Pack`, `Verify`, `Input`, `Component`, `Manifest`, `Bundle`, key helpers, error values `ErrInvalid`, `ErrMissingSig`, `ErrBadSignature`, `ErrHashMismatch`, `ErrNonCanonical`, `MaxBundleSize` | experimental (AGPL module, used by the runtime and tests) |
| Component kinds the runtime reads: `flow`, `connection` | experimental; other kinds (for example script modules) are carried but ignored |
| `runtime.yaml` keys `bundle_dir`, `trusted_keys` (hex Ed25519 public keys) | experimental, see [operations.md](../../operations.md) |

## Security

- Tampering with the manifest, any file, the order, entry types or the container (trailing bytes)
  is detected; the signature is checked before the manifest is parsed for anything else.
- Tar header metadata is not signed and is ignored. The archive hash is not a signed identity;
  identify a bundle by key id plus manifest hash. The runtime nevertheless binds the archive hash to
  the deployment digest, which is safe because `Verify` enforces a canonical container.
- Memory: at most `MaxBundleSize` bytes are buffered; a single entry at most 64 MiB.
- **Unsafe default:** without `trusted_keys` the runtime runs unsigned `<digest>.json` content
  ([security.md](../../security.md) lists this). There is no key rotation, revocation list or
  expiry; `createdAt` is not checked, so an old signed bundle can be replayed if its digest is
  deployed again.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestRoundTrip`, `TestReproducible` (`core/deploy/bundle/bundle_test.go`) | pack then verify; byte-identical output for the same input and key | `cd core/deploy/bundle && GOWORK=off go test -race ./...` | |
| `TestModifiedFileRejected`, `TestModifiedManifestRejected`, `TestWrongKeyRejected`, `TestMissingSignatureRejected`, `TestReorderedEntriesRejected`, `TestExtraAndDuplicateEntriesRejected`, `TestGarbageRejected` | each tampering class | same | |
| `TestPackValidation`, `TestKeyEncoding` | input validation, key round trip | same | |
| `TestVerifyRejectsTrailingGarbage`, `TestPackRejectsDuplicateConnector`, `TestVerifyRejectsBadManifestShape`, `TestVerifyTotalSizeCap`, `TestCanonicalJSONRejectsDuplicateKeys` (`hardening_test.go`) | hardening rules | same | |
| `TestSignedBundleRejections`, `TestChain` (`core/runtime/internal/e2e/chain_test.go`) | runtime accepts signed bundles and rejects tampered or untrusted ones | `cd core/runtime && GOWORK=off go test -race ./internal/e2e/...` | |
| `TestSignedBundleArtifactPath` (`core/runtime/internal/interop/interop_test.go`) | control plane revision digest equals the packaged artifact digest; missing artifact reports `failed` | `cd core/runtime && GOWORK=off go test ./internal/interop/...` | skipped when `node` is not on PATH or the control plane `dist` is missing |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | No transfer path: `FetchArtifact` is unimplemented on both sides (`UnimplementedRuntimeGatewayServiceServer` in `sdk/go/gen/nurtuo/runtime/v1/gateway_grpc.pb.go`). An operator must copy every bundle into `bundle_dir` on every runtime host; a deployment whose file is missing goes to `failed`. | high | runtime, control plane | draft [bundle-artifact-delivery](../2026-11-22-bundle-artifact-delivery.md) |
| 2 | Signature checking is opt-in. A runtime without `trusted_keys` runs any `<digest>.json` an attacker with write access to `bundle_dir` drops there, with no signature. | high | runtime config | require `trusted_keys` whenever `bundle_dir` is set, with an explicit development opt-out key; covered by the artifact delivery draft |
| 3 | No key lifecycle: a leaked signing key stays valid on every runtime until each `runtime.yaml` is edited; there is no revocation list or key expiry. | medium | security | key rotation and revocation in the connections-and-secrets / security area |
| 4 | Only `flow` and `connection` components are read. A bundle that carries a WASM script module (any other kind) verifies, but the module is ignored, so every `script` step fails with `script.load` ("script is not registered"). | high | runtime | draft [bundle-artifact-delivery](../2026-11-22-bundle-artifact-delivery.md) |
| 5 | `Verify` buffers the whole archive (`data []byte`) plus every entry (`io.ReadAll`), so peak memory is about twice the bundle size; with the 256 MiB cap that is about 512 MiB per verification, which can OOM a small runtime container. | low | `core/deploy/bundle` | stream verification from a file and hash entries while reading |
| 6 | ADR 0006 still says "runtime integration pending" although `SignedBundleSource` exists. | low | docs | update the ADR status line |

## Improvement opportunities

- Return a typed `BundleContent` from `SignedBundleSource` instead of re-encoding to JSON and
  parsing again.
- Record the key id and manifest hash in deployment status so audits can tie a running flow to a
  signature.

## References

- Open Mercato: `code-workflow-version-pinning` (pin exactly which definition version runs).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
