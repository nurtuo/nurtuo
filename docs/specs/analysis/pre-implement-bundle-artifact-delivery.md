<!-- SPDX-License-Identifier: Apache-2.0 -->
# Pre-implement: Bundle artifact delivery (`FetchArtifact`), control-plane signing and standalone `bundle_dir` defaults

**Readiness:** conditional (updated 2026-11-24 after approval; the original audit verdict was no-go)
**Spec:** docs/specs/2026-11-24-bundle-artifact-delivery.md
**Date:** 2026-11-24. Audited against `main` at `c9d1a7c` (includes #71 and #74).

## Update 2026-11-24 (after owner approval)

Łukasz answered Q1 to Q4 on 2026-11-24, 12:29 Europe/Warsaw and accepted all recommendations:
D7 (TS packer), D5 (opt-in `NURTUO_BUNDLE_SIGNING_KEY_FILE`), D9 (signed-only `FetchArtifact`) and
D11 (do not block on #57). They are recorded as decided in the spec, the Open Questions block is
closed, and the spec Status is `approved`. The product decisions in the sections below are
unchanged; the Blockers and the readiness-gate Critical finding are resolved, and the original
text is kept as the audit record.

Factual updates since the audit (`main` at `f4f50af`): #75 (#60) has merged, so the "rebase on
#75 if it merged" item now applies and Gaps #3 is lowered; #57's spec is approved (PR #73) and no
longer a draft. #61 is still a draft spec (PR #76) and unchanged.

**Verdict after approval: conditional.** Implementation may start. No blocker remains. Warnings
that stay open and must be handled during implementation (see "During implementation"): the
non-ASCII and number-heavy cross-language fixture; CI for interop/e2e or restating the criterion
as locally verified; rebasing Phase 2 on the merged #75; re-reading the key file on `EEXIST` in
`loadOrGenSigningKey`; re-checking #61 before Phase 2.

## Blockers (original audit, resolved by approval)

- **Status is `draft`, and four blocking owner decisions are open:** D5 (new control-plane env
  `NURTUO_BUNDLE_SIGNING_KEY_FILE` and the `bundle_digest` value switch), D7 (a TypeScript writer
  for the FROZEN `nurtuo.bundle/v1` format), D9 (signed-only serving), D11 (shipping on Windows
  before #57). The skill's readiness gate fails: Status is not `approved`, and blocking Open
  Questions remain. AGENTS.md also requires `approved` before coding sizeable security work.
- Nothing else blocks. The spec describes the codebase accurately (see "Grounding"), so once the
  owner answers Q1 to Q4 the expected verdict is **conditional**, given the during-implementation
  items below.

## Findings

- **Critical: readiness gate failed (draft plus open needs-owner questions).** Evidence: spec header
  `Status: draft`; Open Questions Q1 to Q4; Decisions rows D5, D7, D9, D11 classified
  `needs-owner`. Fix: the owner answers Q1 to Q4 on the spec PR. Record the answers in Decisions
  as decided, remove the Open Questions block, set `approved`.
- **Warning: second implementation of a FROZEN format (D7).** The compatibility contract
  ([backward-compatibility-contract](../2026-11-22-backward-compatibility-contract.md)) classifies
  the bundle signature domain and manifest canonical form as FROZEN. A TS writer cannot change
  the format, because `bundle.Verify` is the arbiter. But TS and Go canonical JSON differ on
  non-ASCII key order, number literals and U+2028 escaping (spec Gaps #1). The spec mitigates
  this with a golden test (ASCII input) and the interop test. Fix during implementation: add
  at least one non-ASCII and one number-heavy fixture that the TS writer packs and Go `Verify`
  accepts (shell out to `go test` from the interop suite, or check in a TS-produced archive
  fixture signed with an in-test-derived key). Do not claim byte equality with Go for those
  inputs.
- **Warning: new STABLE public surface (D5).** Control-plane environment variables are STABLE
  in the compatibility contract (`DATABASE_URL`, `PORT`, `GATEWAY_*`, …). Adding
  `NURTUO_BUNDLE_SIGNING_KEY_FILE` is additive, and the spec names it in Contracts and
  Compatibility. It is still a public contract change under AGENTS.md "Ask First: before
  implementation", which the spec correctly marks `needs-owner`. The opted-in change of the
  `Deployment.bundle_digest` value matches the proto comment ("sha256 of the signed bundle").
  The spec documents the operator ordering (runtimes get `trusted_keys` first). No fix beyond
  owner approval.
- **Warning: the "CI deploy needs no manual copy" criterion is not enforced by any current CI job.**
  `go` in `.github/workflows/ci.yml` runs `go test -race github.com/nurtuo/nurtuo/...` without
  building the control plane. The interop suite and `TestStandaloneE2EHandshakeAndDeploy` then
  skip (they need Node and `core/control-plane/dist`). Searched `ci.yml` for `interop` and
  `standalone`: no matches. Fix during implementation: add a CI step or job that builds
  `@nurtuo/control-plane` and runs `./internal/interop/...` and `-run TestStandaloneE2E` (adding
  tests or jobs needs no owner approval per AGENTS.md). Alternatively, state in the spec that the
  criterion is verified locally until such a job exists.
- **Warning: merge-conflict and behaviour interaction with in-flight PR #75 (#60).** Phase 2
  edits `core/runtime/internal/gateway/client.go` (stores the `grpc.ClientConn`, adds
  `FetchArtifact`), which #75 also changes (`ApplyDesiredState` off the receive loop). The spec
  records the behavioural interaction (Gaps #3, D15) but not the file overlap. Fix: rebase Phase 2
  on #75 if it merged first; keep the conn-field change minimal.
- **Important: Windows tests for D3 modes.** `TestDataDirsEnsureBundles` and the `0600` key check
  are Unix-only until #57. The spec says "skipped on Windows until #57". The Go CI job only
  cross-compiles for Windows, so this is consistent. Fix: none now; tie the follow-up to #57 (step 14).
- **Important: key file created with `O_EXCL` vs bootstrap called twice.** `runStandalone` calls
  `NewLocalBootstrapper().Bootstrap(dirs)` and `Stack.Run` calls it again. `loadOrGenSigningKey`
  must load on the second call (the spec defines load-if-exists first, so this is fine). Two
  concurrent `nurtuo standalone` processes on one data dir could race. With `O_EXCL` the loser
  must then re-read, not fail. Fix during implementation: on `EEXIST`, read the file again.
- **Important: #61 assumption is explicit but unverified.** The spec assumes #61 changes only
  `DirBundleSource` and adds a start-up warning (issue text). No #61 spec or PR is on `main`
  (searched `docs/specs/` and open PRs on 2026-11-24). The seam is narrow: one digest helper,
  reused by whichever lands second. Fix: re-check #61's merged design at implementation time.
  If #61 changes `SignedBundleSource` or the `.bundle` layout, update Phase 2 before coding.
- **Nice-to-have: `core/deploy/bundle/README.md` says "not yet part of `go.work`"**, but `go.work`
  lists `./core/deploy/bundle`. This is outside the spec's files. It does not affect the plan: the
  golden test runs in the `go` job either way.

## Grounding (spec claims checked against code)

| Claim in spec | Checked | Result |
|---|---|---|
| `FetchArtifact` server-streaming, `FetchArtifactRequest{digest}`, `FetchArtifactResponse{data}`; `bundle_digest` comment "sha256 of the signed bundle" | `proto/nurtuo/runtime/v1/gateway.proto` | correct |
| Control plane registers only `connect`; `desiredStateMessage` sends `rev.digest`; `peerKey` from the mTLS cert | `core/control-plane/src/gateway.ts` | correct |
| `digestOf` = sha256 of `canonicalJson`; optional caller `digest` on revisions | `core/control-plane/src/api.ts` | correct |
| No packer or signer in the control plane; `Pack` used only in Go tests | grep for `bundle.Pack`, `nurtuo-bundle-v1`, `manifest.sig` across `*.ts`/`*.go` | correct (tests: `interop/harness_test.go`, `e2e`, `runtime/signed_test.go`) |
| `Verify` checks hashes and order, not canonical bytes; no trailing bytes allowed; `MaxBundleSize` 256 MiB; `nameRe` `{0,127}` | `core/deploy/bundle/verify.go`, `manifest.go`, `pack.go` | correct |
| `RunWith` source selection; `unconfiguredDeployer` with `TODO(#64)`; `errNoBundleDir` | `core/runtime/internal/runtime/run.go` | correct |
| `SignedBundleSource` reads `<hex>.bundle`, digest then signature, keeps `flow` and `connection` | `deployer.go` | correct |
| `Client.Run` owns the `grpc.ClientConn` locally; `ApplyDesiredState` synchronous on receive | `core/runtime/internal/gateway/client.go` | correct |
| `DataDirs` has no bundles dir; `Ensure` `0700` only for `Secrets`; `loadOrGenSecret` is 24-byte hex | `standalone_layout.go`, `standalone_bootstrap.go` | correct |
| `runRealRuntime` sets no `BundleDir` or `TrustedKeys`; `controlPlaneEnv` variables | `standalone_register.go`, `standalone.go` | correct |
| Tests named as existing: `TestSignedBundleArtifactPath`, `TestStandaloneE2EHandshakeAndDeploy`, `TestLocalBootstrapIdempotentAndUsableForMTLS`, `TestControlPlaneEnvWiresGatewayAndAuth`, `TestNoBundleDirNeverRunning`, `TestRunWithoutBundleDirReportsError`, `fakeServer`, TS `Rt` helper | test files | all exist |
| #57 draft proposes `secrets.EnsurePrivateDir` and says #64 should call it for `bundles\` | PR #73 head, `docs/specs/2026-11-24-windows-secrets-dacl.md` | correct (draft, not on `main`) |

No codebase mis-description was found. "Does not exist" claims are scoped to the searches listed.

## Contract and compatibility audit

| Phase | Surface | Change | Migration path | Verdict |
|---|---|---|---|---|
| 1 | `gateway.proto` | none | n/a | ok (`buf breaking` unaffected) |
| 1 | `FetchArtifact` behaviour | new on an experimental RPC | n/a | ok |
| 1 | control-plane env | `NURTUO_BUNDLE_SIGNING_KEY_FILE` added (opt-in) | unset = unchanged | needs owner (D5) |
| 1 | `Deployment.bundle_digest` value | archive digest when opted in | documented operator order | needs owner (D5) |
| 1 | `nurtuo.bundle/v1` | second writer, format unchanged | n/a | needs owner (D7), Warning above |
| 2 | `runtime.yaml` | no keys; signed mode also fetches | none needed | ok |
| 3 | standalone data-dir layout (FROZEN) | additive `bundles/`, `secrets/bundle-signing.key` | created on first start | ok (decided D3) |
| all | REST, CLI flags, MCP tools | none | n/a | ok |

## Repo-rule compliance

- Spec location, SPDX header, changelog, INDEX row, `Supersedes`/`Extends`: compliant (AGENTS.md
  "Spec first"; spec-writing skill). The only edit to the 11-22 draft is the one-line
  `Superseded by` note.
- License directories: all new code is in `core/` (AGPL). No Apache directory imports `core/`.
  `make boundaries` is unaffected.
- No new production dependency: Ed25519 and tar use `node:crypto` and hand-written code in TS,
  and the stdlib in Go.
- Tests in the same PR as the code; self-contained fixtures; no committed keys: compliant as written.
- Changesets: the touched packages (`@nurtuo/control-plane`, the Go runtime) are AGPL and not
  consumed as Apache packages, so a release note is enough. The spec plans one (step 13).
- Security work: needs `approved` before code (AGENTS.md). No check is disabled or loosened, so no
  `needs-owner-review` merge gate is expected.

## Risks

| Risk | Severity | Mitigation in spec | Residual |
|---|---|---|---|
| TS/Go format divergence | High | golden test plus interop test | add non-ASCII fixture (see Warning) |
| Windows key readable by other users before #57 | High (multi-user Windows) | D11, documented warning | owner accepts or blocks |
| Large fetch blocks receive loop before #75 | Medium | Gaps #3 | rebase on #75 |
| No retry until next desired state on transient fetch failure | Medium | Gaps #4 (pre-existing) | follow-up |
| Interop/e2e not in CI | Medium | — | add CI job (Warning) |

## During implementation

Applies once the owner has answered the blockers. These items make the verdict `conditional`:

1. Add the non-ASCII / number-heavy cross-language fixture (TS writes, Go `Verify` accepts).
2. Add a CI step that builds the control plane and runs the interop and standalone e2e tests, or
   restate the CI acceptance criterion as locally verified.
3. Rebase Phase 2 on PR #75 (now merged); keep the `Client` conn-field change minimal.
4. `loadOrGenSigningKey`: on `EEXIST`, re-read instead of failing.
5. Re-check #61's merged design before Phase 2; reuse its digest helper if present.

## Next action

- `conditional`: start implementation on a `feat/` PR (`Closes #64`), phase by phase, and clear the
  five "During implementation" items. Re-run this audit before Phase 2 if #61 has merged.

```
Readiness: conditional
Report: docs/specs/analysis/pre-implement-bundle-artifact-delivery.md
Spec: docs/specs/2026-11-24-bundle-artifact-delivery.md
```
