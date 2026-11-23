<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Never report a deployment as running without a bundle source (`bundle_dir`)

Status: approved (2026-11-24, Łukasz). Date: 2026-11-23.

Extends: [runtime-supervisor-and-deployer](implemented/2026-11-21-runtime-supervisor-and-deployer.md) (gap 1), [runtime-gateway-protocol-v1](implemented/2026-11-21-runtime-gateway-protocol-v1.md) (gap 5), [runtime-cli-service-standalone](implemented/2026-11-21-runtime-cli-service-standalone.md) (gap 2). Related: [bundle-artifact-delivery](2026-11-22-bundle-artifact-delivery.md) (`FetchArtifact`).

## TLDR

When `bundle_dir` is unset the runtime uses `memDeployer`, which accepts every deployment, so the supervisor reports `running` and the UI shows a flow as running while nothing listens. This spec makes a runtime without a bundle source report `error` ("no bundle_dir configured") and never `running` (Phase 1, runtime only, no contract change; this is what the #58 implementation PR ships). Wiring standalone mode to a real `bundle_dir` and a local signing key (Phase 2) is **not** done here (decision D2): it waits for `FetchArtifact` and is tracked in https://github.com/nurtuo/nurtuo/issues/64.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/70 (this spec; docs only); implementation will use `Closes #58` on a later `feat/` or `fix/` PR) |
| ADRs | [ADR 0006](../adr/0006-bundle-format-and-domain-separated-signature.md), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) |
| Issues | https://github.com/nurtuo/nurtuo/issues/58 (follow-up of #56, fixed by https://github.com/nurtuo/nurtuo/pull/69); phase 2 (`FetchArtifact` + runtime `BundleDir`/signing key): https://github.com/nurtuo/nurtuo/issues/64 |
| Code | `core/runtime/internal/runtime/run.go` (`RunWith`, `memDeployer`, `newMemDeployer`, `Options.Deployer`), `core/runtime/internal/runtime/config.go` (`Config.BundleDir`, `Config.TrustedKeys`, `Validate`), `core/runtime/internal/runtime/deployer.go` (`EngineDeployer`, `DirBundleSource`, `SignedBundleSource`), `core/runtime/internal/supervisor/` (`Deployer`, `StateProvider`, `Reconcile`), `core/runtime/cmd/nurtuo/standalone_register.go` (`runRealRuntime`), `core/runtime/cmd/nurtuo/standalone_layout.go` (`DataDirs`), `core/runtime/cmd/nurtuo/standalone_e2e_test.go` |
| User docs | [operations.md](../operations.md) (`runtime.yaml` table, standalone section) |

## Problem and goals

Who: operators and users of the canvas, who read deployment status in the UI.

Evidence on `main` (`916359c`, includes #69):

- `RunWith` (`run.go`): with `Options.Deployer == nil` and `cfg.BundleDir == ""` it sets `dep = newMemDeployer()`. `memDeployer.Apply` stores the deployment and returns nil; `Actual` returns the stored map.
- `supervisor.Reconcile` then calls `liveState`, which returns `StateRunning` because `memDeployer` does not implement `StateProvider`. The status `running` goes upstream; the control plane maps it through `STATE_MAP` in `core/control-plane/src/gateway.ts` to deployment status `running`.
- `runRealRuntime` (`standalone_register.go`) builds `nrt.Config` without `BundleDir`, so `nurtuo standalone` always takes this path. `TestStandaloneE2EHandshakeAndDeploy` documents it ("Limit (#58)") and asserts `running` for a flow nothing started.
- `docs/operations.md` states that with an empty `bundle_dir` "the runtime uses a placeholder deployer that only records deployments".

Goals:

- A deployment that has no bundle source is never reported `running`; the operator sees `error` with a message naming the missing `bundle_dir`.
- No change to a public contract (proto, REST, CLI flags, `runtime.yaml` keys) in Phase 1.
- Standalone mode has an explicit story for where bundles come from: none until `FetchArtifact` lands (Phase 2, issue #64).

Non-goals:

- Implementing `FetchArtifact` or a bundle cache (draft spec `bundle-artifact-delivery`).
- Key rotation or revocation.
- Changing the supervisor state machine or the gateway protocol.
- Making unsigned `DirBundleSource` bundles digest-verified (supervisor spec gap 3, owned by the security area).

## Open Questions

All blocking owner questions are resolved (D2, D3, D6 in Decisions, 2026-11-24). Nothing blocking remains.

Non-blocking:

- **Q4 (D5):** Startup warning when `bundle_dir` is empty. Resolved as the implementer default: yes, log a WARN.

## Design

### Current behavior (as the code works)

- Selection happens only in `RunWith`: injected `Options.Deployer` wins; else `BundleDir != ""` builds `EngineDeployer` (with `SignedBundleSource` when `TrustedKeys` is non-empty, else `DirBundleSource`); else `newMemDeployer()`.
- `EngineDeployer.Apply` already returns an error when `d.Source == nil` ("deployer: no bundle source configured") and when the file is missing (`deployer: fetch bundle ...: no such file`). The supervisor turns an `Apply` error into `Status{State: "error", Message: err.Error()}` and does not advance the revision, so the control plane resends on the next handshake. This is the existing, contract-conformant way to report failure.
- `Config.Validate` accepts an empty `bundle_dir`; it rejects `trusted_keys` without `bundle_dir`.
- Tests that rely on the empty-`bundle_dir` path: `TestRunGracefulShutdown` and `TestRunRejectsInvalidConfig` in `internal/runtime/runtime_test.go` (no deployments are delivered), and `TestStandaloneE2EHandshakeAndDeploy` (delivers one and expects `running`). `newMemDeployer` has no other callers (searched `core/` for `newMemDeployer|memDeployer`).

### Drift in prior specs (design is against the code)

| Spec says | Code does | Reference | Intentional? |
|---|---|---|---|
| runtime-cli-service-standalone gap 2: "`runRealRuntime` sets no `BundleDir`" | Still true after #69; #69 only fixed the identity and registration. The e2e test comments it as a limit. | `standalone_register.go`, `standalone_e2e_test.go` | Known gap, not drift |
| runtime-cli-service-standalone gap 1 mentions "placeholder deployer until gap 2" | Matches | `standalone_e2e_test.go` | yes |
| supervisor spec gap 1 proposes "make the placeholder report `error` or require `bundle_dir`" | Neither is done; the placeholder is unchanged | `run.go` `memDeployer` | open (this spec) |
| Issue #58 says "`TrustedKeys` from a locally generated signing key" | No standalone code generates an Ed25519 key (searched `core/runtime/cmd`: keys exist only for ECDSA TLS in `standalone_bootstrap.go`); bundle signing exists only in `core/deploy/bundle` and tests | `core/deploy/bundle/sign.go` | Gap: it must be built (see Phase 2) |
| Issue #58 implies `<data-dir>/bundles` makes deployments run | The control plane stores revisions in its repository and computes the digest as SHA-256 of canonical JSON (`digestOf` in `core/control-plane/src/api.ts`); nothing writes `<digest>.json` or `<hex>.bundle` into any directory | `core/control-plane/src/gateway.ts` (`desiredStateMessage`), `api.ts` | Likely oversight in the issue; see Q1 |

### Proposed approach

**Phase 1 — honest placeholder (runtime only).** Replace `memDeployer` by an `unconfiguredDeployer` (same file, same `supervisor.Deployer` seam, no new package):

- `Actual` returns an empty map; `Remove` is a no-op returning nil; `Apply` always returns `errors.New("no bundle_dir configured: the runtime cannot run deployments")`.
- No `StateProvider`: the state never reaches `running`. Because `Actual` is empty, every reconcile re-attempts and re-reports `applying` then `error`; this is bounded by reconcile triggers (new desired state, reconnect), not a timer.
- `RunWith` logs one `WARN` at start: `bundle_dir is not set; deployments will be reported as error`.
- Injected `Options.Deployer` is untouched, so tests and embedders keep full control.

Alternatives considered:

| Option | Why not (recommended) |
|---|---|
| Make `bundle_dir` required in `Config.Validate` | Breaks the documented "stable" key semantics (optional) and every config / test that omits it (`TestRunGracefulShutdown`, existing runtime-only installs that only connect). Startup failure is also less visible to a UI user than a `failed` deployment with a message. Kept as an owner option (D1). |
| Keep `memDeployer`, add a `StateProvider` returning `error` | Same observable result but keeps a deployer that records state it never acts on; larger surface than a deployer that refuses. |
| Report `applying` forever | Never terminal; hides the misconfiguration. |

**Phase 2 — standalone bundle source (out of scope for the #58 implementation PR; tracked in [#64](https://github.com/nurtuo/nurtuo/issues/64), ships with or after `FetchArtifact`; D2 forbids setting standalone `BundleDir` or a signing key before then).** Agreed shape (D3), recorded here for #64:

- Add `Bundles string` (`<root>/bundles`, `0o700`) to `DataDirs` and create it in `Ensure` (`standalone_layout.go`); set `BundleDir: env.Dirs.Bundles` in `runRealRuntime`.
- Generate an Ed25519 signing key on first bootstrap, persisted as `<data-dir>/secrets/bundle-signing.key` (`0600`, reused after restart, no rotation; hex seed via the existing `loadOrGenSecret` pattern in `standalone_bootstrap.go`), and pass the public key as `Config.TrustedKeys` so `SignedBundleSource` (digest check + signature) is used and unsigned `.json` files are refused.
- Something must still produce signed `<sha256>.bundle` files for a deployment's digest. The control plane digest is the SHA-256 of canonical JSON, not of a bundle archive, so without `FetchArtifact` or a control-plane packer no deployment can resolve: **every standalone deployment ends in `error` `fetch bundle ...: no such file`**. This is why D2 rules out Phase 2 alone: a working standalone deploy needs `bundle-artifact-delivery` (#64).

### Failure modes and what the operator sees

| Situation | Operator sees |
|---|---|
| `bundle_dir` unset, any deployment | status `error` → deployment `failed`, message "no bundle_dir configured ..." |
| `bundle_dir` set, bundle file missing | `error`, `deployer: fetch bundle sha256:...: no such file` (existing) |
| `bundle_dir` set, unsigned file but `trusted_keys` set | `error`, `deployer: invalid bundle digest` / rejected (existing) |

### Rollback

Phase 1 is a single-deployer swap; reverting restores the old behavior with no data change. Phase 2 (#64) adds a directory and a key file; rollback leaves them unused (the key must not be deleted if signed bundles were produced with it).

### Compatibility of in-flight deployments (D4)

`memDeployer` holds state only in memory. After upgrading and restarting, a standalone runtime has an empty `Actual`; the control plane's next handshake resends the desired state, and each deployment is applied again and reported `error`. Existing control-plane rows previously `running` flip to `failed` once, truthfully. No persisted runtime state is migrated. Standalone installs that never ran anything real lose nothing.

## Contracts

| Contract | Change | Stability |
|---|---|---|
| `runtime.yaml` keys `bundle_dir`, `trusted_keys` | None in Phase 1 (still optional; standalone does not set them). Phase 2 (#64) sets them from code, no schema change. | stable |
| `gateway.proto` `DeploymentStatus.state` | None: `error` is already an allowed open-set value used by `EngineDeployer`. | stable |
| Control plane REST / `STATE_MAP` | None. | stable |
| CLI flags | None (Phase 2 / #64 adds no flag; `--data-dir` already exists). | stable |
| Data-dir layout | None in Phase 1. Phase 2 (#64) adds `bundles/` and `secrets/bundle-signing.key`. | experimental |
| Observable behavior | A runtime without `bundle_dir` now reports `error` instead of `running`. | behavior change, no schema break |

## Compatibility

No proto, REST, CLI or config-schema break, so no migration path is needed. The behavioral change is described under D4 and must be called out in the changeset/release note and in `docs/operations.md` (replace "placeholder deployer that only records deployments"). D1 keeps `bundle_dir` optional, so there is no config break.

## Security

- Phase 1 reduces a false-assurance risk (UI says running, nothing runs) and adds no input or file access.
- Phase 2 (#64, not part of the #58 implementation PR): the signing key is a secret; it lives in `secrets/` (`0700` dir, `0600` file), is never logged, and is not committed. With `TrustedKeys` set, unsigned bundles are refused. Without it, `DirBundleSource` does not verify content against the digest (supervisor spec gap 3): anyone who can write `<data-dir>/bundles` controls what runs, so the directory must be `0700`. The key is generated and trusted by the same host, so it proves only "produced on this install", not authorship. Rotation: replacing the key invalidates all existing signed bundles; no rotation (D3).
- Owner approval for the Phase 2 key location and lifecycle was given in D3; the Phase 2 implementation PR (#64) still follows AGENTS.md security rules.

## Step plan

### Phase 1: honest placeholder (the scope of the #58 implementation PR)

1. Characterization test, green on unchanged code: `TestPlaceholderReportsRunning` documents that the current placeholder reports `running` (to be inverted in step 2). Verified by `cd core/runtime && go test -race ./internal/runtime/...`.
2. Replace `memDeployer` with `unconfiguredDeployer` (`Apply` errors) and flip the characterization test into `TestNoBundleDirNeverRunning`; add the startup warning. Verified by the same test command plus `go vet`.
3. Update `TestStandaloneE2EHandshakeAndDeploy`: assert the deployment reaches `failed` with a message containing `bundle_dir`, remove the "Limit (#58)" comment (still gated by `-short`, node and control-plane dist). Verified by `cd core/runtime && go test -race ./cmd/nurtuo -run TestStandaloneE2E`.
4. Docs: update the `bundle_dir` row and standalone text in `docs/operations.md`; add a changeset or release note as AGENTS.md requires (the runtime ships as a Go binary: note for the release). Verified by `node scripts/check-docs-links.mjs`.

### Phase 2: standalone bundle source (tracked in [#64](https://github.com/nurtuo/nurtuo/issues/64); not part of the #58 implementation PR; starts only with `FetchArtifact`)

5. Add `DataDirs.Bundles`; `Ensure` creates it `0o700`. Verified by a new unit test next to the `DataDirs` tests in `standalone_test.go` (no `Ensure` test exists today).
6. Generate and persist the Ed25519 key in the bootstrapper, idempotent across restarts. Verified by a bootstrap unit test (same key on second run, file mode `0600`).
7. `runRealRuntime` passes `BundleDir` and `TrustedKeys`. Verified by a unit test on the produced `nrt.Config` (refactor into a small `standaloneRuntimeConfig(env)` helper) and the standalone e2e now expecting `failed` with `fetch bundle`.
8. Not planned: a local bundle writer helper would be a new CLI surface (Ask First) and is excluded; `FetchArtifact` (#64) replaces it.

## Acceptance criteria

- WHEN a deployment is delivered to a runtime whose `bundle_dir` is unset THEN the reported state is `error` with a message containing `bundle_dir`, and no `running` status is ever emitted for it.
- WHEN a deployment has no bundle source THEN it must never be reported as `running` (including after reconnect, repeated desired state and `ReportChanges`).
- WHEN the runtime starts with `bundle_dir` unset THEN it logs a warning and still connects to the gateway and shuts down gracefully on context cancel.
- WHEN `bundle_dir` is set THEN behavior is unchanged (existing `e2e`, `interop` and `deployer_test` suites pass unmodified).
- WHEN a custom `Options.Deployer` is injected THEN it is used as before.
- WHEN a deployment is applied in standalone mode after Phase 1 THEN the control plane shows it `failed` with the `bundle_dir` message, not `running`.
- WHEN (Phase 2, #64, not in the #58 PR) `nurtuo standalone` starts twice on the same data dir THEN the signing key is unchanged, `bundles/` exists with mode `0700`, and `Config.TrustedKeys` contains exactly that public key.

## Tests

Tests ship in the same change as the code.

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestNoBundleDirNeverRunning` in `core/runtime/internal/runtime/placeholder_test.go` | Unit: `unconfiguredDeployer.Apply` returns an error mentioning `bundle_dir`; through `supervisor.New(dep).Reconcile` with a collecting `Reporter` the statuses are `applying` then `error` and never `running`; `Actual` stays empty; repeated Reconcile and `ReportChanges` never emit `running`; `Remove` of an unknown id returns nil | `cd core/runtime && go test -race ./internal/runtime -run TestNoBundleDirNeverRunning` | self-contained, no network |
| `TestRunWithoutBundleDirReportsError` in `core/runtime/internal/runtime/placeholder_test.go` | `RunWith` with empty `BundleDir` and an in-process bufconn fake gateway server (the existing `fakeServer` in `internal/gateway/client_test.go` is package-private, so the test defines its own small one or the type is moved to a shared test helper) delivers a `DesiredState`; the fake collects `DeploymentStatus`; assert `error` and message; `Options.Gateway` seam, insecure creds, `t.TempDir()` | same package, `-run TestRunWithoutBundleDirReportsError` | cancels ctx for cleanup |
| `TestRunWithDeployerOverrideUnchanged` | injected `Options.Deployer` still used | same package | guards the seam |
| `TestStandaloneE2EHandshakeAndDeploy` (modified) in `core/runtime/cmd/nurtuo/standalone_e2e_test.go` | e2e: deployment polls to `failed` with a message containing `bundle_dir`; second start still works | `cd core/runtime && go test -race ./cmd/nurtuo -run TestStandaloneE2E` | skipped with `-short`, without `node`, or without control-plane `dist`; `NURTUO_TEST_POSTGRES_URL` optional |
| `TestStandaloneRuntimeConfig` (Phase 2, #64; not in the #58 PR) in `core/runtime/cmd/nurtuo/standalone_test.go` | config helper sets `BundleDir`, `TrustedKeys`; key persisted and stable, `0600` | `go test ./cmd/nurtuo -run TestStandaloneRuntimeConfig` | uses `t.TempDir()` |
| existing `TestRunGracefulShutdown`, `TestSignedBundleSource`, `internal/e2e`, `internal/interop` | regression | `cd core/runtime && go test -race ./...` | e2e/interop may need env as today |

Characterization first: before changing `run.go`, add the test asserting today's `running` report (green on unchanged code), then invert it in the same change.

## Decisions

Approved by Łukasz on 2026-11-24 (Europe/Warsaw). Owner answers D2, D3, D6 are recorded exactly; D1, D4, D5, D7, D8 are implementer defaults and need no re-approval.

| ID | Question | Decision | Class |
|---|---|---|---|
| D1 | When `bundle_dir` is unset, what happens? | Placeholder reports `error` "no bundle_dir configured"; `bundle_dir` stays optional. Non-breaking, visible in the UI, one-file change. | implementer-default |
| D2 | Ship standalone `BundleDir` + local signing key before `FetchArtifact`? | **NO.** Do not send or set standalone `BundleDir` or a signing key before `FetchArtifact` exists. Phase 1 is the honest placeholder only, with no contract change. (Owner, 2026-11-24) | owner-approved |
| D3 | Phase 2 location and key | **YES for phase 2.** `BundleDir` = `<data-dir>/bundles` mode `0700`; signing key at `secrets/bundle-signing.key` mode `0600`; reused after restart; no rotation. Tracked in [#64](https://github.com/nurtuo/nurtuo/issues/64). (Owner, 2026-11-24) | owner-approved |
| D4 | `running` deployments accepted by `memDeployer` with no source | None persisted; re-reported `error` on the next handshake. Call out in release notes. | implementer-default |
| D5 | Startup warning when `bundle_dir` is empty? | Log WARN. | implementer-default |
| D6 | Standalone UX regression (deploy shows `failed`) | **YES, accepted.** Standalone deploys may show `failed` until `FetchArtifact` lands; prioritise `bundle-artifact-delivery` (#64). (Owner, 2026-11-24) | owner-approved |
| D7 | Split into more than one spec? | One spec; Phase 1 is implemented under #58, Phase 2 under #64. | implementer-default |
| D8 | Interaction with #56/#69 handshake | None; only the deployer choice changes, after the session is accepted. | implementer-default (informational) |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | A user deploys a flow in standalone after Phase 1 and sees `failed` forever because no bundle delivery exists | high | standalone UX | accepted in D6; prioritise `bundle-artifact-delivery` (#64); message points to `bundle_dir` |
| 2 | Phase 2 shipped alone makes the same deployment `failed` with `no such file` while adding a signing key nobody uses | medium | security / scope | avoided by D2 (not shipping Phase 2 alone) |
| 3 | Standalone with `BundleDir` but no `TrustedKeys` runs any `<digest>.json` a local user drops in, unverified | high | security | Phase 2 (#64) sets `TrustedKeys`; supervisor spec gap 3 for verification |
| 4 | Each reconcile re-applies and re-reports `applying`→`error`, producing status churn in the control plane for N deployments | low | runtime | bounded by reconcile triggers; dedupe is out of scope |
| 5 | The e2e test still asserts `running` in another suite after the change (for example a harness that omits `BundleDir`) | medium | tests | searched: only `standalone_e2e_test.go` relies on the placeholder; re-verify with `grep` at implementation |
| 6 | Signing key lost or replaced makes existing signed bundles unusable | medium | security | no rotation (D3); documented |

## Improvement opportunities

- A `nurtuo standalone` self-check that prints "runtime has no bundle source; deployments will fail" at startup.
- Digest-verify unsigned bundles (supervisor spec gap 3).

## References

- Issue #58; issue #64 (phase 2); PR #69 (handshake fix); PR #25 (runtime).
- [bundle-artifact-delivery](2026-11-22-bundle-artifact-delivery.md), [ADR 0006](../adr/0006-bundle-format-and-domain-separated-signature.md), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md).
- Pre-implement report: [analysis/pre-implement-runtime-bundle-dir.md](analysis/pre-implement-runtime-bundle-dir.md).

## Changelog

- 2026-11-23: created as draft.
- 2026-11-24: approved; decisions D2, D3, D6 recorded; phase 2 tracked in #64
