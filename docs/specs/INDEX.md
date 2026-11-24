<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec index

Every specification with its status. See [README.md](README.md) for the rules and
[open-mercato-comparison.md](open-mercato-comparison.md) for why these specs exist.

The as-built specs for milestone M1 and the draft specs for its gaps are being written; this index
is filled in as their pull requests land.

| Spec | Status | Pull requests | ADRs |
|---|---|---|---|
| [canvas Run](2026-11-23-canvas-run.md): test-run the canvas flow in the control plane via `nurtuo flow run`, workspace connections and write-only secrets, Run button and per-step results | approved | [#68](https://github.com/nurtuo/nurtuo/pull/68) | [ADR 0010](../adr/0010-connections-and-secrets.md), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) |
| [nurtuo flow run](2026-11-23-nurtuoctl-run.md): run a canvas-exported flow locally with connection files and the built-in connectors | approved | [#50](https://github.com/nurtuo/nurtuo/pull/50) | [ADR 0010](../adr/0010-connections-and-secrets.md), [ADR 0005](../adr/0005-set-at-a-time-engine-with-per-document-status.md) |
| [runtime bundle_dir](2026-11-23-runtime-bundle-dir.md): never report a deployment as running without a bundle source; standalone `bundle_dir` and local signing key deferred to #64 (issue #58) | approved | [PR #70](https://github.com/nurtuo/nurtuo/pull/70) | [ADR 0006](../adr/0006-bundle-format-and-domain-separated-signature.md), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) |
| [apply DesiredState off the receive loop](2026-11-24-apply-desired-state-off-receive-loop.md): worker goroutine with a latest-wins slot so commands are handled during a slow apply (issue #60, gap 1 of the gateway protocol v1 spec) | approved | this PR | n/a |
