<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Bundle artifact delivery (`FetchArtifact`), control-plane signing and standalone `bundle_dir` defaults

Status: approved. Date: 2026-11-24.

Issue: https://github.com/nurtuo/nurtuo/issues/64 (phase 2 of https://github.com/nurtuo/nurtuo/issues/58)

Supersedes: [docs/specs/2026-11-22-bundle-artifact-delivery.md](2026-11-22-bundle-artifact-delivery.md) (whole spec; its script-module part is deferred, see Non-goals)

Extends: [docs/specs/2026-11-23-runtime-bundle-dir.md](2026-11-23-runtime-bundle-dir.md) (Phase 2, decisions D2, D3, D6)

## TLDR

Nothing moves a bundle from the control plane to a runtime: `RuntimeGatewayService.FetchArtifact`
is declared in `proto/` and implemented on neither side, and nothing in production packs or signs a
bundle. This spec implements the existing RPC without a proto change. The control plane packs each
deployed revision into a signed `nurtuo.bundle/v1` archive, sends that archive's digest in
`DesiredState`, and streams it on `FetchArtifact`. A runtime with `trusted_keys` downloads missing
archives into `bundle_dir` and verifies them with the existing `SignedBundleSource`. Standalone then
applies the D3 defaults: `<data-dir>/bundles` with mode `0700`, and a key at
`secrets/bundle-signing.key` with mode `0600` that is reused across restarts. After that, a
standalone deploy reaches `running` without a manual copy. **Approved 2026-11-24: Łukasz accepted D5, D7, D9 and D11.**

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/79 (this spec, docs only); implementation will use `Closes #64` on a later `feat/` PR after approval |
| ADRs | [ADR 0006](../adr/0006-bundle-format-and-domain-separated-signature.md) (bundle format, Ed25519), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (outbound-only gateway, `FetchArtifact`) |
| Issues | https://github.com/nurtuo/nurtuo/issues/64; https://github.com/nurtuo/nurtuo/issues/58 (phase 1 shipped in https://github.com/nurtuo/nurtuo/pull/71); related https://github.com/nurtuo/nurtuo/issues/61, https://github.com/nurtuo/nurtuo/issues/62, https://github.com/nurtuo/nurtuo/issues/57 (approved spec, PR https://github.com/nurtuo/nurtuo/pull/73), https://github.com/nurtuo/nurtuo/issues/60 (merged in PR https://github.com/nurtuo/nurtuo/pull/75), tracker https://github.com/nurtuo/nurtuo/issues/67 |
| Code (today) | `proto/nurtuo/runtime/v1/gateway.proto` (`FetchArtifact`, `FetchArtifactRequest{digest}`, `FetchArtifactResponse{data}`); `core/control-plane/src/gateway.ts` (`Gateway`, `desiredStateMessage`, `register`, `createGatewayServer`, `peerKey`); `core/control-plane/src/api.ts` (`canonicalJson`, `digestOf`, `POST /v1/components/{id}/revisions` with optional `digest`); `core/control-plane/src/main.ts` (env `GATEWAY_*`); `core/deploy/bundle` (`Pack`, `Verify`, `GenerateKey`, `EncodePrivateKey`, `DecodePrivateKey`, `EncodePublicKey`, `KeyID`, `MaxBundleSize`); `core/runtime/internal/runtime/deployer.go` (`BundleSource`, `DirBundleSource`, `SignedBundleSource`, `EngineDeployer.Apply`); `core/runtime/internal/runtime/run.go` (`RunWith`, `unconfiguredDeployer`, `errNoBundleDir`); `core/runtime/internal/runtime/config.go` (`Config.BundleDir`, `Config.TrustedKeys`, `trustedKeys`, `Validate`); `core/runtime/internal/gateway/client.go` (`Client`, `Run`, `session`); `core/runtime/cmd/nurtuo/standalone_layout.go` (`DataDirs`, `NewDataDirs`, `Ensure`); `core/runtime/cmd/nurtuo/standalone_bootstrap.go` (`localBootstrapper.Bootstrap`, `Credentials`, `loadOrGenSecret`); `core/runtime/cmd/nurtuo/standalone_register.go` (`runRealRuntime`); `core/runtime/cmd/nurtuo/standalone.go` (`controlPlaneEnv`, `Stack.Run`) |
| User docs | [operations.md](../operations.md) (`runtime.yaml` table, standalone section), [interop.md](../interop.md#artifacts), [security.md](../security.md) |
| Pre-implement report | [analysis/pre-implement-bundle-artifact-delivery.md](analysis/pre-implement-bundle-artifact-delivery.md) |

## Goal

Who needs this: operators and canvas users who deploy a flow to a runtime, standalone first, and
CI jobs that run the standalone or interop stack. Today every deployment needs a bundle file
copied into `bundle_dir` out of band. Standalone has no `bundle_dir` at all, so after #71 every
standalone deploy honestly ends `failed` with "no bundle_dir configured" (accepted interim, D6).
This spec has the control plane pack, sign and serve the bundle over the existing gateway. The
runtime fetches it and verifies the digest and the signature before use. Standalone gets a bundle
directory and an install-local key (D3), so a deploy reaches `running` with no manual step. M1
epic 8 ("signed-bundle deploy") moves from partial to done.

Goals:

- A deployment's bundle reaches the runtime over the runtime's own outbound mTLS gateway connection
  (ADR 0009). No inbound port, no shared filesystem.
- Every fetched byte is checked against the digest. In signed mode the Ed25519 signature is checked
  (`bundle.Verify`) before anything is parsed.
- `nurtuo standalone` deploys reach `running` without a manual copy into `bundle_dir`. This
  includes existing installs whose bundle and secrets directories and any existing signing key
  pass the validation below.
- No proto change and no REST change. Existing `bundle_dir` / `trusted_keys` semantics are kept.
- Phase 1 of #58 (#71) is not weakened: a runtime without `bundle_dir` still never reports `running`.

## Resolved questions

None blocking. Q1 to Q4 were answered by Łukasz on 2026-11-24, 12:29 Europe/Warsaw (he accepted
all recommendations). They are recorded in Decisions:

- **Q1 → D7: yes.** TypeScript packer in the control plane, with cross-language tests.
- **Q2 → D5: yes.** Opt-in `NURTUO_BUNDLE_SIGNING_KEY_FILE`; archive digest in `DesiredState` when set.
- **Q3 → D9: signed only.** `FetchArtifact` without a signing key returns `FAILED_PRECONDITION`.
- **Q4 → D11: wire the Windows bundle directory to #57.** #57 shipped in #85, so wire it in Phase 3.

Non-blocking (implementer defaults, see Decisions): D1, D4, D8, D10, D12 to D16. Non-blocking open
items are tracked in Gaps and risks.

## Scope

In scope:

- Control plane: `FetchArtifact` server handler; deterministic packing and Ed25519 signing of a
  deployed revision; serving only to the runtime whose desired state references the digest.
- Runtime: a fetching wrapper around the existing `SignedBundleSource`. It downloads a missing
  `<hex>.bundle` into `bundle_dir` over the gateway connection, checks the size and digest, then
  hands off to the existing verification.
- Standalone: D3 defaults (`DataDirs.Bundles`, signing key generation and reuse, `BundleDir` and
  `TrustedKeys` in `runRealRuntime`, the key path passed to the control plane child).
- How this composes with #61 (unsigned digest check, empty-`trusted_keys` warning), #62, #57, #60.
- Tests and docs (`operations.md`, `interop.md`, release note) in the same PRs as the code.

Non-goals:

- Key rotation or revocation (D3: no rotation). Multiple signing keys on the control plane.
- #61 work: SHA-256 verification in `DirBundleSource`, the empty-`trusted_keys` warning, making
  `trusted_keys` the default for `service install`. This spec only defines the seam (D8).
- #62 (flow keys by `<deployment>/<flow>`), #57 (Windows DACL implementation, `perm_windows.go`, Windows
  CI job), #60 (moving `ApplyDesiredState` off the receive loop).
- Script (WASM) modules in bundles and a `script` bundle component kind. That part of the
  superseded 11-22 draft is deferred and needs its own spec.
- `allow_unsigned_bundles` or any other new `runtime.yaml` key. Refusing unsigned bundles by
  default (11-22 draft) is a breaking config change. It is not proposed here.
- A key generation or `bundle pack` CLI command (new CLI surface, Ask First). Persisted artifact
  storage (DB table). Resumable, delta or compressed transfer. Cache eviction.
- Packing connection definitions or multiple flows per deployment. A deployment stays one
  component revision, as `desiredStateMessage` builds it today.

## Design

### Current behaviour (as the code works, `main` at `c9d1a7c`)

- **Proto.** `rpc FetchArtifact(FetchArtifactRequest) returns (stream FetchArtifactResponse)`,
  `FetchArtifactRequest{string digest = 1}`, `FetchArtifactResponse{bytes data = 1}`.
  `Deployment.bundle_digest` is commented "sha256 of the signed bundle". The proto header says
  additive changes only within v1. The as-built gateway spec lists `FetchArtifact` as
  "experimental, unimplemented".
- **Control plane.** `Gateway.register` registers only `connect`, so `FetchArtifact` returns
  `UNIMPLEMENTED`. `desiredStateMessage(runtimeId)` sends one `Deployment{deploymentId:
  componentId, bundleDigest: rev.digest}` per desired-state item. `rev.digest` is
  `digestOf(content)` (`sha256:` + hex of `canonicalJson(content)`, `api.ts`), or a
  caller-supplied `digest` for "an externally packaged artifact". In that case the control plane
  stores only the JSON `content`, never the archive bytes. The control plane is TypeScript. It
  has no packer, no signer and no signing key. `main.ts` starts the gateway only when
  `GATEWAY_CERT`, `GATEWAY_KEY` and `GATEWAY_CA` are set.
- **Bundle package** (`core/deploy/bundle`, Go). `Pack(Input, ed25519.PrivateKey)` produces a
  deterministic USTAR archive (`manifest.json`, `config.json`, `components/<kind>/<id>/<rev>.json`,
  `manifest.sig`) with the signature over `"nurtuo-bundle-v1\n" + manifest.json`.
  `Verify(data, pub)` checks the size cap (`MaxBundleSize`, 256 MiB), the entries, that no
  bytes follow the end-of-archive marker, the signature, the order and the hashes. It does not
  require component files to be byte-identical to Go's canonical form; it checks the manifest
  hashes. It is used only by tests (`internal/interop/harness_test.go` `putSigned`,
  `internal/e2e`, `internal/runtime/signed_test.go`).
- **Runtime.** `RunWith`: injected `Options.Deployer` wins. Otherwise, with `BundleDir != ""` it
  builds an `EngineDeployer` with `SignedBundleSource(dir, keys)` when `TrustedKeys` is non-empty,
  else `DirBundleSource(dir)`. Otherwise (#71) it logs a WARN and uses `unconfiguredDeployer`,
  whose `Apply` returns `errNoBundleDir`. It carries `TODO(#64)`. `SignedBundleSource` reads
  `<dir>/<hex>.bundle`, checks SHA-256 against the digest, then `bundle.Verify` with each trusted
  key, and keeps `flow` and `connection` components. `EngineDeployer.Apply` wraps source errors as
  `deployer: fetch bundle <digest>: <err>`. The supervisor turns an `Apply` error into
  `DeploymentStatus{state: "error"}` and does not advance the revision. The control plane maps
  `error` to `failed` (`STATE_MAP`).
- **Gateway client.** `Client.Run` creates the `grpc.ClientConn` locally (`grpc.NewClient`) and
  keeps it private. Nothing can issue a second RPC on it. `ApplyDesiredState` ran synchronously
  on the receive goroutine when this spec was drafted. PR #75 (#60) is merged on `main` and moves it
  off the receive loop, so Phase 2 builds on that.
- **Standalone.** `DataDirs` has `Root`, `Postgres`, `PGRun`, `PGCache`, `Logs`, `Secrets`, and no
  bundles directory. `Ensure` creates `Secrets` with `0o700` and the others with `0o755`.
  `localBootstrapper.Bootstrap` generates the ECDSA CA, certificates, `db-password` and
  `admin-token`, but no Ed25519 key. `loadOrGenSecret` writes 24 random bytes as hex, which is not
  an Ed25519 seed. `runRealRuntime` builds `nrt.Config` without `BundleDir` or `TrustedKeys`
  (D2). `controlPlaneEnv` passes `DATABASE_URL`, `PORT`, `GATEWAY_PORT`, `GATEWAY_CERT`,
  `GATEWAY_KEY`, `GATEWAY_CA`, `NURTUO_AUTH_SECRET` and `NURTUO_BOOTSTRAP_*`.
  `TestStandaloneE2EHandshakeAndDeploy` asserts `failed` with a message containing `bundle_dir`.

### Drift against prior specs (design is against the code)

| Prior spec says | Code does | Reference | Handling |
|---|---|---|---|
| 11-22 draft: "The control plane streams the **stored** archive" | The control plane stores no archive and has no packer; revisions hold JSON content only | `api.ts` `POST …/revisions`, `repository.ts` `createRevision` | This spec adds packing on demand (D4) |
| `core/deploy/bundle/README.md`: "The control plane packs and signs" | Only Go tests call `Pack`; the control plane is TypeScript | `harness_test.go` `putSigned` | D7 (TS packer, decided) |
| 11-22 draft: `bundle_digest` is the digest the runtime fetches; implicit that it is the archive hash | `bundle_digest` is `digestOf(content)` (canonical JSON), so `SignedBundleSource` can never match it unless a digest is supplied by hand | `gateway.ts` `desiredStateMessage`, `api.ts` `digestOf` | D5: with a signing key the control plane sends the archive digest, matching the proto comment |
| 11-22 draft: "a restart with the control plane down still serves the flow" (`TestFetchArtifactCacheSurvivesOutage`) | The runtime persists no desired state; after a restart nothing is applied until `DesiredState` arrives | `supervisor`, `run.go` | Dropped. The cache avoids re-downloads only; Gaps #5 |
| 11-22 draft: refuse to start without `trusted_keys` unless `allow_unsigned_bundles: true` | No such key; empty `trusted_keys` is valid and selects `DirBundleSource` | `config.go` `Validate` | Out of scope (config contract; #61 owns the warning) |
| 11-22 draft: unknown or foreign digest → `PERMISSION_DENIED` | — (unimplemented) | — | D1 uses `NOT_FOUND` for both (still no existence oracle; honest for an authorised caller) |
| 11-22 draft: script modules in bundles | Script steps fail with "script is not registered" (`RunWith` registers empty `engine.NewScriptModules()`) | `run.go` | Deferred, own spec |
| 11-22 draft predates #71 | `unconfiguredDeployer` replaces `memDeployer` | `run.go` | Kept unchanged for `bundle_dir == ""` |
| runtime-bundle-dir Phase 2: key "hex seed via the existing `loadOrGenSecret` pattern" | `loadOrGenSecret` makes a 24-byte hex token, not a 32-byte Ed25519 seed | `standalone_bootstrap.go` | A sibling `loadOrGenSigningKey` with the same create-once/reuse behaviour (D3 unchanged) |

### Proposed approach

Three phases (D13), each shippable on its own. Component names exist today unless marked *new*.

#### A. Control plane: pack, sign, serve (TypeScript, `core/control-plane/src/`)

1. ***New* `bundle.ts`** (AGPL-3.0-only, SPDX header). `packBundle(input, privateKey): Buffer`
   implements `nurtuo.bundle/v1` with the USTAR layout and signature rules of `core/deploy/bundle` (D7, decided):
   - Entries in order: `manifest.json`, `config.json` (`{}`), `components/<kind>/<id>/<revision>.json`.
     Then `manifest.sig` with `{"alg":"ed25519","keyId":<first 16 hex of sha256(pub)>,"sig":<base64>}`.
     The signature is `crypto.sign(null, "nurtuo-bundle-v1\n" + manifestBytes, key)`.
   - Manifest field order as Go's `Manifest` struct (`format`, `name`, `createdAt`, `components`,
     `config`, `connectors`). `ComponentEntry` order: `kind`, `id`, `revision`, `path`, `sha256`,
     `size`. `connectors: []`.
   - USTAR headers: mode `0644`, mtime 0, no owner names, typeflag `0`, `ustar\0` + `00`. The
     archive ends with exactly two 512-byte zero blocks and no record padding. `Verify` rejects
     trailing bytes, so GNU-style 10 KiB padding would fail.
   - Check the 64 MiB per-entry and 256 MiB archive limits before archive allocation, signing,
     or digest publication. Test an entry at the limit and one byte above it. Names must match
     `^[a-z0-9][a-z0-9._-]{0,127}$` (Go `nameRe`).
   - Key loading: the file holds the hex Ed25519 seed (`bundle.EncodePrivateKey` format). It is
     wrapped as PKCS#8 DER and loaded with `crypto.createPrivateKey`. No new dependency.
2. **Input mapping** (D16). For a desired-state item (component `c`, revision `r`):
   `name` = `c.name` when it matches `nameRe`, else `c.id`; `createdAt` = `r.createdAt` truncated
   to seconds, RFC 3339 UTC; one component `{kind: c.kind, id: c.id, revision: r.number,
   document: canonical bundle JSON of r.content}`. Sort object keys by UTF-8 bytes and
   use Go bundle JSON string escaping (escape U+2028 and U+2029, leave HTML-sensitive
   characters literal). Replace
   lone surrogates with U+FFFD and reject keys that collide after replacement. Emit the shortest
   ECMAScript spelling of each stored finite number and preserve that literal in the bundle.
   The input is the stored JSON value. Request number spellings and duplicate keys were consumed by
   the API parser. ADR 0006's rules for raw input to Go Pack still apply. All inputs are immutable, so the same revision and key
   always give the same bytes and the same digest. This holds across control-plane restarts.
3. **Which digest goes into `DesiredState`** (D5, decided).
   The packer-only PR advertises stored revision digests regardless of signing configuration.
   Once the delivery handler is available, signing lets desired state advertise the archive digest
   for revisions whose stored digest equals `digestOf(content)`. The equality is a content test.
   The repository stores no digest provenance for new or legacy rows, so an explicitly supplied
   matching digest is also eligible. Non-matching digests pass through unchanged.
   Replay current desired state at reconnect even when the generation matches, so a signing key
   change, enablement or disablement takes effect for existing runtimes.
   If packing fails (for example the content is not a JSON object, or it is too large), it sends
   the revision digest. `FetchArtifact` then reports the pack error (below), so the deployment
   fails with a readable message instead of disappearing.
4. **Cache** (D4). The packed archive is kept in an in-process cache keyed by
   `revisionId + ":" + keyId`, with a digest-to-entry index. It is bounded by total bytes (default
   256 MiB, oldest-first eviction). An evicted entry is re-packed on demand, deterministically.
   The archive SHA-256 addresses the exact bytes for transfer integrity, digest lookup and the
   runtime cache filename. ADR 0006 identifies the signed bundle by signing key id plus
   manifest hash. Unsigned tar metadata can change the archive digest while leaving that
   identity intact. Fetch authorization still uses the runtime's desired state, and the
   runtime verifies the signature and manifest before using the archive.
   No database table and no migration.
5. **`FetchArtifact` handler**, registered in `Gateway.register` next to `connect` (D1):
   - Identity: `ctx.values.get(peerKey)`. If missing: `UNAUTHENTICATED` (same text as `connect`).
   - `digest` must match `^sha256:[0-9a-f]{64}$`; a bare 64-hex digest is also accepted, as
     `SignedBundleSource` strips the prefix. Otherwise `INVALID_ARGUMENT`.
   - No signing key configured: `FAILED_PRECONDITION` "control plane has no bundle signing key
     configured (NURTUO_BUNDLE_SIGNING_KEY_FILE)". Under D9 (decided), unsigned content is
     not served.
   - Authorisation: maintain a per-runtime digest-to-revision view from current desired state,
     independent of archive eviction. Check that the indexed deployment is still current before
     serving. Unknown and foreign digests get identical `NOT_FOUND` responses without packing.
     A cache miss repacks only the matching revision. Coalesce duplicate repacks and bound
     concurrent packing and streaming. Under cache churn, test that an authorized miss packs
     one revision and that an unknown or foreign request packs zero.
   - A matching deployment whose revision cannot be packed returns `FAILED_PRECONDITION` with the
     pack error. A digest that matches a non-packing revision digest returns `NOT_FOUND`
     "artifact is not stored by the control plane; place it in bundle_dir".
   - Stream the archive in chunks of at most 1 MiB (`data`), well under gRPC's 4 MiB default
     message limit. Stop on client cancel.
6. **Configuration** (D5, decided). `main.ts` reads
   `NURTUO_BUNDLE_SIGNING_KEY_FILE`. If it is set but unreadable or not a 32-byte hex seed, the
   process exits 1 with a message that names the variable and never prints the key. If it is
   valid, the log says `gateway: bundle signing enabled (key id <16 hex>)`. `GatewayOptions` gains
   `signingKey?: { privateKey: KeyObject; keyId: string }`, so tests inject a key without env.

#### B. Runtime: fetch into `bundle_dir`, verify with the existing source (Go, `core/runtime/internal/`)

1. **`gateway.Client.FetchArtifact`** (*new* method, `client.go`):
   `FetchArtifact(ctx, digest string, w io.Writer, limit int64) (int64, error)`. `Run` stores its
   `grpc.ClientConn` in a mutex-guarded field. `FetchArtifact` calls
   `gatewayv1.NewRuntimeGatewayServiceClient(conn).FetchArtifact` on that same connection (same
   mTLS identity, no second dial). Before `Run` has dialled it returns `gateway: not connected`. It
   copies `data` chunks to `w` and aborts with an error once more than `limit` bytes arrived,
   without buffering the stream in memory. The call has its own deadline (default 5 min,
   unexported constant; D10). It is a separate HTTP/2 stream, so it does not block `Connect`.
2. ***New* `FetchingBundleSource(dir string, fetch FetchFunc, inner BundleSource) BundleSource`**
   in `deployer.go`, with
   `type FetchFunc func(ctx context.Context, digest string, w io.Writer, limit int64) (int64, error)`.
   For a digest:
   1. Validate the digest the same way as `SignedBundleSource` (64 lowercase hex after `sha256:`).
   2. If `<dir>/<hex>.bundle` exists, call `inner`. On success, return.
   3. Otherwise (missing, or `inner` failed on the existing file), fetch into
      `os.CreateTemp(dir, ".fetch-*")` (mode `0600`) while hashing, with
      `limit = bundle.MaxBundleSize`. Check SHA-256, verify the signature and decode the
      components using the same verification path as `SignedBundleSource` on the staged file.
   4. After verification, fsync, close and rename to `<hex>.bundle`. On failure remove the
      temp file and preserve any prior file. Test rejected and verified replacements.
   - At `RunWith` start, leftover `.fetch-*` files in `bundle_dir` are removed (crash debris).
3. **`RunWith` wiring.** Only when `BundleDir != ""` **and** `TrustedKeys` is non-empty (signed
   mode): `src = FetchingBundleSource(cfg.BundleDir, fetch, SignedBundleSource(cfg.BundleDir, keys))`.
   `fetch` is a closure bound to the `*gateway.Client` built later in the same function. Unsigned
   mode (`DirBundleSource`) is unchanged and does not fetch (D8, D9). `BundleDir == ""` keeps
   `unconfiguredDeployer` and its WARN unchanged, so #71 is not weakened. No new `runtime.yaml` key.
   Fetching in signed mode is always on. Against an old control plane, `UNIMPLEMENTED` turns into
   the same `error` state a missing file produces today, with a clearer message.
4. **Operator-visible messages**, all wrapped by the existing
   `deployer: fetch bundle <digest>: …`, so they appear as deployment `failed` with this message:

   | Cause | Message after the prefix |
   |---|---|
   | old control plane (`UNIMPLEMENTED`) | `control plane does not implement FetchArtifact; place <hex>.bundle in bundle_dir` |
   | `NOT_FOUND` | `artifact not available from the control plane for this runtime: <server text>` |
   | `FAILED_PRECONDITION` | `control plane cannot serve artifact: <server text>` (no signing key, or pack error) |
   | size cap | `artifact larger than 268435456 bytes` |
   | digest mismatch | `fetched artifact does not match digest` (nothing cached) |
   | signature or format | existing `deployer: bundle <digest> rejected: bundle: signature verification failed` (or `invalid`, `content hash mismatch`) |
   | not connected or transport error | `fetch artifact: <grpc status>`; retried on the next desired state or reconnect (supervisor behaviour, unchanged) |

#### C. Standalone D3 defaults (Go, `core/runtime/cmd/nurtuo/`)

1. `DataDirs.Bundles = <root>/bundles` (`NewDataDirs`). `Ensure` creates it with
   mode `0700` on Unix. For an existing directory, open `bundles` relative to the data-root
   directory descriptor with `O_DIRECTORY | O_NOFOLLOW`, validate that the opened object is a
   directory owned by the effective user, then tighten its mode to `0700` using `fchmod` on
   that same descriptor. If creation races with an existing entry, apply the same checks.
   Reject a symlink before changing permissions or writing any bundle. On Windows, call
   `secrets.EnsurePrivateDir(d.Bundles)` (D11).
2. `localBootstrapper.Bootstrap` creates or loads `<secrets>/bundle-signing.key` with a *new*
   `loadOrGenSigningKey(path)`:
   - On Unix, require a real secrets directory owned by the effective user with mode `0700`
     and a regular signing key owned by that user with mode `0600`. Windows uses the
     standalone private DACL and ownership checks from #57. Reject symlinks (including
     dangling links) and Windows reparse points at either path before reading or creating
     the key. Validate ownership and permissions on the objects used for key I/O. If ownership
     or mode fails validation, report the affected path and the expected owner and mode so the
     operator can correct them and restart. Leave any existing key untouched.
   - If the file exists, read it and `bundle.DecodePrivateKey`. A corrupt file fails bootstrap
     with "bundle-signing.key is invalid; restore it from backup or delete it to generate a new
     key". It is never silently regenerated, because silent regeneration would be a rotation (D3).
   - If the file is missing, create an exclusive temporary file in the validated secrets
     directory with mode `0600` on Unix or a private DACL on Windows. Write, sync and close it
     before publishing the final name atomically:
     - Linux uses `renameat2` with `RENAME_NOREPLACE`.
     - macOS uses `renamex_np` with `RENAME_EXCL`.
     - Windows uses same-volume `MoveFileExW` with both `MOVEFILE_REPLACE_EXISTING` and
       `MOVEFILE_COPY_ALLOWED` omitted.
     - On Unix, if the exclusive rename is unavailable, use `linkat` to publish the staged
       file at the final name, then unlink the temporary name. A volume without hard links
       can still use its exclusive rename primitive.
   - If the destination already exists, discard the staged seed and reopen, validate and
     decode the winner using the same checks as for an existing key. If the filesystem
     supports none of the applicable atomic no-replace primitives, remove the staged file
     and fail bootstrap with the path and an error explaining that the data directory needs
     a filesystem that supports atomic no-replace publication. Never fall back to an
     overwriting rename or to writing directly at the final path.
   - `Credentials` gains `BundleSigningKeyFile` (path) and `BundlePublicKey` (hex,
     `bundle.EncodePublicKey`). The key bytes are never logged.
3. `controlPlaneEnv` appends `NURTUO_BUNDLE_SIGNING_KEY_FILE=<path>` (D5).
4. `runRealRuntime` sets `BundleDir: env.Dirs.Bundles` and
   `TrustedKeys: []string{env.Creds.BundlePublicKey}`, through a small
   `standaloneRuntimeConfig(gatewayAddr, env) nrt.Config` helper so it is unit-testable. Signed
   mode is selected, so phase B fetches.
5. Remove the `TODO(#64)` on `unconfiguredDeployer`. It stays for non-standalone runtimes without
   `bundle_dir`. Update the "Limitation (#58 …)" text in `operations.md`.

### Composition with #61 (D8, assumption stated)

Assumed direction of #61, from the issue text: `DirBundleSource` will check that
`sha256(<hex>.json) == digest`, the runtime will log a WARN at start when `trusted_keys` is empty,
and `service install` will default to `trusted_keys`. #61 does not change `SignedBundleSource`, the
`<hex>.bundle` layout or the gateway.

- **Signed mode (this spec):** digest check, then signature check, then manifest hashes, for both
  fetched and manually placed archives. This is `SignedBundleSource`, unchanged, plus the
  pre-rename digest check in `FetchingBundleSource`. The digest check is the step #61 adds for
  unsigned bundles. If #61 lands first with an exported or unexported helper (for example
  `checkDigest(data, digest) error` in `deployer.go`), `FetchingBundleSource` reuses it. If this
  spec lands first, it adds that helper and #61 reuses it. Neither side changes the other's
  source function.
- **Unsigned mode (no `trusted_keys`):** unchanged by this spec. No fetch, `DirBundleSource` reads
  `<hex>.json`, and #61 adds the digest check and the warning. A control plane with a signing key
  sends archive digests, which an unsigned runtime cannot use; its deployments fail with
  `no such file` as today. This is documented as "enable `trusted_keys` on every runtime before
  setting `NURTUO_BUNDLE_SIGNING_KEY_FILE`" (Compatibility).
- **Empty `trusted_keys` warning:** #61's warning text should mention that signed bundles are
  fetched from the control plane only when `trusted_keys` is set. Suggested wording, not owned
  here.
- If D9 is answered "serve unsigned content too", the unsigned path becomes
  `FetchingBundleSource(dir, fetch, DirBundleSource(dir))` writing `<hex>.json`. The control plane
  then serves exactly `canonicalJson(content)` for derived digests. #61's digest check makes this
  safe. This is the only change; the seam stays the same.

### Related work: #62, #60, #57

- **#62** (flow keys by deployment and flow id): artifacts are content-addressed by archive digest,
  and the cache file name is `<hex>.bundle`, independent of flow ids. The manifest component id is
  the control-plane component id (= `deployment_id`). This spec adds no new flow-id-keyed state and
  does not touch `EngineDeployer.flows` or `connhost.Manager`. Two deployments of the same revision
  share one read-only cache file. That is compatible with `<deployment>/<flow>` keys.
- **#60 / PR #75 (merged):** fetching happens inside `EngineDeployer.Apply`, which now runs off
  the gateway receive loop, so a large download does not block commands or the receive goroutine.
  Phase 2 is implemented on top of #75 and must be rebased on it. There is no ordering dependency
  left (Gaps #3).
- **#57 / PR #85 (merged):** standalone already applies the private DACL to `secrets\`.
  Apply its `secrets.EnsurePrivateDir` to `<data-dir>\bundles` in Phase 3 (D11). Create the
  signing seed with a private DACL and validate the key on load.

### Alternatives considered

| Option | Why not |
|---|---|
| Runtime-side packing/signing (runtime fetches JSON, signs locally) | Signing and verifying in one process proves nothing; no tamper protection for the cache |
| Control plane shells out to a Go `nurtuo bundle pack` | New CLI surface (Ask First); the distributed control plane image would need the Go binary |
| Pack at `POST /v1/deployments` and persist in a new `artifacts` table | Needs a migration and a REST error path; on-demand deterministic packing gives the same digests with no schema change. Kept as a follow-up if packing cost shows up (D4) |
| New proto fields (size, offset, key id) | Not needed: size is bounded on the client, and the key id is inside `manifest.sig`. Can be added later as optional fields |
| Make `bundle_dir` required, or default `bundle_dir` for every runtime | Config contract change; #71 behaviour must stay |

### Failure modes and what the operator sees

| Situation | Operator sees |
|---|---|
| Standalone, normal deploy | `pending` → `running`; `<data-dir>/bundles/<hex>.bundle` appears |
| Control plane restarted (same key) | same digests (deterministic); runtime cache hit, no download |
| `bundle-signing.key` deleted | next start generates a new key; control plane repacks; runtime finds the old cached file fails the signature check, fetches again and replaces it. Deployments return to `running` after reconnect. This is recovery, not a rotation feature |
| `bundle-signing.key` corrupt | `nurtuo standalone` exits 1: "bootstrap: bundle-signing.key is invalid …" |
| `NURTUO_BUNDLE_SIGNING_KEY_FILE` invalid (distributed) | control plane exits 1 naming the variable |
| Runtime with `trusted_keys` against a control plane without a key | `failed`: "control plane cannot serve artifact: control plane has no bundle signing key configured …" |
| Runtime trusts a different key than the control plane signs with | `failed`: "bundle … rejected: bundle: signature verification failed"; nothing cached |
| Desired state changed between delivery and fetch | `failed` "artifact not available …", then the next `DesiredState` (already queued, since that is what changed) re-applies |
| Disk full in `bundle_dir` | `failed` "fetch artifact: write …: no space left on device"; temp file removed |
| Runtime without `bundle_dir` | unchanged (#71): `failed` "no bundle_dir configured …", never `running` |

### Rollback

- Phase A: unset `NURTUO_BUNDLE_SIGNING_KEY_FILE` and the control plane sends revision digests
  again, as today. Reverting the code removes the handler, and runtimes get `UNIMPLEMENTED` →
  `failed`.
- Phase B: reverting removes fetching. Cached `<hex>.bundle` files keep working as manual bundles.
- Phase C: reverting removes `BundleDir` and `TrustedKeys` from standalone, so it goes back to #71
  behaviour (`failed`). `bundles/` and `bundle-signing.key` stay on disk unused. Do not delete the
  key while cached bundles signed with it are still wanted.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `gateway.proto` `FetchArtifact`, `FetchArtifactRequest`, `FetchArtifactResponse` | none (implemented as declared) | FROZEN names/numbers; behaviour experimental → stable once shipped |
| `FetchArtifact` behaviour: chunk ≤ 1 MiB, error codes (`UNAUTHENTICATED`, `INVALID_ARGUMENT`, `NOT_FOUND`, `FAILED_PRECONDITION`), authorisation by desired state | new behaviour on an existing RPC | experimental |
| `Deployment.bundle_digest` | with a signing key: archive digest (matches the proto comment "sha256 of the signed bundle"); without: unchanged | STABLE field; value change is opt-in (D5) |
| Control plane env `NURTUO_BUNDLE_SIGNING_KEY_FILE` | **new**, additive, optional (D5, decided) | STABLE once shipped (control plane env is STABLE in the compatibility contract) |
| `nurtuo.bundle/v1` | none; a second (TS) writer must produce archives `bundle.Verify` accepts (D7) | FROZEN |
| `runtime.yaml` `bundle_dir`, `trusted_keys` | no schema change; signed mode additionally fetches missing archives | STABLE |
| Standalone data dir layout | adds `bundles/` (`0700`) and `secrets/bundle-signing.key` (`0600`, hex Ed25519 seed). Additive; decided in D3 | FROZEN (additive entries, no format change) |
| REST API, CLI flags, MCP tools | none | — |
| `internal/` Go APIs (`FetchingBundleSource`, `gateway.Client.FetchArtifact`, `DataDirs.Bundles`, `Credentials` fields) | new | internal, not a contract |

## Compatibility

- **No proto, REST, CLI or `runtime.yaml` schema change.** `buf breaking` is unaffected. The one
  new public surface is the additive control-plane env variable (D5).
- **Existing standalone installs (D6 interim, D12).** On the first start after upgrade, `Ensure`
  prepares `bundles/` and bootstrap loads or generates `bundle-signing.key` if the secrets
  directory and any existing key pass validation. The control plane child gets
  the key and the runtime trusts it. The runtime restarts with revision 0, so the control plane
  resends the desired state on Hello. Deployments that were `failed` ("no bundle_dir configured")
  are applied again, fetched, and move `failed` → `pending` → `running`. No manual step and no
  data migration is needed for those installs. Bootstrap fails closed on a legacy Unix secrets
  directory with loose permissions or foreign ownership. The operator must correct its owner
  or mode and restart. The release note documents both outcomes.
- **Distributed control plane, unchanged config.** Without the new variable nothing changes:
  revision digests, `bundle_dir` copies, `FetchArtifact` returns `FAILED_PRECONDITION`.
  Signed-mode runtimes now show a clearer message for a missing file.
- **Distributed control plane enabling signing.** Opt-in. All runtimes of that control plane get
  archive digests, so every runtime needs `bundle_dir` and `trusted_keys` containing the control
  plane's public key first. Unsigned runtimes would fail with `no such file`. Documented in
  `operations.md`. Getting the public key from the hex seed has no tool yet (Gaps #7).
- **Old runtime against a new control plane with a key:** it receives archive digests. With
  `trusted_keys` and a manually placed archive of that digest it works; otherwise `failed`, as
  today.
- **New runtime against an old control plane:** `UNIMPLEMENTED` → `failed` with a pointer to
  `bundle_dir`. Same state as today.
- **Externally packaged artifacts** (stored digest differs from `digestOf(content)`): unchanged. They are
  never packed, and the archive must still be placed in `bundle_dir` (`TestSignedBundleArtifactPath`
  keeps passing; only the failed-case message text changes).

## Security

- **Integrity.** Every fetched byte is bounded (`MaxBundleSize`), streamed to a `0600` temp file,
  checked against the digest and signature-verified by `bundle.Verify` before JSON parsing or
  publication. A bundle that fails is not cached. The digest in `DesiredState` arrives over the
  same mTLS session.
- **Authorisation.** `FetchArtifact` uses the mTLS identity (`peerKey`, as `connect` does) and
  serves only digests in that runtime's current desired state. Unknown and foreign digests get
  the same `NOT_FOUND`. A compromised runtime cannot download another runtime's flows.
- **Key handling.** `bundle-signing.key` lives in `secrets/` (`0700`), file mode `0600`, published
  atomically with create-once semantics, never logged; only the key id is logged. It is never committed, and tests derive
  keys in-process. The control plane reads it from a path, not from the env value itself, so the
  key does not appear in `/proc/<pid>/environ` or `ps e`. No rotation (D3). Replacing the key
  makes existing cached bundles fail verification; they are refetched.
- **What the signature proves.** In standalone, the signer and the verifier are the same install.
  The signature proves "packed by this install's control plane": it detects tampering of cached
  files by a party that can write `bundles/` but cannot read `secrets/`. It does not prove
  authorship (same statement as the runtime-bundle-dir spec).
- **Windows (D11).** #57 protects `secrets\` with a private DACL and owner checks.
  Phase 3 uses the same directory protection for `bundles\`. Bootstrap fails if it cannot
  establish or validate the required protection.
- **Cache content.** Bundles hold flows and connection definitions with `secret://` references,
  never secret values; the packer adds nothing secret.
- **Owner approval.** This is security work (signatures, secrets). Łukasz approved this spec on
  2026-11-24 (status `approved`). The implementation PR follows AGENTS.md security rules. No check is disabled or
  loosened.

## Step plan

Phases are independently shippable. Each step leaves `main` green.

### Phase 1: control plane packs, signs and serves (`core/control-plane`)

Exit: with a key configured, a TS client over mTLS can fetch an archive that Go `bundle.Verify`
accepts. Without a key, behaviour is byte-for-byte unchanged.

1. `bundle.ts`: `packBundle`, key loading, tar writer. Verified by `bundle.test.ts` (determinism,
   tar layout, two zero blocks, size cap, name rules), with
   `pnpm --filter @nurtuo/control-plane run test` plus `lint` and `typecheck`.
2. Cross-language golden. A fixed seed (derived in-test, for example
   `sha256("nurtuo-golden-bundle-key")`) and a fixed ASCII flow input give the same archive
   SHA-256 in `bundle.test.ts` (TS) and in `TestPackGoldenMatchesControlPlane` in
   `core/deploy/bundle/golden_test.go` (Go). Require a second shared golden with non-ASCII keys
   (including a supplementary-plane key), U+2028 and number-heavy content. Go `Verify` must
   accept both archives. No key material is committed.
3. `Gateway`: add the signing key option and artifact cache in the packer PR. Enable the
   `desiredStateMessage` digest switch in the delivery PR alongside `fetchArtifact`. Cover
   matching-generation reconnect and cache churn in `gateway.test.ts`.
4. `main.ts`: `NURTUO_BUNDLE_SIGNING_KEY_FILE`. Verified by a `main`-level test or a small
   exported `loadSigningKey(path)` unit test (invalid file → error that names the variable and
   contains no key bytes).

### Phase 2: runtime fetches in signed mode (`core/runtime/internal`)

Exit: a runtime with `bundle_dir` and `trusted_keys`, connected to a Phase-1 control plane with
the matching key, reaches `running` with an empty `bundle_dir`.

5. Characterization first: `TestSignedSourceMissingFileError` documents today's
   `no such file` error for signed mode. Green on unchanged code, then updated in step 7.
6. `gateway.Client.FetchArtifact` plus conn field. Verified by `client_test.go` cases against the
   bufconn `fakeServer` (which gains a `FetchArtifact` implementation).
7. `FetchingBundleSource` and `RunWith` wiring (signed mode only), plus `.fetch-*` cleanup.
   Verified by `fetch_test.go` and `TestRunWithFetchesSignedBundle`, run with
   `cd core/runtime && go test -race ./internal/runtime/... ./internal/gateway/...`.
8. Interop: `TestFetchArtifactSignedByControlPlane` and the failure cases against the real TS
   gateway. Verify locally with `go test -race ./internal/interop/...` (needs Node 20+ and the built
   control plane). The tests ship with #64. CI execution is tracked in [#89](https://github.com/nurtuo/nurtuo/issues/89).
9. Docs: `interop.md#artifacts`, the `operations.md` `trusted_keys` row and the distributed
   "enable signing" procedure. Verified by `node scripts/check-docs-links.mjs`.

### Phase 3: standalone D3 defaults (`core/runtime/cmd/nurtuo`), only after Phases 1 and 2 are on `main` (D2)

Exit: `nurtuo standalone` deploys reach `running` automatically when the data directories and
signing key pass D3 validation.

10. `DataDirs.Bundles` plus `Ensure` (`0700`, tighten validated existing directories and reject
    symlinks on Unix). Verified by
    `TestDataDirsEnsureBundles` in `standalone_test.go`.
11. `loadOrGenSigningKey`, plus `Credentials.BundleSigningKeyFile` and `BundlePublicKey`. Verified
    by an extended `TestLocalBootstrapIdempotentAndUsableForMTLS` and a new
    `TestBundleSigningKeyStableAndPrivate`. Add the unsafe-path, concurrent-bootstrap
    and publication-fallback tests listed below.
12. `controlPlaneEnv` plus `standaloneRuntimeConfig`. Verified by an extended
    `TestControlPlaneEnvWiresGatewayAndAuth` and a new `TestStandaloneRuntimeConfig`.
13. Flip `TestStandaloneE2EHandshakeAndDeploy` to expect `running` with a real flow and the
    archive cached in `bundles/`. Add the Unix legacy secrets-directory upgrade test below.
    With PostgreSQL, create a failed deployment with signing
    disabled, then upgrade and verify the same deployment reaches `running` without a new
    deploy POST. Restart again and check the same key, digest and untouched cache file.
    Remove `TODO(#64)`. Update `operations.md` (standalone limitation removed, data-dir table)
    and the release note. Verify locally with
    `cd core/runtime && go test -race ./cmd/nurtuo -run TestStandaloneE2E` and
    `node scripts/check-docs-links.mjs`. The test changes ship with #64. CI execution is tracked in
    [#89](https://github.com/nurtuo/nurtuo/issues/89).
14. Windows (D11): `Ensure` calls `secrets.EnsurePrivateDir(d.Bundles)` and propagates
    errors. Verify creation and repair of the private DACL in `TestDataDirsEnsureBundlesPrivate`.

## Acceptance criteria

- WHEN a user deploys a flow revision in `nurtuo standalone` on a fresh data dir THEN the
  deployment reaches `running` without anything being copied into `bundle_dir`, and
  `<data-dir>/bundles/<hex>.bundle` exists, where `<hex>` is the digest sent in `DesiredState`.
- WHEN the standalone and interop tests run against an empty `bundle_dir` THEN deployments
  reach `running` without an out-of-band copy step. Verify both suites locally with a built
  control plane until the CI job in [#89](https://github.com/nurtuo/nurtuo/issues/89) lands.
  The tests ship with #64 and may skip in the current `go` job.
- WHEN `nurtuo standalone` starts on a data dir THEN `bundles/` exists with mode `0700` and
  `secrets/bundle-signing.key` exists with mode `0600` (Unix); WHEN it starts again THEN the key
  file content and the runtime's `TrustedKeys` are unchanged and no second key is created.
- WHEN the Unix secrets directory has a mode other than `0700` or a foreign owner, or the
  existing key has a mode other than `0600` or a foreign owner, THEN signing-key bootstrap
  fails without using or replacing the key. Windows key loading rejects a DACL or owner
  outside #57's policy. On all supported platforms, bootstrap rejects a symlinked secrets directory
  or key (including Windows reparse points) without accessing the target.
- WHEN two signing-key bootstraps race on a private secrets directory with no key THEN both
  return the same public key, exactly one complete seed is published, and staged files are
  removed. WHEN no atomic no-replace primitive is available THEN bootstrap reports a
  filesystem error, removes the staged file and leaves the final path absent.
- WHEN Windows standalone creates `bundles\` or encounters a legacy directory with an
  ordinary-users read ACE THEN `Ensure` leaves it protected by #57's private DACL.
- WHEN an existing standalone install that showed `failed` ("no bundle_dir configured") is upgraded
  and started with the bundle and secrets directories and any existing signing key passing validation
  THEN, without operator action, those deployments move to `running`.
- WHEN a legacy Unix install has a loose or foreign-owned secrets directory containing an
  existing signing key THEN startup fails with the affected path and expected owner and mode,
  and the key bytes and metadata are preserved. After the operator fixes the directory and
  restarts, bootstrap reuses the same key.
- WHEN a legacy Unix install has a symlink at `bundles/` THEN startup rejects it and leaves
  the target directory mode and contents unchanged.
- WHEN the fetched bytes do not hash to the digest THEN the deployment is `failed` with
  "fetched artifact does not match digest" and nothing is left in `bundle_dir`.
- WHEN the fetched archive is signed by a key not in `trusted_keys` (or its manifest or a file was
  altered) THEN the deployment is `failed` with "bundle … rejected: bundle: signature verification
  failed" (or `content hash mismatch`), no flow runs, and the file is not kept.
- WHEN runtime `rt-2` requests a digest from `rt-1`'s desired state, or an unknown digest, THEN it
  gets `NOT_FOUND` with the same message in both cases.
- WHEN a `FetchArtifact` call has no runtime identity in its client certificate THEN it gets
  `UNAUTHENTICATED`; WHEN the digest is malformed THEN `INVALID_ARGUMENT`.
- WHEN the control plane has no signing key THEN `DesiredState` digests are identical to today's
  (revision digests) and `FetchArtifact` returns `FAILED_PRECONDITION`.
- WHEN the control plane restarts with the same key THEN the digest for an unchanged deployment is
  the same and the runtime does not download it again.
- WHEN a stream exceeds `bundle.MaxBundleSize` THEN the runtime aborts the fetch with "artifact
  larger than …", without holding the stream in memory, and caches nothing.
- WHEN a runtime has no `bundle_dir` THEN behaviour is exactly #71: WARN at start, every deployment
  `failed` "no bundle_dir configured", never `running`.
- WHEN a runtime has `bundle_dir` but no `trusted_keys` THEN it never calls `FetchArtifact`
  (unsigned path unchanged; #61 owns its verification).
- WHEN a stored revision digest differs from `digestOf(content)` THEN the control plane sends that
  digest unchanged and `TestSignedBundleArtifactPath` (manual archive in `bundle_dir`) still
  reaches `running`.

## Tests

Tests ship in the same PR as the code of each phase. Every test creates its own fixtures
(`t.TempDir()`, in-test key generation or seed derivation, in-memory repositories) and cleans up.
No key material is committed or printed.

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `core/control-plane/src/bundle.test.ts` | `packBundle`: same input → identical bytes; entry order; USTAR header fields (mode 0644, mtime 0, magic); archive ends with exactly 1024 zero bytes; manifest field order; `keyId` = first 16 hex of sha256(pub); size cap and `nameRe` rejection; golden SHA-256 equals the Go constant; key loading rejects bad hex or wrong length without echoing the value | `pnpm --filter @nurtuo/control-plane run test` | pure, no network |
| `TestPackGoldenMatchesControlPlane` in `core/deploy/bundle/golden_test.go` | Go `Pack` with the same seed and input matches both TS golden SHA-256 values using equivalent number spellings; `Verify` accepts both | `cd core/deploy/bundle && GOWORK=off go test ./...` | same constant as the TS test; a TS-produced archive fixture is optional (no keys in it) |
| `gateway.test.ts` (new cases) | with `signingKey`: `desiredStateMessage` sends the archive digest; `fetchArtifact` over real mTLS (`Rt` helper) streams chunks ≤ 1 MiB whose concatenation hashes to that digest and verifies (signature checked with `node:crypto`); `rt-2` asking for `rt-1`'s digest and an unknown digest get `NOT_FOUND` with identical text; malformed digest → `INVALID_ARGUMENT`; explicit digest equal to `digestOf(content)` → advertises the archive digest and fetch streams the signed archive; explicit digest differing from `digestOf(content)` → advertises the supplied digest unchanged and fetch → `NOT_FOUND`; restart (new `Gateway`, same key) → same digest; without key: digests unchanged and fetch → `FAILED_PRECONDITION` | same | existing test certs in `testdata/certs` |
| `client_test.go` (new cases) | `Client.FetchArtifact` against the bufconn `fakeServer` with `FetchArtifact`: multi-chunk copy; `limit` exceeded aborts; server error status propagated; call before `Run` → "not connected"; runs while `Connect` is active | `cd core/runtime && go test -race ./internal/gateway/...` | drain the stream while the server sends (lesson [ci-label-gates-and-duplex-grpc-tests](../lessons/ci-label-gates-and-duplex-grpc-tests.md)) |
| `core/runtime/internal/runtime/fetch_test.go` | `FetchingBundleSource` with a fake `FetchFunc` and a real `bundle.Pack` archive (key from `bundle.GenerateKey` in-test): cache hit makes no fetch; miss → fetch, rename, `<hex>.bundle` mode `0600`; digest mismatch → error, no file, no temp left; bad staged signature → error, prior file retained; verified fetch replaces an invalid existing file; oversize → error; invalid digest rejected before any fetch; `.fetch-*` leftovers cleaned | `cd core/runtime && go test -race ./internal/runtime -run Fetch` | self-contained |
| `TestSignedSourceMissingFileError` → `TestRunWithFetchesSignedBundle` in `internal/runtime/fetch_test.go` | characterization (step 5), then `RunWith` with `BundleDir` + `TrustedKeys` and an `Options.Gateway` bufconn fake that serves the archive: status `applying` → `running`; without `TrustedKeys` the fake's `FetchArtifact` is never called | same package | ctx cancel for cleanup |
| existing `TestNoBundleDirNeverRunning`, `TestRunWithoutBundleDirReportsError` (`placeholder_test.go`) | #71 unchanged | `go test -race ./internal/runtime/...` | regression |
| `TestFetchArtifactSignedByControlPlane` in `internal/interop/interop_test.go` | real TS gateway (`startControlPlane` with `extraEnv` `NURTUO_BUNDLE_SIGNING_KEY_FILE=<tempdir file>`), runtime trusting the matching public key, empty `bundle_dir`: deployment `running`, flow serving, archive cached; second deployment of a new revision fetched too | `cd core/runtime && go test -race ./internal/interop/...` | verify locally with Node 20+ and a built control plane. May skip in the current `go` job until [#89](https://github.com/nurtuo/nurtuo/issues/89) |
| `TestFetchArtifactWrongKeyRejected`, `TestFetchArtifactForeignDigestNotFound`, `TestFetchArtifactNoSigningKey` (interop) | runtime trusts another key → `failed` with "signature verification failed"; rt-2 fetching rt-1's digest → `NOT_FOUND`; control plane without key → `failed` with "no bundle signing key" | same | |
| `TestSignedBundleArtifactPath` (existing, message assertion updated) | externally supplied digest still works from `bundle_dir`; the missing case now carries the `NOT_FOUND` text | same | |
| `TestDataDirsEnsureBundles` in `cmd/nurtuo/standalone_test.go` | `bundles/` created `0700`; a pre-existing `0755` dir tightened to `0700`; a legacy symlink to a directory with a sentinel file is rejected and the target mode and contents stay unchanged (Unix) | `cd core/runtime && go test -race ./cmd/nurtuo -run TestDataDirs` | `t.TempDir()` |
| `TestBundleSigningKeyStableAndPrivate` (new) and `TestLocalBootstrapIdempotentAndUsableForMTLS` (extended) | key created `0600`; second `Bootstrap` returns the same public key and file bytes; corrupt file → error, file untouched; `bundle.DecodePrivateKey` accepts it; no key bytes in the error | `go test -race ./cmd/nurtuo -run 'Bootstrap|SigningKey'` | |
| `TestBundleSigningKeyRejectsUnsafePaths` (new) | loose or foreign-owned secrets dir, symlinked secrets dir or key (including a dangling link), key mode other than `0600`, foreign-owned key and non-regular key all fail without using or replacing the key. Windows exercises reparse points and invalid key ACL or ownership at the signing-key loader | `cd core/runtime && go test -race ./cmd/nurtuo -run SigningKey` | use a privileged fixture for foreign-owner cases when required |
| `TestStandaloneLegacySecretsDirUpgrade` (new, Unix) | a legacy `0755` secrets directory with a valid `0600` signing key fails startup with the directory path and expected owner and mode; the key bytes, mode and owner are preserved; after tightening the directory to `0700`, restart loads the same public key | `cd core/runtime && go test -race ./cmd/nurtuo -run TestStandaloneLegacySecretsDirUpgrade` | use a pre-upgrade layout |
| `TestBundleSigningKeyConcurrentBootstrap` (new) | overlap two bootstrap processes after both stage different seeds. Only one publishes, both return the same public key, the final file decodes and staged files are removed | same | exercise the native publisher on Linux, macOS and Windows |
| `TestBundleSigningKeyPublishFallback` (new) | simulate exclusive rename unavailable on Unix and verify hard-link publication. Simulate no hard-link support with native rename available and verify success. With all applicable primitives unavailable, verify the error, absent final file and temp cleanup | same | inject unsupported results at the publication boundary |
| `TestDataDirsEnsureBundlesPrivate` (new, Windows) | a fresh `bundles\` and a legacy directory with a `BUILTIN\Users` read ACE both end with #57's protected DACL | `go-windows` job | reuse #57's DACL assertions |
| `TestControlPlaneEnvWiresGatewayAndAuth` (extended), `TestStandaloneRuntimeConfig` (new) | env contains `NURTUO_BUNDLE_SIGNING_KEY_FILE=<secrets>/bundle-signing.key`; config has `BundleDir = <root>/bundles` and `TrustedKeys = [pub]` | `go test ./cmd/nurtuo -run 'ControlPlaneEnv|StandaloneRuntimeConfig'` | |
| `TestStandaloneE2EHandshakeAndDeploy` (modified) | full stack: a valid flow revision deploys to `running`; `bundles/<hex>.bundle` exists; restart the stack on the same data dir → same key, deployment `running` again, no second download (the file mtime is unchanged) | `cd core/runtime && go test -race ./cmd/nurtuo -run TestStandaloneE2E` | verify locally until [#89](https://github.com/nurtuo/nurtuo/issues/89). Skips with `-short`, without `node` or without the control-plane `dist`, including in the current `go` job if the build is absent |

## Decisions

Classification: **decided** = decided by Łukasz (restated, not reopened); **implementer-default** = reversible engineering default, does not block
approval.

| ID | Question | Options | Recommendation | Why | Class |
|---|---|---|---|---|---|
| D1 | Shape of `FetchArtifact` on gateway v1 | (a) existing server-streaming RPC as declared, no proto change; (b) add fields (size, offset, key id) | **(a)**: digest `sha256:<64 hex>` (bare hex accepted); chunks ≤ 1 MiB; client-side cap `bundle.MaxBundleSize`; mTLS identity; serve only digests in the caller's current desired state; `UNAUTHENTICATED` / `INVALID_ARGUMENT` / `NOT_FOUND` (unknown and foreign alike) / `FAILED_PRECONDITION` (no key, pack error) | existing contract is sufficient; no `buf breaking` impact; optional fields can be added later | implementer-default |
| D2 | Send/set standalone `BundleDir` / signing key before `FetchArtifact` exists? | — | **No.** Phase 1 of #58 is the honest placeholder only, no contract change; implemented by #71. Here: Phase 3 starts only after Phases 1–2 are on `main` | Łukasz, 2026-11-24 ([runtime-bundle-dir](2026-11-23-runtime-bundle-dir.md), #64 comment) | decided |
| D3 | Standalone bundle dir and key | — | **Yes:** `BundleDir` = `<data-dir>/bundles` mode `0700`; key at `secrets/bundle-signing.key` mode `0600`; reused after restart; no rotation | Łukasz, 2026-11-24 (same sources) | decided |
| D4 | Where and how the control plane serves artifacts | (a) pack on demand, deterministic, bounded in-memory cache; (b) pack at deploy and store in a new `artifacts` table; (c) build per request, no cache | **(a)**, same code for standalone and distributed; only revisions matching `digestOf(content)` are packed; other digests pass through | no migration, no REST change, restart-stable digests; (b) stays a follow-up if packing cost shows | implementer-default |
| D5 | How the control plane gets the signing key, and what `bundle_digest` means then | (a) new optional env `NURTUO_BUNDLE_SIGNING_KEY_FILE` (path to hex seed); with it, `DesiredState` carries the archive digest; without, unchanged; (b) key in DB / REST upload; (c) env with the key value | **(a)** | additive and opt-in; matches the proto comment "sha256 of the signed bundle"; path not value keeps the key out of process env listings; a new STABLE control-plane env key and a value change for opted-in installs, accepted by the owner | decided (Łukasz, 2026-11-24, 12:29 Europe/Warsaw, accepted recommendation) |
| D6 | Standalone deploys show `failed` until this lands | — | **Yes, accepted**; this issue is prioritised | Łukasz, 2026-11-24 | decided |
| D7 | Who signs, with what, where the packer lives | (a) control plane signs, TS port of `nurtuo.bundle/v1` writer in `core/control-plane/src/bundle.ts`, `node:crypto` Ed25519, cross-language golden + interop tests; (b) Go packer called by the control plane (sidecar/CLI); (c) runtime signs | **(a)**; key generated by the standalone bootstrapper (Go, `bundle.GenerateKey`, hex seed) and wired into `Config.TrustedKeys` by `runRealRuntime`; no rotation (D3) | ADR 0006 / bundle README say the control plane packs and signs; no new dependency or CLI; (b) adds a CLI surface and a Go binary to the control-plane image; (c) proves nothing. A second implementation of a FROZEN format is an architecture choice | decided (Łukasz, 2026-11-24, 12:29 Europe/Warsaw, accepted recommendation) |
| D8 | Composition with #61 | (a) signed-mode fetch only; unsigned path untouched; one shared digest-check helper reused by whichever lands second; (b) this spec also adds unsigned digest checks | **(a)** | keeps #61's files and scope intact; digest-then-signature order is the same in both paths; assumption about #61 stated in Design | implementer-default |
| D9 | Serve unsigned content over `FetchArtifact` to runtimes without `trusted_keys`? | (a) no, signed only in this issue (`FAILED_PRECONDITION` without key; unsigned runtimes do not fetch); (b) yes, serve `canonicalJson(content)` for derived digests, runtime verifies SHA-256 | **(a)** | lowest blast radius; nudges towards `trusted_keys` (#61 direction); (b) is a later additive change through the same seam | decided (Łukasz, 2026-11-24, 12:29 Europe/Warsaw, accepted recommendation) |
| D10 | Runtime fetch/cache behaviour | — | fetch in `Apply` when `<hex>.bundle` is missing or fails verification; temp file `.fetch-*` (`0600`) in `bundle_dir`, digest and signature checks, component decoding, fsync, rename; preserve the prior file on failure; no eviction; `Remove` keeps the file; 5 min per-fetch deadline; no new config key | reuses `SignedBundleSource` and the existing file layout; no data loss; reversible | implementer-default |
| D11 | Sequencing against #57 on Windows | ship independently and wire the DACL once #57 is available | #57 shipped in #85. Phase 3 calls `secrets.EnsurePrivateDir(<data-dir>\bundles)` | reuses the shipped Windows directory protection | decided (Łukasz, 2026-11-24, 12:29 Europe/Warsaw, accepted recommendation) |
| D12 | Existing standalone installs after upgrade | — | automatic when the secrets directory and any existing key pass validation: `Ensure` prepares `bundles/`, bootstrap loads or generates the key, desired state is resent on Hello, `failed` → `running` | no data migration; unsafe legacy Unix secrets directories require ownership or mode correction before restart; document in the release note | implementer-default |
| D13 | Split into more than one spec? | (a) one spec, three shippable phases (control plane, runtime, standalone); (b) separate specs per side | **(a)** | one capability (artifact delivery) end to end; phases ship independently; script modules split out (Non-goals) | implementer-default |
| D14 | Interaction with #62 | — | none required: content-addressed cache, component id as manifest id, no new flow-id-keyed state | keeps #62 unblocked | implementer-default (informational) |
| D15 | Interaction with #60 / PR #75 | — | no dependency; #75 is merged, so `ApplyDesiredState` runs off the receive loop and a long fetch does not block it (Gaps #3) | fetch is its own HTTP/2 stream | implementer-default (informational) |
| D16 | Bundle content per deployment | (a) one `flow` component = revision content, id = component id, name = component name (or id), `createdAt` = revision time; (b) expand `{"flows":[…]}` envelopes into several components | **(a)** | matches `desiredStateMessage` (one revision per deployment) and what the MCP store posts (`control-plane-store.ts`); envelopes are a follow-up (Gaps #6) | implementer-default |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | TS writer diverges from Go canonical JSON for stored values | medium | control plane / format | ASCII and Unicode/number-heavy shared golden digests and Go verification |
| 2 | The Windows bundle directory inherits a DACL that exposes flow definitions to other local users | high (multi-user Windows only) | security | Phase 3 step 14 applies #57's private DACL and tests creation and legacy directory repair |
| 3 | A 200 MiB bundle downloads inside `Apply`. With #75 (merged) this no longer runs on the receive goroutine; a long download only delays that deployment's apply | low | runtime | Phase 2 is built on #75; chunked stream keeps memory flat; check apply behaviour while a fetch is running |
| 4 | Transient fetch failure while connected leaves the deployment `failed` until the next desired state or reconnect (no retry timer) | medium | runtime / supervisor | existing supervisor behaviour; out of scope; candidate follow-up |
| 5 | Runtime restart while the control plane is down: nothing runs, even though archives are cached (no persisted desired state) | medium | runtime | pre-existing; cache only saves downloads; separate spec if needed |
| 6 | A revision whose content is a `{"flows":[…]}` envelope is packed as one `flow` component and fails `flow.Parse` on the runtime | low | control plane | clear `failed` message; envelope expansion as follow-up |
| 7 | Distributed operators have no tool to generate a key or print its public half | medium | operations | document an `openssl`/Go one-liner; a `nurtuo bundle keygen` CLI is a separate, Ask-First change |
| 8 | Enabling the signing key on a control plane with unsigned runtimes makes all their deployments fail | medium | compatibility | opt-in; documented order (runtimes first); D5 |
| 9 | Cache files are never evicted; months of revisions fill `bundle_dir` | low | runtime | eviction of digests not in any desired state after a grace period (follow-up) |
| 10 | Control-plane memory: many large deployments → artifact cache churn and repacking CPU | low | control plane | bounded cache (256 MiB default); D4 (b) persisted artifacts if measured |
| 11 | Component names longer than 128 chars do not match `nameRe` | low | control plane | fall back to the component id as the bundle name (D16) |
| 12 | Desired state changes between `DesiredState` delivery and `FetchArtifact` → `NOT_FOUND` | low | gateway | self-heals with the already queued new desired state |

## Improvement opportunities

- Report the verified key id and manifest hash in `DeploymentStatus.message` (audit trail).
- Persist artifacts (D4 b) to make packing a one-time cost and to allow serving after a revision is
  edited elsewhere.
- Prefetch artifacts when `DesiredState` arrives, before `Apply`.
- A startup self-check in standalone that prints the bundle signing key id.

## References

- Issue #64 and its comment (D2, D3, D6); issue #58; PR #71 (phase 1); PR #70 (approved
  runtime-bundle-dir spec).
- [runtime-gateway-protocol-v1](implemented/2026-11-21-runtime-gateway-protocol-v1.md) gap 6;
  [runtime-supervisor-and-deployer](implemented/2026-11-21-runtime-supervisor-and-deployer.md);
  [runtime-cli-service-standalone](implemented/2026-11-21-runtime-cli-service-standalone.md);
  [deployment-bundles](implemented/2026-11-21-deployment-bundles.md);
  [backward-compatibility-contract](2026-11-22-backward-compatibility-contract.md) (control plane env
  and standalone layout classes).
- Superseded draft: [2026-11-22-bundle-artifact-delivery](2026-11-22-bundle-artifact-delivery.md).
- #57 spec ([windows-secrets-dacl](implemented/2026-11-24-windows-secrets-dacl.md); implementation landed in [#85](https://github.com/nurtuo/nurtuo/pull/85)), only for the
  `EnsurePrivateDir` dependency; wiring that call is this issue's step 14, not the DACL work itself.
- Open Mercato: `code-workflow-version-pinning` (ship exactly the pinned definition that runs).

## Changelog

- 2026-11-24: created as draft.
- 2026-11-24: approved; Łukasz accepted D5, D7, D9, D11 (TS packer, opt-in NURTUO_BUNDLE_SIGNING_KEY_FILE, signed-only FetchArtifact, do not block Windows on #57). D2, D3, D6 unchanged. Merged `main` (#75 landed; #57 spec approved in #73).
- 2026-11-24: merged `main` after #85; retargeted the #57 spec link to `docs/specs/implemented/`.
- 2026-11-24: after Recenzent's major on #79, moved CI coverage to [#89](https://github.com/nurtuo/nurtuo/issues/89) and specified local verification in the interim.
