<!-- SPDX-License-Identifier: Apache-2.0 -->
# Which specs nurtuo needs, compared with Open Mercato

[Open Mercato](https://github.com/open-mercato/open-mercato) keeps about 610 specifications in
`.ai/specs/` (448 pending or in progress, 165 in `implemented/`), written before each feature. We
reviewed all of their titles and the folder rules in `.ai/specs/AGENTS.md` and picked the ones whose
scope matches what nurtuo has built (milestone M1) or must build next. This file is the result:
which specs nurtuo should have now, and which Open Mercato specs to read for ideas.

Open Mercato is a commerce/ERP platform; most of its specs are domain features that do not apply to
an integration platform. Categories we skipped, with approximate counts: CRM and customers (~60),
sales, orders, pricing, tax and payments (~55), catalog and products (~25), warehouse/WMS (~8),
storefront and checkout (~10), design system, UI components and accessibility of the admin UI (~45),
timesheets, staff and bookings (~15), documents, messages and email (~20), their AI assistant and
agent orchestrator internals (~40), their module/generator/standalone-app tooling (~45), one-off
issue fixes (~30).

What we adopt from their process: dated file names, `implemented/` for finished specs, a fixed set
of sections with concrete risks, and the rule "spec before code for new features and contract
changes". See [README.md](README.md).

## As-built specs for milestone M1

M1 shipped as a stack of pull requests (#19 to #37) on 2026-11-21 without specs. These specs
describe the code on `main`, link the pull requests and list the gaps. Squash commits are on `main`.

| # | nurtuo spec | Pull requests (squash commit) | ADRs | Open Mercato specs with a similar scope |
|---|---|---|---|---|
| 1 | `repo-foundation-and-ci` | #19 (`678663a`), #15 (`c69f869`), #39 (`e22ffab`) | 0001 | `foundation`, `ci-cd-performance`, `phased-integration-ci`, `harness-validation-gate` |
| 2 | `license-boundaries` | #15, #19 | 0001 | `enterprise-contribution-guard` (workflow), their `LICENSE` split |
| 3 | `release-and-packaging` | #16 (`3d3d506`) | — | `open-mercato-develop-snapshot-release`, `release-upgrade-companion-automation` |
| 4 | `connector-contract-v1` | #20 (`e637dfd`), #41 | — | `integration-extensions`, `llm-provider-ports-and-adapters` |
| 5 | `runtime-gateway-protocol-v1` | #20, #25 (`ee4d733`), #26 (`dcd60dc`) | 0009 | `module-runtime-start-hook`, `secure-sse-probe-event-scope` |
| 6 | `flow-format-v1-and-validator` | #21 (`e0bf106`) | 0002, 0003, 0004 | `code-based-workflow-definitions`, `subworkflow-explicit-ports-schema-builder` |
| 7 | `deployment-bundles` | #22 (`52d8561`) | 0006 | `code-workflow-version-pinning` |
| 8 | `engine-interpreter` | #23 (`54ca076`) | 0005 | `workflow-run-outcomes`, `workflows-parallel-fork-join`, `mutation-lifecycle` |
| 9 | `maps-and-expressions` | #23 | — | `workflows-wait-for-condition` |
| 10 | `wasm-script-sandbox` | #23 | — | none; their `security-checklist.md` style applies |
| 11 | `queues-and-dead-letter` | #24 (`2903374`) | 0007 | `queue-job-coalescing`, `queue-status-dashboard`, `data-sync-retry-resume-actions`, `purge-queues-on-db-wipe` |
| 12 | `listeners` | #24, #42 | 0008 | `inbound-webhook-handlers`, `payment-gateway-webhook-body-limits`, `webhooks-module` |
| 13 | `performance-budget` | #24 | — | `runtime-saturation-observability`, `enterprise-performance-stability-hardening` |
| 14 | `runtime-supervisor-and-deployer` | #25 | 0009 | `health-monitoring`, `module-runtime-start-hook` |
| 15 | `connections-and-secrets` | #25 | 0010 | `integration-secret-credential-editing`, `integrations-credentials-encryption-fail-closed`, `encryption-key-rotation-hardening` |
| 16 | `runtime-cli-service-standalone` | #25, #16 | — | `cli-standalone-app-support`, `production-bootstrap-credentials` |
| 17 | `control-plane-api-and-data` | #26 | — | `openapi-route-declared-response-precedence`, `oss-optimistic-locking`, `list-count-strategies` |
| 18 | `auth-rbac-audit` | #27 (`4541089`), #39 | — | `two-factor-authentication`, `rate-limiting`, `auth-acl-change-audit-logging`, `bearer-logout-session-revocation`, `tenant-ownership-and-module-acl-authorization` |
| 19 | `connector-http` | #28 (`7e8f57c`) | — | `payment-gateway-webhook-transport-hooks`, `inbound-webhook-handlers` |
| 20 | `connector-sftp` | #29 (`d6e14a8`) | — | `storage-hub` |
| 21 | `connector-postgres` | #30 (`b3ccd55`) | — | `data-sync-hub` |
| 22 | `connector-nats` | #31 (`2251a82`) | — | `webhook-hub` |
| 23 | `bridges` | #32 (`266731d`), #41 | — | `integration-marketplace`, `official-modules-lifecycle-management` |
| 24 | `ts-client-and-cli` | #33 (`c534511`) | — | `cli-module-search`, `mercato-cli-skills-sync` |
| 25 | `mcp-server-and-ai-evals` | #33 | — | `mcp-tool-exposure-parity-and-audit`, `issue-3805-direct-tool-mutation-approval`, `ai-agents-agentic-loop-controls`, `ai-key-user-acl-ceiling` |
| 26 | `canvas-editor` | #34 (`83a0fa4`) | — | `workflow-visual-editor-canvas-space`, `workflows-ux-redesign`, `wcag-22-aa-accessibility` |
| 27 | `e2e-and-interop-testing` | #35 (`411cf2e`), #39 | — | `integration-testing-automation`, `agentic-property-based-testing`, `stryker-mutation-testing-ci-gate` |

## Draft specs for gaps found in M1

Each gap below has evidence in the code or in the [M1 review guide](../review/PR-STACK.md). These
specs start as drafts and describe the work, not finished code.

| nurtuo spec | Evidence | Open Mercato specs with a similar scope |
|---|---|---|
| `backward-compatibility-contract` | `proto/`, `nurtuo.flow/v1`, `nurtuo.bundle/v1`, REST API, CLI and MCP tools are public; only `buf breaking` guards `proto/` | `BACKWARD_COMPATIBILITY.md` (root document) |
| `workspace-isolation` | roles and API tokens are not partitioned per workspace (M1 review guide) | `tenant-ownership-and-module-acl-authorization`, `org-scope-fail-open-authorization-hardening`, `crud-fail-open-hardening` |
| `auth-rate-limiting-and-lockout` | a throttle helper exists (`core/control-plane/src/auth/throttle.ts`), there is no per-route limit policy | `rate-limiting` |
| `enterprise-identity-saml-scim` | `core/control-plane/src/auth/enterprise-stubs.ts` answers 501; the README promises SSO/SAML and SCIM | `two-factor-authentication`, `oauth2-grant-lifecycle-core` |
| `observability-logging-telemetry-health` | `core/runtime/internal/telemetry` is a placeholder; no logging, metrics or health contract | `structured-logging-facade`, `telemetry-and-otel`, `health-monitoring`, `error-reporting-policy` |
| `bundle-artifact-delivery` | `FetchArtifact` is not implemented; WASM script modules cannot be shipped in bundles | `code-workflow-version-pinning` |
| `http-listener-routing` | the HTTP listener matches exact paths only; the GetEmployee example uses `/employees/{id}` | `inbound-webhook-handlers` |
| `queue-step-runner` | the flow schema accepts `queue` steps, the engine has no runner for them | `queue-job-coalescing` |
| `connector-listen-operation` | `Listen` is skipped in the contract suite; the NATS and JVM connectors do not implement it | `webhooks-module` |
| `distributed-runtime-roles` | `nurtuo role` is a stub; the README promises clusters and HA | `lazy-auto-spawn-queue-workers`, `lazy-auto-spawn-scheduler` |
| `release-signing-and-installers` | cosign signing is skipped, no MSI or macOS notarization, the control plane is not packaged | `open-mercato-develop-snapshot-release` |
| `quality-gates` | TypeScript `lint` scripts are `echo TODO` in nine packages; CI does not check for stale generated code; contract-suite cases are TODO | `harness-validation-gate`, `stryker-mutation-testing-ci-gate`, `agentic-property-based-testing` |
| `m1-acceptance-review` | ARCHITECTURE.md defers M1 acceptance criteria to "reference Part V §1"; the reference document is not in the repository | process, no direct equivalent |

## Open Mercato specs worth reading later

Not needed yet, useful when the matching milestone starts: `webhooks-module` and `webhook-hub`
(outbound webhooks), `integration-marketplace` and `official-modules-*` (connector catalogue),
`data-sync-hub` and `data-sync-run-scoped-cursor` (incremental sync), `encryption-key-rotation-hardening`
and `system-scoped-encryption-key-contract` (secret store), `oauth2-grant-lifecycle-core` (connector
OAuth), `workflows-parallel-fork-join` and `correlated-workflow-signal-waits` (engine), `railway-one-command-deploy`
and `aws-terraform-deployment-playbook` (hosting), `pr-label-workflow` (when the contributor count grows).
