<!-- SPDX-License-Identifier: Apache-2.0 -->
# Specifications

A specification (spec) says **what** a feature does, which contracts it touches and how we know it
works. An [ADR](../adr/) records **why** one design was chosen. A feature usually has one spec and
zero or more ADRs.

## When a spec is required

Write or update a spec **before coding** when a change:

- adds a new module, connector, step kind, CLI command, MCP tool or API resource;
- changes a public contract: `proto/`, `nurtuo.flow/v1`, `nurtuo.bundle/v1`, the control plane REST
  API (`core/control-plane/openapi.yaml`), CLI flags, MCP tool schemas, runtime config keys;
- touches security: authentication, authorization, secrets, signatures, sandboxing, TLS;
- spans more than one area (for example runtime and control plane) or more than about a day of work.

Skip the spec for bug fixes, refactors without behaviour change, dependency bumps, CI tweaks and
documentation-only changes.

## Naming and lifecycle

- File name: `YYYY-MM-DD-kebab-case-title.md`, the date is when the spec was written.
- `docs/specs/` holds specs that are **draft**, **approved** or **in progress**.
- `docs/specs/implemented/` holds specs whose scope is merged on `main`. Move a spec there with
  `git mv` in the PR that completes it and update its status and changelog.
- Specs for work that already shipped without a spec (milestone M1) are written **as built**: they
  describe the code on `main`, link the pull requests and list the gaps found.
- [INDEX.md](INDEX.md) lists every spec with its status, pull requests and ADRs.
  [open-mercato-comparison.md](open-mercato-comparison.md) explains which specs this repository needs
  at its stage and which reference specs from Open Mercato they correspond to.

## Writing a spec

Start from [_template.md](_template.md). Rules:

- Every claim is checkable: name files, packages, functions, test names and commands.
- Link pull requests as `https://github.com/nurtuo/nurtuo/pull/N` and the squash commit on `main`.
- **Gaps & risks** names a concrete failure scenario, its severity (critical, high, medium, low), the
  affected area and the mitigation. "Might be slow" is not a risk; "a 300 MB payload is buffered in
  memory and the runtime is OOM-killed" is.
- **Testing** names the tests that exist, how to run them, and which ones are skipped or gated by an
  environment variable.
- A spec that changes a contract has a **Compatibility** section: what breaks, for whom, and how to
  migrate (see the backward compatibility spec).
- Keep the spec accurate: when the code changes, update the spec in the same pull request and add a
  changelog line.
- Run `node scripts/check-docs-links.mjs` before opening the pull request.
