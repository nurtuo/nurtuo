<!-- SPDX-License-Identifier: Apache-2.0 -->
# Pre-implement: Verify the SHA-256 of unsigned bundles and warn when `trusted_keys` is empty

**Readiness:** conditional
**Spec:** docs/specs/2026-11-24-unsigned-bundle-sha256.md

Date: 2026-11-24. Analysis only. The spec's product decisions were not changed. Four purely factual
corrections were applied to the spec before this report:

- the `SignedBundleSource` line range is now 49–86;
- `placeholder_test.go` was described as using a custom `BundleSource`; it uses `unconfiguredDeployer`;
- the `trusted_keys` search result now also lists the `trustedKeys()` error;
- `TestSignedBundleRejections` is now listed as unaffected.

The grounding was checked twice: by the spec author and by a separate read-only audit agent that was given only the spec path.

## Readiness update (2026-11-25)

Łukasz approved D2 and D3 at 19:09 Europe/Warsaw, clearing the owner-decision gate for both phases. The spec and index now show `approved`. Phase 2 uses the emergency security exception, includes upgrade-note wording, and treats `allow_unsigned_bundles` as experimental, matching the #64 draft in open PR #79. Readiness is conditional on incorporating the remaining implementation-plan findings below.

## Rules loaded

- `AGENTS.md`:
  - Spec first; no code while the spec is `draft`.
  - Ask First for `runtime.yaml` keys and CLI behaviour.
  - Never import `core/` from Apache-2.0 directories. This does not apply: all code here is under `core/runtime`.
- `docs/specs/README.md` and `_template.md`.
- spec-writing review checklist.
- Compatibility document: `docs/specs/2026-11-22-backward-compatibility-contract.md`. It is itself a draft and is used here as the best available statement of surface stability.
- Lessons: the `docs/lessons/` index was routed. No record matches. The candidates were proto descriptor names and CI label gates / duplex gRPC tests; both are about other areas.

## Grounding (verified against the code at `7953d4f`)

The spec's description of current behaviour is correct:

- **Runtime start-up.** `RunWith` selects the bundle source in `run.go` lines 79–127, and the `bundle_dir is not set` warning is at line 125.
- **Plain bundles.** `DirBundleSource` (`deployer.go` 35–43) checks only the name and does not hash the content.
- **Signed bundles.** `SignedBundleSource` checks the digest syntax, reads the file, compares the digest, verifies the signature and re-encodes the content. Its error messages are as quoted in the spec.
- **Failed fetch.** `EngineDeployer.Apply` returns on a fetch error at lines 199–201. That is before it takes the lock or touches `flows`, `desired` or `deps` (line 248), so a failed `Apply` leaves the previous state in place.
- **Reconcile.** `supervisor.Reconcile` skips deployments that have not changed (`reconcile.go` line 130, `equal`). The claim that the supervisor does not re-apply an unchanged digest is correct.
- **Config.** `Config.Validate` rejects `trusted_keys` without `bundle_dir`.
- **Service.** `service.go`: case `"install"` is at line 178 and only calls `LoadConfig`. `slogWriter` maps `level=WARN` to `Warning`.
- **Control plane.**
  - `canonicalJson` and `digestOf` are in `api.ts` lines 21–29.
  - An explicit `digest` must match `^sha256:[0-9a-f]{64}$`.
  - `gateway.ts:75` passes `rev.digest` through unchanged.
- **Who writes plain bundles.** Nothing outside tests writes `<digest>.json` or calls `DirBundleSource`. The search covered Go, TS, `scripts/`, `docs/` and `examples/`. Standalone sets no `BundleDir`.
- **Tests.** Every helper the test plan names exists with a compatible signature:
  - `startRuntime(t, url, signed)`, `statusFor`, `setDesired`, `dep`, `employeeFlow`, `refused(addr)`;
  - `placeholderGateway`, `TestDispatchServiceErrors`, `TestLoadConfigYAMLAndJSON`;
  - `Manager()`.
- **#64 draft.** `bundle-artifact-delivery` does propose `allow_unsigned_bundles` and refusing to start without keys (lines 71–72, 95–101). D2 and D3 are consistent with it.
- **Bundle-dir spec.** There is no contradiction with the approved bundle-dir spec, decisions D1–D8:
  - #58 phase 1 (`unconfiguredDeployer`, the WARN) is kept;
  - standalone `BundleDir` and the signing key stay with #64.

## Findings

- **Resolved: readiness gate.** D2 and D3 are approved.
- **Resolved: Phase 2 compatibility path.** The spec names the emergency security exception for the stable install command and includes the upgrade note.
- **Resolved: opt-out stability.** Both specs classify `allow_unsigned_bundles` as experimental. PR #79 already uses that classification.

- **Warning: the Phase 2 risk to the Windows MSI is not covered.**
  - Evidence: the `release-signing-and-installers` draft (line 53) says the MSI registers the service with `nurtuo service install`. Under D3 (ii), an MSI install with an unsigned `runtime.yaml` would fail.
  - Fix: add a Gaps row, and say how the installer passes the opt-out or which config it writes first.
- **Important: one acceptance criterion has no test.**
  - Evidence: the criterion "a deployment that is already running is not changed" after its file is tampered with, and "the same one after a runtime restart" fails. `TestPlainBundleTamperedRejected` checks neither.
  - Fix: in a second subtest, deploy a valid plain bundle (`running`), tamper with the file, resend the same desired state at a new revision, and assert the flow still serves. Then restart the runtime (`boot` again on the same dir) and assert `error`.
- **Important: the warning test needs a shared helper.**
  - Evidence: `placeholderGateway` (`placeholder_test.go:67`) is only the gRPC server. The bufconn dial and the `Options.Gateway` wiring are inline in `TestRunWithoutBundleDirReportsError` (lines 95–115).
  - Fix: step 4 should extract that wiring into a small test helper. If the new test reuses its hard-coded `sha256:aa` with `bundle_dir` set, the deployment fails with `invalid bundle digest`. That is harmless for a warning test, but say so.
- **Important: the interop migration reorders calls.**
  - Evidence: every call site runs `cp.component(...)` first and `putPlain(digest, ...)` after (`interop_test.go` lines 20–21, 37–41, 61–62, 124–125, 136–137).
  - After the migration, `putPlain` must run first and return the digest. That digest is passed to `component` or to the direct `POST`.
  - Interop is skipped without `node` or the control-plane `dist`.
  - Fix: state the new call order in step 3. Enforce "run interop once before merge" as a PR checklist item (the spec's Gaps row 6).
- **Important: stale comments that step 5 does not list.**
  - `deployer.go:46–47` (`SignedBundleSource` "returns {"flows":[...]}"; it also returns `connections`).
  - `config.go:31` (the `BundleDir` comment names only `<digest>.json`).
  - `config.go:32–34` (the `TrustedKeys` comment sits above `HTTPAddr`).
  - Fix: add them to step 5.
- **Important: scope cohesion.** Phase 1 (verify and warn, no contract change) and Phase 2 (new key, install behaviour, Ask First) can each ship on their own. D12 keeps one spec, which is acceptable. Fix: ship Phase 2 as a separate PR using the recorded D3 approval.

## Contract and compatibility audit

| Surface | Phase | Change | Covered by the spec |
|---|---|---|---|
| `<bundle_dir>/<hex>.json` layout (experimental) | 1 | narrowed: the name must be the SHA-256 of the bytes | yes (Contracts, Compatibility, release note) |
| `gateway.proto`, REST, CLI flags | 1, 2 | none | yes |
| `runtime.yaml` `allow_unsigned_bundles` | 2 | new experimental key under approved D3 (ii) | yes, aligned with #64 |
| `nurtuo service install` exit behaviour (STABLE) | 2 | exit 1 for an unsigned config | yes, emergency security exception and upgrade note |

## Risks

- **High:** residual authenticity risk without `trusted_keys`. Under D2 (a), a control-plane revision writer can point a digest at a planted file. The spec documents and accepts it under approved D2; the mitigation is #64's default flip.
- **Medium:** non-canonical producers break after upgrade. Mitigation: release note and the explicit-`digest` path.
- **Low:** merge conflicts with #59 (`service.go`) and #62 (`deployer.go`). Mitigation: the new `digest.go` and separate PRs.

## During implementation

Add the remaining Warning and Important findings to the relevant implementation steps. Ship Phase 2 separately under the recorded D3 approval and include the upgrade note in its release.

```
Readiness: conditional
Report: docs/specs/analysis/pre-implement-unsigned-bundle-sha256.md
Spec: docs/specs/2026-11-24-unsigned-bundle-sha256.md
```
