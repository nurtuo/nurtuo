<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Bundle artifact delivery (`FetchArtifact`) and script modules in bundles

Status: draft. Date: 2026-11-22.

## TLDR

Signed bundles exist, but nothing moves them from the control plane to a runtime: the
`RuntimeGatewayService.FetchArtifact` RPC is defined in `proto/` and implemented on neither side,
so operators copy files into `bundle_dir` by hand. Bundles also cannot carry WASM script modules,
so every deployed `script` step fails. This spec implements `FetchArtifact` end to end with
digest and signature verification and a local cache, adds a `script` component kind, and makes
signature checking the default.

## Status and links

| | |
|---|---|
| Pull requests | — (builds on https://github.com/nurtuo/nurtuo/pull/22, https://github.com/nurtuo/nurtuo/pull/23 and https://github.com/nurtuo/nurtuo/pull/25) |
| ADRs | [ADR 0006](../adr/0006-bundle-format-and-domain-separated-signature.md), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) |
| Issues | — |
| Code | `proto/nurtuo/runtime/v1/gateway.proto` (`FetchArtifact`, `FetchArtifactRequest{digest}`, `FetchArtifactResponse{data}`), `core/runtime/internal/runtime/deployer.go` (`BundleSource`, `DirBundleSource`, `SignedBundleSource`), `core/runtime/internal/runtime/run.go`, `core/runtime/internal/engine/script.go` (`ScriptSource`), `core/control-plane/src` (gateway server, component revisions), `core/deploy/bundle` |
| As-built specs | [deployment-bundles](implemented/2026-11-21-deployment-bundles.md), [wasm-script-sandbox](implemented/2026-11-21-wasm-script-sandbox.md) |

## Problem and goals

Evidence on `main` (`3d3d506`):

- `TestSignedBundleArtifactPath` (`core/runtime/internal/interop/interop_test.go`): "FetchArtifact
  is not implemented, so 'stores it in bundle_dir' stands in for the artifact transfer."
  [interop.md](../interop.md#artifacts) says the same; the generated Go server stub returns
  `codes.Unimplemented`.
- `BundleSource` comment in `deployer.go`: the source "is expected to hand over already verified
  content"; `DirBundleSource` reads unsigned `<digest>.json` and is the default.
- `SignedBundleSource` keeps only components of kind `flow` and `connection`.
- `RunWith` registers `engine.NewScriptModules()` empty, so every `script` step fails with
  `script.load` ("script is not registered"); [flow-reference.md](../flow-reference.md#script)
  documents this.

Goals:

- The runtime fetches the artifact for a deployment's `bundle_digest` over the existing outbound
  gateway connection; no inbound port, no shared filesystem.
- The runtime verifies the archive digest and the Ed25519 signature before using any byte, with
  the trusted keys from `runtime.yaml`.
- Fetched bundles are cached by digest on disk, so a restart or a control plane outage does not
  stop already deployed flows.
- Bundles carry WASM modules; script steps resolve modules from the bundle of their deployment.
- Unsigned bundles are refused unless explicitly allowed for development.

Non-goals: key rotation and revocation (security area); delta or compressed transfer; a public
artifact registry; connector binaries in bundles.

## Design (proposed)

**RPC.** Use the existing `rpc FetchArtifact(FetchArtifactRequest) returns (stream
FetchArtifactResponse)`. The control plane streams the stored archive in chunks of at most
1 MiB (`data`), only to the authenticated runtime identity (mTLS, ADR 0009) that has a deployment
referencing that digest; otherwise `PERMISSION_DENIED` (no existence oracle: unknown digest also
returns `PERMISSION_DENIED`). No proto change is needed; a total-size field can be added later as
an optional field.

**Runtime source.** New `GatewayBundleSource(client, cacheDir, keys)` implementing `BundleSource`:

1. if `<cacheDir>/<hex>.bundle` exists and verifies, use it;
2. otherwise call `FetchArtifact`, write chunks to a temp file while hashing, abort above
   `bundle.MaxBundleSize`, compare the SHA-256 with the digest, then `rename` into the cache;
3. `bundle.Verify` with each trusted key (as `SignedBundleSource` does) and return the content.

`bundle_dir` stays as a manual override (air-gapped installs) and becomes the cache directory.
`RunWith` selects `GatewayBundleSource` when `trusted_keys` is set; with no keys it refuses to
start unless `allow_unsigned_bundles: true` is set, which keeps `DirBundleSource` and logs a
warning on every deployment.

**Script modules.** New component kind `script` in `nurtuo.bundle/v1`. Because components are
canonical JSON, the module is embedded as
`{"name": "<script name>", "abi": 1, "sha256": "<hex of wasm>", "wasm": "<base64>"}` at
`components/script/<name>/<revision>.json`. `SignedBundleSource` / `GatewayBundleSource` return
the modules with the flows; `EngineDeployer` keeps a per-deployment `ScriptSource` and the
`ScriptRunner` looks scripts up by (deployment, name). Modules are compiled at deploy time
(`wasm.Engine.Compile`), so `invalid_module` and `abi` errors fail the deployment, not the first
document. The `sha256` field is checked after base64 decoding. Base64 inflates modules by a third;
with the 64 MiB entry limit (`maxEntrySize`) a module can be up to about 48 MiB.

**Control plane.** Packing (`Pack`) includes `script` components for every script a flow in the
deployment references; a missing script fails packing with a clear error. The component revision
`digest` field already exists (`POST /v1/components/{id}/revisions`).

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `FetchArtifact` behaviour (chunking, authorization, errors) | implemented, proto unchanged | stable once shipped |
| Bundle component kind `script` and its JSON body | new, additive to `nurtuo.bundle/v1` | experimental |
| `runtime.yaml` `allow_unsigned_bundles`; `bundle_dir` becomes the cache | new or changed | experimental |
| Default: unsigned bundles refused | changed default | stable |

## Compatibility

- Runtimes configured without `trusted_keys` stop at start-up after upgrade until the operator
  either adds keys or sets `allow_unsigned_bundles: true`. This is intentional (breaking for
  insecure setups) and goes in the release notes with the migration steps.
- Old runtimes ignore `script` components and keep failing script steps as today; bundles stay
  readable by them.
- Manually placed `<hex>.bundle` files in `bundle_dir` keep working as cache entries.

## Security

- Every byte is verified (digest, then signature, then per-file hashes) before parsing beyond the
  tar reader; the fetch path writes to a temp file and is bounded by `MaxBundleSize`.
- Authorization: a runtime can fetch only artifacts of its own deployments, so one compromised
  runtime cannot download another customer's flows and connection definitions.
- Connection definitions in bundles reference secrets (`secret://`), never contain them; the
  packer must keep rejecting literal secrets.
- The cache directory holds signed but unencrypted flows; file mode 0600, directory 0700.

## Testing

Acceptance tests:

| Test | Expectation |
|---|---|
| `TestFetchArtifactStreamsSignedBundle` (`core/runtime/internal/interop`) | a deployment with only the control plane holding the artifact reaches `running`; replaces the out-of-band copy in `TestSignedBundleArtifactPath` |
| `TestFetchArtifactDigestMismatch` | the control plane serves different bytes; deployment `failed`, nothing cached |
| `TestFetchArtifactForeignDigestDenied` | runtime rt-2 asks for rt-1's digest and gets `PERMISSION_DENIED` |
| `TestFetchArtifactCacheSurvivesOutage` | after one fetch, a restart with the control plane down still serves the flow |
| `TestFetchArtifactSizeCap` | a stream above `MaxBundleSize` is aborted without buffering it in memory |
| `TestUnsignedRefusedByDefault` (`internal/runtime`) | no `trusted_keys` and no `allow_unsigned_bundles`: start-up error |
| `TestScriptModuleFromBundle` (`internal/e2e`) | a bundle with a `script` component runs the `script` step successfully |
| `TestScriptModuleInvalidFailsDeployment` | a bad module fails the deployment with `script.invalid_module` |

Run: `cd core/runtime && GOWORK=off go test -race ./internal/runtime/... ./internal/e2e/... ./internal/interop/...`
(interop needs Node 20+ and the built control plane, as today) and
`pnpm turbo run test --filter=@nurtuo/control-plane`.

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A 200 MiB bundle over one gRPC stream on a slow link takes minutes; a reconnect restarts from zero. | medium | runtime, gateway | resumable fetch with an offset field (additive proto change) once measured |
| 2 | Embedding WASM as base64 in JSON costs 33 % size and a JSON decode of tens of MiB per deploy. | low | bundle format | a binary `blobs/` entry type in `nurtuo.bundle/v2` if modules grow |
| 3 | The changed default (unsigned refused) breaks every existing test setup that relies on `<digest>.json`. | medium | compatibility | update tests and docs in the same pull request; release note |
| 4 | Cache entries are never evicted; many deployments over months fill the disk. | low | runtime | evict digests not referenced by any deployment after a grace period |

## Improvement opportunities

- Report the verified key id and manifest hash in deployment status (audit trail).
- Prefetch artifacts for scheduled deployments.

## References

- Open Mercato: `code-workflow-version-pinning` (pin and ship exactly the definition version that
  runs).

## Changelog

- 2026-11-22: created.
