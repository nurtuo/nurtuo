<!-- SPDX-License-Identifier: Apache-2.0 -->
# Pre-implement: Never report a deployment as running without a bundle source

**Readiness:** no-go (superseded: see the update below)
**Spec:** docs/specs/2026-11-23-runtime-bundle-dir.md

Date: 2026-11-23. Analysis only; the spec's product decisions were not changed. Two purely factual
corrections (a non-existent test name, an inaccessible test fixture) were applied to the spec before
this report.

## Update 2026-11-24

Owner decisions D2, D3 and D6 are recorded in the spec and it is `approved`. The questions and blockers
below are resolved and kept as history. Remaining work is implementing Phase 1 under #58; Phase 2
(`FetchArtifact`, runtime `BundleDir`/signing key) is tracked in
https://github.com/nurtuo/nurtuo/issues/64 and is out of scope for the #58 PR. Re-run this check
before starting Phase 1 code.

## Blockers

- Status is `draft` and three blocking Open Questions remain (Q1 to Q3), mapped to `needs-owner`
  decisions D2, D3 and D6. The readiness gate cannot pass until the owner answers them.
- Phase 2 (standalone `BundleDir` and signing key) touches signatures and secrets and has an
  unconfirmed high-stakes assumption (a local key as the trust root).

Phase 1 (honest placeholder, D1 and D4 to D5 implementer defaults) has no blocking question of its
own and could be approved separately once the owner confirms D6.

## Rules loaded

- `AGENTS.md`, `CLAUDE.md`: spec first, Ask First (security work, public contract, new CLI surface),
  tests in the same PR, SPDX/docs rules, `Source spec:` in the PR.
- `docs/specs/README.md`, `_template.md`: sections mapped; Compatibility and Gaps present.
- Compatibility doc: `docs/specs/2026-11-22-backward-compatibility-contract.md` exists (draft); the
  spec has its own Contracts and Compatibility sections.
- Lessons: `docs/lessons/` has two records, neither matches this area (CI label gates; proto message
  names), so none was opened.

## Codebase grounding (spec claim -> code)

| Spec claim | Verified in | Result |
|---|---|---|
| `RunWith` picks `newMemDeployer()` when `Deployer` nil and `BundleDir == ""` | `core/runtime/internal/runtime/run.go` | matches |
| `memDeployer` has no `StateProvider`, so `liveState` defaults to `running` | `run.go`, `supervisor/reconcile.go` | matches |
| `Apply` error becomes `Status{error}`, revision does not advance | `supervisor/reconcile.go` | matches |
| `EngineDeployer.Apply` errors when `Source == nil` and on a missing file | `internal/runtime/deployer.go:195-202` | matches |
| `Config.Validate` accepts empty `bundle_dir`, rejects `trusted_keys` without it | `config.go` | matches |
| `runRealRuntime` sets no `BundleDir`; e2e test records the limit | `cmd/nurtuo/standalone_register.go`, `standalone_e2e_test.go` | matches |
| `STATE_MAP` maps `error` to `failed`, `running` to `running` | `core/control-plane/src/gateway.ts:28` | matches |
| Digest is SHA-256 of canonical JSON; nothing writes bundle files for the runtime | `core/control-plane/src/api.ts:20-29,104-107`, `gateway.ts:75` | matches (searched `core/control-plane/src` and `core/runtime/cmd`) |
| No standalone Ed25519 key generation | `cmd/nurtuo/standalone_bootstrap.go` (ECDSA only) | matches (searched `core/runtime/cmd`) |
| `newMemDeployer` has no other callers | grep over `core/` | matches |
| `TestDataDirsEnsure` | does not exist | **fixed in the spec** (factual) |
| `fakeServer` reusable from `internal/runtime` tests | it is package-private to `internal/gateway` | **fixed in the spec** (factual) |

No codebase mis-description remains.

## Findings

- **Critical (readiness): draft with blocking needs-owner questions.** Spec Open Questions Q1 to Q3,
  Decisions D2, D3, D6. Fix: owner answers; then the spec moves to `approved` (Phase 1 alone
  could be approved first by splitting at approval time, D7).
- **Important: Phase 1 changes observable behavior for standalone.** After it, every standalone
  deployment shows `failed` until `FetchArtifact` exists; the e2e test must flip from `running` to
  `failed`. Spec covers this (D6, gap 1) but the product call is the owner's.
- **Important: issue #58's standalone proposal does not deliver a working standalone.** Evidence:
  nothing produces `<digest>.json` or signed `.bundle` files. The spec records this drift and
  recommends not shipping Phase 2 alone (D2). Owner must confirm that this deviates from the issue
  text.
- **Warning: Phase 2 step 8 (local bundle writer) would add a CLI surface** and is excluded; if wanted
  it needs its own spec.
- **Warning: test fixture for `TestRunWithoutBundleDirReportsError`** needs either a new small bufconn
  fake or moving the gateway `fakeServer` to a shared helper; the spec names both options, the
  implementer should choose the smaller one.
- **Warning: changeset/release note.** The runtime is an AGPL Go binary; AGENTS.md changesets apply to
  Apache-2.0 packages. Step 4 mentions a release note; confirm the mechanism at implementation.
- **Nice-to-have: status churn** (gap 4) can be deduped later.

Contract audit: no proto, REST, CLI flag or config-schema change in Phase 1; Phase 2 adds data-dir
files (experimental). No break without migration. Spec has a Compatibility section.

## Next action

Owner decisions D2, D3 and D6 (and a confirmation of D1 and its alternative). Then revise the
spec with the spec-writing skill, set `approved`, and re-run this check.

```
Readiness: no-go
Report: docs/specs/analysis/pre-implement-runtime-bundle-dir.md
Spec: docs/specs/2026-11-23-runtime-bundle-dir.md
```
