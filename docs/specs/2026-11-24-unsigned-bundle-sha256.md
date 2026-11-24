<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Verify the SHA-256 of unsigned bundles and warn when `trusted_keys` is empty

Status: draft. Date: 2026-11-24.

Extends: [runtime-supervisor-and-deployer](implemented/2026-11-21-runtime-supervisor-and-deployer.md) (gap 3). Related: [runtime bundle_dir](2026-11-23-runtime-bundle-dir.md) (approved, phase 1 on `main`), [bundle-artifact-delivery](2026-11-22-bundle-artifact-delivery.md) (draft, issue #64).

## TLDR

With `bundle_dir` set and `trusted_keys` empty, `DirBundleSource` reads `<bundle_dir>/<digest>.json` and runs it without checking that the bytes hash to the digest, so anyone who can write to `bundle_dir` decides what runs. This spec makes the unsigned path **fail closed on a SHA-256 mismatch** (same check `SignedBundleSource` already does), logs a **start-up warning** when `bundle_dir` is set but `trusted_keys` is empty, and (Phase 2, pending owner decision D3) makes `service install` refuse an unsigned configuration unless the operator opts out explicitly. Phase 1 needs no contract change. Making unsigned bundles refused by default at runtime start stays with #64.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/76 (this spec; docs only). Implementation will use `Closes #61` on a later `feat/` or `fix/` PR after approval |
| ADRs | [ADR 0006](../adr/0006-bundle-format-and-domain-separated-signature.md) (signed bundles, hash pinning), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) (the digest arrives over the authenticated gateway) |
| Issues | https://github.com/nurtuo/nurtuo/issues/61. Boundaries: #64 (`FetchArtifact`, standalone `BundleDir`, signing key), #59 and #60 (parallel work), #62 (follows this one, same parent spec), #67 (tracker) |
| Code | `core/runtime/internal/runtime/deployer.go` (`BundleSource`, `DirBundleSource`, `SignedBundleSource`, `EngineDeployer.Apply`), `core/runtime/internal/runtime/run.go` (`RunWith`, `unconfiguredDeployer`), `core/runtime/internal/runtime/config.go` (`Config.BundleDir`, `Config.TrustedKeys`, `Validate`, `trustedKeys`), `core/runtime/cmd/nurtuo/service.go` (`runServiceAction`, case `"install"`), `core/control-plane/src/api.ts` (`digestOf`, `canonicalJson`, `POST /v1/components/{id}/revisions` with optional `digest`) |
| User docs | [operations.md](../operations.md) (`runtime.yaml` table, `bundle_dir` paragraph), [security.md](../security.md) (bundle trust boundary row), [interop.md](../interop.md) (Artifacts) |

## Goal

Operators who run a runtime with a plain `bundle_dir` (no `trusted_keys`) today get no integrity guarantee: a local user, a compromised sync job or a stray copy can replace `<digest>.json` and the runtime runs the new content under the old digest. Proposed: the runtime always checks that the bytes it runs hash to the digest the control plane sent, rejects the deployment otherwise, and tells the operator at start (and, after D3, at `service install`) that without `trusted_keys` bundle authenticity rests on the control plane alone. This is a security fix scoped to the unsigned read path and the policy around an empty `trusted_keys`.

## Open Questions

Blocking (owner decisions; see Decisions for options and recommendations):

- **Q1 (D2):** With `bundle_dir` set and `trusted_keys` empty, should the runtime (a) warn and still run SHA-256-verified unsigned bundles, (b) warn and refuse all bundles, (c) fail start, or (d) fail closed only under `service`? Recommendation: (a) now; the refuse-by-default flip ships with #64.
- **Q2 (D3):** What does "make `trusted_keys` the default for `service install`" mean? Recommendation: install refuses a config with `bundle_dir` set and `trusted_keys` empty unless `allow_unsigned_bundles: true` is set in `runtime.yaml` (a new key, shared with #64's draft); no key generation.

Non-blocking (implementer defaults, listed so the owner can override):

- **Q3 (D7):** The digest of a plain bundle covers the raw file bytes, not a re-canonicalised JSON form. Producers must write exactly the bytes they hashed (for control-plane-computed digests: `canonicalJson(content)`), or post the revision with an explicit `digest`.
- **Q4 (D12):** One spec with two phases (Phase 1 runtime verify + warn; Phase 2 install policy), not two specs.

## Scope

In scope:

- SHA-256 verification of unsigned `<bundle_dir>/<hex>.json` bundles against the deployment's `bundle_digest`, fail closed.
- Strict digest syntax on the unsigned path (optional `sha256:` prefix, then exactly 64 lowercase hex characters), shared with the signed path.
- A start-up `WARN` when `bundle_dir` is set and `trusted_keys` is empty.
- Phase 2 (after D3): the `service install` policy for an empty `trusted_keys`.
- Migrating the tests that today write plain bundles under names that are not their SHA-256 (`internal/e2e`, `internal/interop`).
- Doc updates (`operations.md`, `security.md`, `interop.md`) and a release note.

Non-goals:

- `FetchArtifact`, a bundle cache, standalone `BundleDir`, generating, storing or rotating a signing key, delivering signed artifacts (all #64).
- Refusing unsigned bundles by default at runtime start (the `allow_unsigned_bundles` runtime switch in the #64 draft). This spec only reserves the key name for the install check if D3 is accepted.
- Exiting the OS service when start fails (#59); moving `ApplyDesiredState` off the receive loop (#60); keying flows by `<deployment>/<flow>` (#62).
- Changing the signed path (`SignedBundleSource`, `bundle.Verify`) beyond sharing the digest helpers.
- File permissions or ACLs on `bundle_dir` (Windows DACL work is #57 and covers the standalone secrets directory only).
- Changing #58 phase 1: with `bundle_dir` unset the runtime keeps using `unconfiguredDeployer` and reports `error`.
- A size cap on plain bundle reads (see Gaps and risks 5).

## Design

### Current behavior (as the code works on `main` at `7953d4f`)

- **Source selection** (`RunWith`, `run.go` lines 79 to 127): an injected `Options.Deployer` wins. Otherwise, with `cfg.BundleDir != ""`, `src := DirBundleSource(cfg.BundleDir)`; if `cfg.trustedKeys()` returns at least one key, `src = SignedBundleSource(cfg.BundleDir, keys)`. With `BundleDir == ""` it logs `bundle_dir is not set; deployments will be reported as error` and uses `newUnconfiguredDeployer()` (#58 phase 1).
- **Unsigned path** (`DirBundleSource`, `deployer.go` lines 35 to 43): strips an optional `sha256:` prefix, rejects an empty name, `/`, `\` and `..` with `deployer: invalid bundle digest %q`, then returns `os.ReadFile(<dir>/<name>.json)`. No hash is computed. Any other string (for example `d1`, `ABC`, `sha256:abc`) is a valid name.
- **Signed path** (`SignedBundleSource`, lines 49 to 86): requires exactly 64 lowercase hex characters, reads `<dir>/<hex>.bundle`, compares `hex(sha256(data))` with the name (`deployer: bundle content does not match digest %s`), then `bundle.Verify` against each key (`deployer: bundle %s rejected: ...`; with no keys `deployer: no trusted keys configured`), then re-encodes the flow and connection components as `{"flows":[...],"connections":[...]}`. Plain `.json` files are never read on this path.
- **Apply** (`EngineDeployer.Apply`): wraps any source error as `deployer: fetch bundle <digest>: <err>`, then `ParseBundle`. The supervisor reports `error` with that message and does not advance the revision; nothing is started for a failed `Apply`.
- **Config** (`config.go`): `trusted_keys` is optional; `Validate` decodes every key (`bundle.DecodePublicKey`) and rejects `trusted_keys` without `bundle_dir`. No warning exists for an empty `trusted_keys` (searched `core/runtime` for `trusted_keys`: only the config comment and the errors from `Validate` and `trustedKeys()`, no log line).
- **Digest origin** (`core/control-plane/src/api.ts`): a revision's digest is `sha256:` + hex SHA-256 of `canonicalJson(content)` (sorted keys, no whitespace), unless the client posts an explicit `digest` matching `^sha256:[0-9a-f]{64}$`. The gateway sends it unchanged as `Deployment.bundle_digest` (`gateway.ts`). Nothing in the repository writes `<digest>.json` files; operators place them out of band.
- **`service install`** (`service.go` `runServiceAction`, case `"install"`, line 178): calls `runtime.LoadConfig(cfgPath)` and refuses with `refusing to install with invalid config: ...` if it fails, then `svc.Install()`. It writes no config and generates no keys. `service run` loads the config again on every start and calls `RunWith`.

**Tests that rely on the gap** (they write plain bundles whose bytes do not hash to the name):

- `internal/e2e/harness_test.go` `putJSON` returns `"sha256:" + name` for names like `d1` (used by `TestChain/json-bundle`).
- `internal/e2e/realwiring_test.go` `putWith(t, false, ...)` returns `"sha256:" + name` for `b1`, `w1` (used by `TestRealWiringHTTPConnector/json-bundle`, `TestRealWiringTestModeWarnAndEncryptedFileSecrets`).
- `internal/interop/harness_test.go` `putPlain` writes `json.Marshal({"flows":[doc]})` under the control plane's digest, which is the hash of `canonicalJson(doc)`, a different byte string (used by `TestRuntimeAgainstTSGateway`, `TestWrongIdentityRejected`, `TestCertificateIdentities`).

Tests that use custom `BundleSource` funcs (`deployer_test.go`, `signed_test.go` `TestPausedListenerSurvivesSiblingApply`) or no source at all (`placeholder_test.go`, which uses `unconfiguredDeployer`) do not go through `DirBundleSource` and are unaffected. `TestSignedBundleRejections` (`internal/e2e/chain_test.go`) writes a plain `<64×a>.json` but runs in `trusted_keys` mode, so `DirBundleSource` never reads it; also unaffected.

### Drift between prior specs and the code

| Where | Spec or comment says | Code does | Intentional? |
|---|---|---|---|
| supervisor spec, TLDR and Wiring | "Without `bundle_dir` the runtime silently uses an in-memory placeholder" (`newMemDeployer`) | `unconfiguredDeployer` reports `error` (#58 phase 1, `run.go`) | Yes, already recorded by the bundle-dir spec; design is against the code |
| `deployer.go` `BundleSource` doc comment | "Signature verification and unpacking of signed .nurtuo bundles is not done here yet: the source is expected to hand over already verified content" | `SignedBundleSource` verifies digest and signature | Stale comment; fix it in Phase 1 step 5 |
| `docs/interop.md` Artifacts | plain bundle is `{"flows":[...]}` | `ParseBundle` also accepts one bare flow document (as `operations.md` says) | Doc imprecision; fix with the Phase 1 doc update |
| #64 draft `bundle-artifact-delivery` | "with no keys [the runtime] refuses to start unless `allow_unsigned_bundles: true`" | no such key or check exists | Draft, not implemented; this spec does not implement the runtime part (see D2) |

### Proposed approach

**Phase 1: verify unsigned bundles, warn on empty `trusted_keys` (runtime only, no contract change).**

1. New file `core/runtime/internal/runtime/digest.go` (same package, no new package) with two unexported helpers:
   - `digestHex(digest string) (string, error)`: strip one optional `sha256:` prefix; require `len == 64` and only `0-9a-f`; otherwise `fmt.Errorf("deployer: invalid bundle digest %q", digest)`. No case folding.
   - `checkSHA256(data []byte, hexDigest, digest string) error`: `hex.EncodeToString(sha256.Sum256(data))` compared with `hexDigest` (plain string compare is fine: both are public values); on mismatch `fmt.Errorf("deployer: bundle content does not match digest %s", digest)`.
2. `SignedBundleSource` uses both helpers in place of its inline checks. Same order (syntax, read, digest, signature, re-encode), same messages: a refactor with no behavior change.
3. `DirBundleSource`: `name, err := digestHex(digest)`; `data, err := os.ReadFile(filepath.Join(dir, name+".json"))`; `checkSHA256(data, name, digest)`; return `data`. The bytes that are hashed are the bytes returned and parsed (one `ReadFile`, one buffer), so there is no time-of-check/time-of-use window between hash and use.
4. `RunWith`: after choosing `DirBundleSource` (keys empty, `BundleDir` set), log once:
   `log.Warn("trusted_keys is empty: unsigned bundles are accepted after a SHA-256 check only; their authenticity rests on the control plane. Set trusted_keys to require signed bundles", "bundle_dir", cfg.BundleDir)`.
   Under `service run` the existing `slogWriter` maps `level=WARN` to the OS logger's `Warning`. No warning is added when `BundleDir` is empty (the existing `bundle_dir is not set` warning stays the only one; `Validate` already rejects keys without a dir). No warning with an injected `Options.Deployer`.

What the operator sees after Phase 1:

| Situation | Reported state and message |
|---|---|
| `<hex>.json` bytes hash to the digest | unchanged: `running` |
| `<hex>.json` bytes differ (tampered, pretty-printed, re-encoded, wrong file) | `error`, `deployer: fetch bundle sha256:<hex>: deployer: bundle content does not match digest sha256:<hex>`; nothing starts; revision not advanced, so the control plane resends on the next handshake |
| digest is not 64 lowercase hex (for example `sha256:ABC…`, `d1`) | `error`, `... invalid bundle digest "..."`; the file is not opened |
| file missing | unchanged: `error`, `... no such file or directory` |
| `trusted_keys` set | unchanged (signed path) |
| `bundle_dir` unset | unchanged (#58 phase 1): `error`, `no bundle_dir configured ...` |
| start with `bundle_dir` set and no keys | one `WARN` line naming `trusted_keys`; the runtime connects and runs verified bundles |

**Phase 2: `service install` policy (blocked on D3; recommended option shown, pending owner confirmation).**

5. A pure function in `core/runtime/cmd/nurtuo/service.go`, for example `installPolicy(cfg *runtime.Config) (warning string, err error)`, called from case `"install"` after `LoadConfig` and before `svc.Install()`:
   - `bundle_dir` empty: no error (the runtime will report `error` for every deployment; #58 phase 1 already warns at start).
   - `bundle_dir` set and `trusted_keys` non-empty: no error.
   - `bundle_dir` set, `trusted_keys` empty, `allow_unsigned_bundles` not true: `refusing to install: bundle_dir is set but trusted_keys is empty; add the public key(s) that sign your bundles to trusted_keys, or set allow_unsigned_bundles: true to accept SHA-256-verified unsigned bundles`. Exit code 1 (existing path in `serviceMain`).
   - same with `allow_unsigned_bundles: true`: install proceeds and prints a warning line to stdout.
   - Recommended D3 option needs a new `runtime.yaml` key `allow_unsigned_bundles` (bool, default false, `Config.AllowUnsignedBundles`). In Phase 2 only `service install` reads it; #64 can later make `RunWith` honour it at start, so there is one opt-out, not two. If the owner prefers no new key, the fallback is warn-only: install prints the same text as a warning and succeeds (no contract change).
   - No key is generated, read beyond the public `trusted_keys` hex strings, or printed. The signing key belongs to whoever packages bundles (`core/deploy/bundle` `Sign`), not to the runtime host; standalone's install-local key is #64 (D3 of the bundle-dir spec).
   - Existing installed services are not affected: install is not re-run on upgrade and `service run` does not apply the policy.

### How the checks compose with signed bundles and #64

- Always: the bytes that will be parsed and run are hashed and must equal the deployment's digest (`checkSHA256`). This is already true for `SignedBundleSource`; Phase 1 makes it true for `DirBundleSource`.
- With keys: additionally `bundle.Verify` must pass for one trusted key, and `.json` files are never read (unchanged).
- #64's draft `GatewayBundleSource` (fetch, size cap, digest compare, then `bundle.Verify`) can call the same `digestHex`/`checkSHA256` helpers; nothing in this spec adds a second hash step or a bypass for #64 to remove. If #64 flips the default to "refuse unsigned unless `allow_unsigned_bundles`", the Phase 1 warning becomes the message logged when the opt-out is used, and the Phase 2 key (if accepted) is the same key.

### Alternatives considered

| Option | Why not (for now) |
|---|---|
| Re-canonicalise the parsed JSON in Go and hash that, to match `canonicalJson` | Parses untrusted bytes before the integrity check; JS `JSON.stringify` and Go `encoding/json` differ in number and escape output (`<`, `>`, `&`, U+2028), so a cross-language canonicaliser is a new mismatch source; `SignedBundleSource` and #64 hash raw bytes. Kept as an owner override of D7 |
| Warn on mismatch and run anyway | Leaves the reported hole open; no operator benefit |
| Fail runtime start on empty `trusted_keys` now | Breaks every runtime-only install and test that uses plain bundles, needs a new opt-out key at runtime, and under the OS service a start error is only logged until #59 lands (`svc.Run` keeps the process alive). This is #64's default flip |
| Generate a keypair in `service install` | The runtime host is the verifier, not the signer; a host-generated key would sign nothing. Key lifecycle is #64 |

### Failure modes and rollback

- A producer that writes non-canonical bytes sees every plain deployment go `failed` with "does not match digest" after upgrade. Mitigation: release note, doc change with the exact rule, and the explicit-`digest` revision path.
- Rollback of Phase 1: revert the commit; no data or config changes. Rollback of Phase 2: revert; an `allow_unsigned_bundles` key left in a config is then silently ignored by `yaml.Unmarshal` (unknown keys are ignored today).

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `<bundle_dir>/<hex>.json` (unsigned layout) | Phase 1: the name must be the 64-lowercase-hex SHA-256 of the file bytes; other names and non-matching content are rejected | experimental (supervisor spec contracts table) |
| Unsigned bundle content (one flow document or `{"flows":[...],"connections":[...]}`) | none | experimental |
| `<bundle_dir>/<hex>.bundle`, `nurtuo.bundle/v1` | none | frozen / experimental as today |
| `Deployment.bundle_digest` (`gateway.proto`) | none; format already `sha256:<64 hex>` from the control plane | additive-only |
| `runtime.yaml` `bundle_dir`, `trusted_keys` | none | stable |
| `runtime.yaml` `allow_unsigned_bundles` | Phase 2, only if D3 is accepted as recommended: new optional bool, default false | new, stable once released (Ask First) |
| `nurtuo service install` behavior and exit code | Phase 2: exit 1 for a config that was accepted before (bundle_dir set, keys empty, no opt-out) | stable (Ask First, D3) |
| CLI flags, REST API, proto | none | |
| Log line `trusted_keys is empty: ...` | new WARN text; not a contract | human-readable |

## Compatibility

- Phase 1 changes no stable contract. It narrows an experimental file layout: plain bundles whose name is not their SHA-256 stop running. Who is affected: operators placing `<digest>.json` files whose bytes differ from what the digest was computed over (for example pretty-printed or wrapped `{"flows":[...]}` files under a control-plane-computed digest). Migration: write exactly `canonicalJson(content)` bytes, or post the revision with `digest` = `sha256:` + SHA-256 of the file and place the file under that name, or move to signed bundles with `trusted_keys`. Announce in the release note and in `operations.md`.
- Phase 2 (recommended D3) is a behavior change of a stable command: re-running `service install` with an unchanged unsigned config fails until the operator adds keys or `allow_unsigned_bundles: true`. Running services are untouched.

## Security

- Threat closed by Phase 1: a local writer (user, sync job, backup restore) substitutes content for a digest the control plane deployed. After Phase 1 substitution only produces `error` (availability), not execution.
- Residual without `trusted_keys`: the control plane is the trust root for content. Anyone who can create a revision (with an explicit `digest`) or deploy through the control plane, or who compromises it, can make a runtime run a file they placed in `bundle_dir`. Signatures (`trusted_keys`) remove that dependency. The start-up warning and Phase 2 make this visible.
- Plain-bundle digests are public values; constant-time comparison is not needed.
- Nothing in this spec reads, generates or logs a private key. Test fixtures that sign bundles keep generating keys in memory (`ed25519.GenerateKey`) as today.

## Decisions

| ID | Question | Options | Recommendation | Why | Class |
|---|---|---|---|---|---|
| D1 | Unsigned bundle whose bytes do not SHA-256 to the digest | reject (fail closed) / warn and run | **Reject; report `error`, start nothing** | Closes the reported hole; same rule as `SignedBundleSource`; the unsigned layout is `experimental`, so no stable contract breaks (release note) | implementer-default |
| D2 | Policy when `bundle_dir` is set and `trusted_keys` is empty | (a) warn, run SHA-verified unsigned bundles; (b) warn, refuse all bundles; (c) fail start; (d) fail closed only under `service` | **(a) now**, with the D3 install gate; the refuse-by-default flip with an `allow_unsigned_bundles` opt-out ships with #64 | (a) leaves authenticity resting on the control plane (CP or revision-writer compromise can point a digest at a planted file). (b)/(c) break every current plain-bundle install and test and need an opt-out key; (c) is invisible under the OS service until #59. (d) breaks the same service installs and adds a mode switch `RunWith` does not have. (a) is the tightest option with no contract change and is strictly stronger than today | needs-owner |
| D3 | "Make `trusted_keys` the default for `service install`" | (i) refuse unless `--allow-unsigned-bundles` flag; (ii) refuse unless `allow_unsigned_bundles: true` in `runtime.yaml`; (iii) warn only; (iv) generate a keypair; (v) refuse, no opt-out | **(ii)**; Design fallback if declined: (iii) | Install writes no config, so "default" can only mean "required unless opted out". (ii) gives one opt-out that #64 reuses at start; (i) adds a flag that becomes redundant; (iv) invents key lifecycle that belongs to #64 and the runtime is not the signer; (v) blocks legitimate dev installs. Changes stable install behavior and adds a config key | needs-owner |
| D4 | Signed bundles and #64 | add SHA-256 to unsigned only / reject unsigned whenever keys exist / both | **Always hash the bytes that run; with keys also require a valid signature and never read `.json`** (the code already does the keyed part; do not regress). Shared helpers in `digest.go` for #64 | One digest check per source, no bypass; #64 composes without dual checks | implementer-default |
| D5 | `bundle_dir` unset | apply checks / no-op | **No change**: `unconfiguredDeployer` stays; only the existing `bundle_dir is not set` warning fires; no `trusted_keys` warning (`Validate` rejects keys without a dir) | Does not touch #58 phase 1 | implementer-default |
| D6 | Config and contract surface | — | **Phase 1: none.** Field names `bundle_dir`, `trusted_keys` unchanged; file `<64 lowercase hex>.json`; optional `sha256:` prefix on `bundle_digest`; hash = SHA-256, lowercase hex. Phase 2 adds `allow_unsigned_bundles` only under D3 | Lowest blast radius | implementer-default (Phase 2 key covered by D3) |
| D7 | What bytes the digest covers | raw file bytes / Go re-canonicalised JSON / either | **Raw file bytes** | Matches the signed path and #64; no parsing before verification; no cross-language canonicaliser. Producers write `canonicalJson(content)` or post an explicit `digest` | implementer-default |
| D8 | Digest syntax and case | keep loose names / 64 lowercase hex, no folding | **64 lowercase hex, optional `sha256:` prefix, no case folding** | A non-hex name cannot be verified; the control plane emits lowercase; on case-insensitive file systems (Windows, macOS) an upper-case file name still opens but its content must hash to the lowercase digest, so case cannot be used to swap content | implementer-default |
| D9 | TOCTOU and caching | hash then reopen / hash the buffer that is parsed; cache verified digests | **Hash the single `ReadFile` buffer that is returned; no cache** | No window between check and use; the supervisor applies only on change, so the hash runs once per apply | implementer-default |
| D10 | Observability | log only / status message / metric | **Status message (existing `error` path) plus one start-up `WARN`; no metric** | The runtime package has no metrics today; the status reaches the UI | implementer-default |
| D11 | Tests that assert the broken behavior | keep / migrate | **Migrate in the same PR**: `putJSON`, `putWith(false)`, `putPlain` return the real SHA-256 of the written bytes; interop posts revisions with explicit `digest` | Otherwise e2e and interop go red; also proves the control-plane path | implementer-default |
| D12 | Split the spec | one spec, two phases / two specs | **One spec, two phases**; Phase 1 can ship without Phase 2 | Phase 1 strictly strengthens security and needs no owner contract decision | implementer-default |
| D13 | Permissions on `bundle_dir` | add checks / out of scope | **Out of scope**; document "keep `bundle_dir` writable only by the service account" | After Phase 1 a writer can only cause `error`; Windows DACL work is #57 (standalone secrets) | implementer-default |

## Step plan

### Phase 1: verify unsigned bundles and warn on empty `trusted_keys` (no contract change)

1. Characterization test on unchanged code: `TestDirBundleSourceAcceptsMismatchedContent` in `core/runtime/internal/runtime/plain_test.go` writes `<64 hex>.json` with content that does not hash to the name and asserts today's `DirBundleSource` returns it. Verified: `cd core/runtime && go test -race ./internal/runtime -run TestDirBundleSource` is green.
2. Add `digest.go` (`digestHex`, `checkSHA256`) and switch `SignedBundleSource` to it, no behavior change. Verified: `TestSignedBundleSource`, `TestConfigTrustedKeys` and `internal/e2e` `TestSignedBundleRejections` pass unmodified (`cd core/runtime && go test -race ./internal/runtime/... ./internal/e2e/...`).
3. `DirBundleSource` uses the helpers; invert step 1 into `TestDirBundleSourceRejectsDigestMismatch`; add the syntax and deployer tests below; migrate `putJSON`, `putWith(t, false, ...)` and interop `putPlain`/`component` to real digests; add `TestPlainBundleTamperedRejected` (e2e). Verified: `cd core/runtime && go test -race ./internal/runtime/... ./internal/e2e/...` and `go test ./internal/interop/...` (skips without `node` or the control-plane `dist`; run it with them once before merge).
4. `RunWith` start-up warning plus `TestRunWarnsWhenTrustedKeysEmpty`. Verified: `cd core/runtime && go test -race ./internal/runtime -run TestRunWarns`.
5. Docs and comments: `operations.md` (`bundle_dir`/`trusted_keys` rows, the "With `bundle_dir` set" paragraph: name = SHA-256 of the bytes, how to produce them, the warning), `security.md` (bundle trust boundary row and "What is not covered"), `interop.md` (Artifacts), the `BundleSource` doc comment in `deployer.go`; release note for the runtime binary in the PR. Verified: `node scripts/check-docs-links.mjs`; `cd core/runtime && go vet ./...`.

### Phase 2: `service install` policy (starts only after D3 is decided)

6. Add `installPolicy` (and, under D3 (ii), `Config.AllowUnsignedBundles` with yaml/json tag `allow_unsigned_bundles`) with `TestInstallPolicy`. Verified: `cd core/runtime && go test -race ./cmd/nurtuo -run TestInstallPolicy` and `./internal/runtime -run TestLoadConfig`.
7. Call it from case `"install"` before `svc.Install()`; print the warning to stdout; docs: `operations.md` service install section and `runtime.yaml` table. Verified: `TestInstallPolicy`, `TestDispatchServiceErrors` still green; manual `nurtuo service install --config <unsigned.yaml>` on a Linux VM prints the refusal and exits 1 without registering a unit (`systemctl status nurtuo` → not found).

## Acceptance criteria

- WHEN a deployment's `bundle_digest` is `sha256:<H>`, `trusted_keys` is empty and `<bundle_dir>/<H>.json` contains bytes whose SHA-256 is not `<H>` THEN `Apply` fails, the reported state is `error` with a message containing `does not match digest`, no listener is started for it, and the applied revision does not advance.
- WHEN the same file's bytes hash to `<H>` THEN the deployment is applied and reported `running` as today.
- WHEN a plain bundle file is overwritten with different content after its digest was issued THEN every later `Apply` that resolves that digest (another deployment, or the same one after a runtime restart) is reported `error`, and a deployment that is already running is not changed (the supervisor does not re-apply an unchanged digest, and a failed `Apply` leaves the previous flows and listeners in place).
- WHEN `bundle_digest` (after the optional `sha256:` prefix) is not exactly 64 lowercase hex characters THEN the deployment is reported `error` with `invalid bundle digest` and no file is opened.
- WHEN the runtime starts with `bundle_dir` set and `trusted_keys` empty THEN exactly one `WARN` line containing `trusted_keys is empty` is logged and the runtime still connects and serves verified bundles.
- WHEN the runtime starts with `trusted_keys` set, with `bundle_dir` unset, or with an injected `Options.Deployer` THEN that warning is not logged (with `bundle_dir` unset the existing `bundle_dir is not set` warning is still logged and deployments are still `error`).
- WHEN `trusted_keys` is set THEN behavior is unchanged: `.json` files are never read, a signed bundle is accepted only if its digest and signature verify (`TestSignedBundleSource`, `TestSignedBundleRejections` pass unmodified).
- (Phase 2, D3 as recommended) WHEN `nurtuo service install` runs with a config where `bundle_dir` is set, `trusted_keys` is empty and `allow_unsigned_bundles` is not `true` THEN it exits 1 with a message naming `trusted_keys` and `allow_unsigned_bundles` and does not register the service.
- (Phase 2) WHEN the config has `trusted_keys`, or `allow_unsigned_bundles: true`, or no `bundle_dir` THEN install proceeds as today (with a warning line for `allow_unsigned_bundles: true`).

## Tests

Tests ship in the same change as the code. Unit tests use `t.TempDir()` and in-memory keys; nothing touches the network except the existing e2e/interop harnesses.

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestDirBundleSourceAcceptsMismatchedContent` → `TestDirBundleSourceRejectsDigestMismatch` in `core/runtime/internal/runtime/plain_test.go` (new) | characterization first, then: content `{"flows":[<valid flow>]}` written under its real SHA-256 is returned byte-identical (with and without `sha256:`); one byte flipped → error containing `does not match digest` and the digest | `cd core/runtime && go test -race ./internal/runtime -run TestDirBundleSource` | inverted in the same PR |
| `TestDirBundleSourceRejectsInvalidDigest` in `plain_test.go` | table: `""`, `sha256:`, `abc`, 63 and 65 hex chars, upper-case hex of a real digest, `../<64 hex>`, `a/b`, `a\b`, non-hex 64 chars → `invalid bundle digest`; place a file with the upper-case name in the dir to prove it is not read | same | case-insensitive FS still rejected by syntax first |
| `TestEngineDeployerRejectsMismatchedPlainBundle` in `plain_test.go` | `NewEngineDeployer(engine.New(), memory.New(), DirBundleSource(dir))` with an HTTP-start flow whose file does not match: `Apply` errors, `Actual` is empty, `Manager()` has no listeners; through `supervisor.New(dep).Reconcile` with a collecting reporter the statuses are `applying` then `error`, never `running` | same package | mirrors `TestNoBundleDirNeverRunning` |
| `TestRunWarnsWhenTrustedKeysEmpty` in `plain_test.go` | `RunWith` with `BundleDir` = temp dir, no keys, logger writing to a buffer, `Options.Gateway` pointing at the bufconn `placeholderGateway` from `placeholder_test.go`; cancel ctx; assert one `level=WARN` line containing `trusted_keys is empty`; subcases: keys set (use a generated public key hex) → no such line; `BundleDir` empty → only `bundle_dir is not set`; injected `Options.Deployer` → no such line | `cd core/runtime && go test -race ./internal/runtime -run TestRunWarns` | no network |
| existing `TestSignedBundleSource`, `TestConfigTrustedKeys` in `signed_test.go` | regression for the shared helpers | `cd core/runtime && go test -race ./internal/runtime/...` | unmodified |
| `TestChain/json-bundle` in `internal/e2e/chain_test.go` (harness change) | `putJSON` returns `sha256:` + hex SHA-256 of the written bytes and writes `<hex>.json`; the name argument is dropped or ignored | `cd core/runtime && go test -race ./internal/e2e/...` | assertions unchanged |
| `TestRealWiringHTTPConnector/json-bundle`, `TestRealWiringTestModeWarnAndEncryptedFileSecrets` in `internal/e2e/realwiring_test.go` (harness change) | `putWith(t, false, ...)` returns the real digest | same | |
| `TestPlainBundleTamperedRejected` in `internal/e2e/chain_test.go` (new) | real `RunWith` with plain `bundle_dir` (`startRuntime(t, be.URL, false)`), modelled on `TestSignedBundleRejections`: write `employeeFlow` as a plain bundle, keep its digest, overwrite the same `<hex>.json` with a different valid flow, `setDesired(1, dep("tampered", d))` → `statusFor(..., "error")` with a message containing `digest`; `setDesired(2, dep("upper", "sha256:"+64×"A"))` with a matching upper-case file present → `error`; finally `refused(e.httpAddr)` is true (no listener started) | same | self-contained, uses the fake gateway; no other deployment runs, so `refused` is meaningful |
| `TestRuntimeAgainstTSGateway`, `TestWrongIdentityRejected`, `TestCertificateIdentities` in `internal/interop/interop_test.go` (harness change) | `putPlain` writes bytes and returns their digest; `cp.component(t, name, content, digest)` and the direct `POST /v1/components/{id}/revisions` in `TestRuntimeAgainstTSGateway` pass that `digest`; asserts unchanged | `cd core/runtime && go test ./internal/interop/...` | skipped without `node` or the control-plane `dist`; run once with them before merge |
| `TestInstallPolicy` in `core/runtime/cmd/nurtuo/service_test.go` (Phase 2) | table on `installPolicy(&runtime.Config{...})`: no `bundle_dir` → ok; dir + keys → ok; dir, no keys → error containing `trusted_keys` and `allow_unsigned_bundles`; dir, no keys, `AllowUnsignedBundles` → ok with non-empty warning; the message never contains key material | `cd core/runtime && go test -race ./cmd/nurtuo -run TestInstallPolicy` | pure function, no OS service manager |
| `TestLoadConfigYAMLAndJSON` (Phase 2 extension) in `internal/runtime/runtime_test.go` | `allow_unsigned_bundles: true` parses in YAML and JSON | `cd core/runtime && go test -race ./internal/runtime -run TestLoadConfig` | only under D3 (ii) |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | After Phase 1 an operator who copies pretty-printed `flow.json` files under control-plane digests sees every plain deployment `failed` with "does not match digest" | medium | compatibility | release note; `operations.md` states the exact byte rule and the explicit-`digest` path; message names the digest |
| 2 | Without `trusted_keys`, a user with revision-create rights posts a revision with `digest` = hash of a file they planted in `bundle_dir` (or a compromised control plane does), and the runtime runs it | high | security | residual by design under D2 (a); start-up warning; D3 install gate; signed bundles; default flip in #64 |
| 3 | Phase 2 lands but D3 is answered "warn only": new service installs keep running unsigned with only a warning | medium | security | owner decision; Phase 1 still prevents substitution |
| 4 | #62 edits `deployer.go` (`EngineDeployer.flows`) and Phase 2 edits `service.go` while #59 changes `program`/`runService` there: merge conflicts | low | process | helpers live in a new `digest.go`; Phase 1 touches only `DirBundleSource`/`SignedBundleSource` bodies and one `RunWith` line; Phase 2 touches only the `"install"` case; rebase, do not combine PRs |
| 5 | A multi-GiB file named `<hex>.json` is read fully into memory before hashing (also true for `.bundle` today) and the runtime is OOM-killed | low | runtime | out of scope; an `io.LimitReader` with `bundle.MaxBundleSize` is a follow-up (and part of #64's fetch path) |
| 6 | Interop tests are skipped in a CI job without `node`/`dist`, so the migrated `putPlain` is not exercised and a broken control-plane digest path goes unnoticed | medium | tests | run interop locally with the control plane built before merge; e2e `TestPlainBundleTamperedRejected` covers the runtime side without node |
| 7 | Under the OS service a start error is only logged (#59), so if the owner picks D2 (c) the runtime looks healthy while it runs nothing | high (only under D2 (c)) | runtime | recommended D2 (a) avoids it; otherwise sequence after #59 |

## Improvement opportunities

- A small `nurtuo bundle digest <file>` helper that prints the digest of a plain bundle would make the byte rule easier to follow (new CLI surface, Ask First; not proposed here).
- Include the expected and actual SHA-256 in the mismatch message for faster diagnosis (actual hash is not secret).

## References

- Issue #61; parent gap list PR #47; supervisor spec gap 3; bundle-dir spec (#58, #70); #64 and the `bundle-artifact-delivery` draft.
- Pre-implement report: [analysis/pre-implement-unsigned-bundle-sha256.md](analysis/pre-implement-unsigned-bundle-sha256.md).
- Open Mercato specs with similar scope: none directly; `module-runtime-start-hook` for start-up checks (ideas only).

## Changelog

- 2026-11-24: created as draft.
