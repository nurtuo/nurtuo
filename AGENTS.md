# AGENTS.md

Instructions for coding agents working in this monorepo. Human contributors: read [CONTRIBUTING.md](CONTRIBUTING.md).

## Always

- Check the Task Router below before researching or coding.
- Before a non-trivial change, look for an existing spec in [docs/specs/](docs/specs/) and in [docs/specs/INDEX.md](docs/specs/INDEX.md). Read the ADRs in [docs/adr/](docs/adr/) that the spec links.
- Spec first. Sizeable work needs a spec at `docs/specs/YYYY-MM-DD-<topic>.md`, written before code from [docs/specs/_template.md](docs/specs/_template.md). Sizeable means (copied from [docs/specs/README.md](docs/specs/README.md)): a new module, connector, step kind, CLI command, MCP tool or API resource; a public contract change; security work (authentication, authorization, secrets, signatures, sandboxing, TLS); or work spanning more than one area or more than about a day. Start implementing only when the spec status is `approved` or `in progress` (not `draft`).
- A PR that implements a spec MUST cite `Source spec: docs/specs/YYYY-MM-DD-<topic>.md` in the "Source spec" field of the PR template. Tests MUST land in the same PR as the code.
- Small fixes (bug fix, refactor without behaviour change, dependency bump, CI tweak, docs-only) need no spec; a short note in the issue is enough.
- Apache-2.0 packages that users consume need a changeset (`pnpm changeset`), or a note for the Go module tag (see [CONTRIBUTING.md](CONTRIBUTING.md)).
- Work trunk-based: branch from `main`, use Conventional Commits (`feat(runtime): ...`, `fix(canvas): ...`, `docs: ...`). PRs are squash-merged.
- Put code in the directory whose license fits. `core/` is AGPL-3.0-only; `proto/`, `sdk/`, `connectors/`, `bridges/`, `cli/`, `examples/`, `docs/` are Apache-2.0. Root files and new `scripts/*` files are `AGPL-3.0-only`. Start every new source file with an SPDX header matching the directory license (see [LICENSING.md](LICENSING.md), [docs/adr/0001-monorepo-license-split.md](docs/adr/0001-monorepo-license-split.md)). New Markdown under `docs/` starts with `<!-- SPDX-License-Identifier: Apache-2.0 -->`; the one exception is lesson records in `docs/lessons/`, which start with `---` frontmatter and carry the SPDX comment right after it.
- Change `.proto` files only under `proto/`, then run `make gen` and commit the regenerated code (`sdk/go/gen`, `sdk/ts/proto/src/gen`). Also regenerate the committed Python stubs with `python bridges/python/scripts/gen_proto.py` (CI checks `gen_proto.py --check` in `bridge-python`).
- Merging: Recenzent squash-merges when required CI is green (go, ts, proto, licenses, cla, bridge-dotnet), the PR matches its spec (or is a small fix with no spec), and no blocker/major is open. CI/workflow changes that only add tests or jobs are mergeable. Label `needs-owner-review` and wait for Łukasz only for the three cases under "Ask First: before merge".
- When a lesson record matches your task, read it: see [docs/lessons/README.md](docs/lessons/README.md). Never bulk-read `docs/lessons/`.

## Ask First

Owner approval (comment on the issue or PR) is needed at two points only.

Before implementation (no code until approved):

- a new spec in `docs/specs/` (status `approved` first, see Always);
- a public contract change (`proto/`, `nurtuo.flow/v1`, `nurtuo.bundle/v1`, `core/control-plane/openapi.yaml`, CLI flags, MCP tool schemas, runtime config keys); a breaking one also needs the merge approval below;
- a new production dependency;
- reducing the agreed scope of a spec;
- an architecture, license, or product-scope decision (new ADR, moving code between license directories);
- changing release automation (`.goreleaser.yaml`, `make snapshot`).

Before merge (label `needs-owner-review`; Recenzent does not merge until Łukasz approves):

- breaking contract/API changes;
- incompatible third-party licenses;
- security weakening: disabling or skipping checks; loosening workflow permissions; changing branch protection (including `main`); loosening the license/CLA allow-list; any change to `LICENSE*` or `LICENSING.md`.

Everything else, including CI changes that only add tests or jobs, needs no owner approval.

TODO(team): define which changes need a benchmark review beyond `make bench` on hot-path code.

## Never

- Never push to `main`. Open a PR from a branch.
- Never import or depend on `core/` (AGPL) from an Apache-2.0 directory. `make boundaries` enforces this.
- Never edit generated files by hand. Regenerate them instead:
  - `sdk/go/gen`, `sdk/ts/proto/src/gen`: `make gen`.
  - `bridges/python/src/nurtuo_bridge/_gen/`: `python bridges/python/scripts/gen_proto.py`.
  - `sdk/ts/flow-schema/src/schema.generated.ts`, `sdk/go/flow/flow.v1.schema.json`: `pnpm --filter @nurtuo/flow-schema run sync`.
  - `sdk/ts/client/src/schema.generated.ts`: `pnpm --filter @nurtuo/client run gen`.
  - `core/mcp/src/connectors.generated.ts`: `pnpm --filter @nurtuo/mcp run sync`.
  - `docs/flow-reference.md`: `node scripts/gen-flow-reference.mjs`.
- Never commit credentials, tokens, private keys, or secrets. Never open a public issue for a vulnerability; follow [SECURITY.md](SECURITY.md).
- Never start code for sizeable work whose spec is `draft` or missing.
- Never rename or restructure proto messages without checking [docs/lessons/proto-message-name-descriptor-breaks-csharp.md](docs/lessons/proto-message-name-descriptor-breaks-csharp.md).

## Validation Commands

Choose the smallest set that covers your change. Do not run the full monorepo suite for a docs-only change.

| Change | Command |
|---|---|
| Docs / Markdown / links | `node scripts/check-docs-links.mjs` for links; `make docs` also validates `examples/flows/reference/` and needs `pnpm --filter @nurtuo/flow-validate build` first |
| Example operation catalog | `node scripts/check-example-operations.mjs` |
| License or import boundaries, SPDX headers | `make boundaries` (checks SPDX only in `*.go`, `*.ts`, `*.tsx`, `*.proto`, not Markdown) |
| `proto/` | `buf lint`, `buf breaking`, then `make gen` |
| Go code | `go test -race ./<module-path>/...` in the touched module (modules: see `go.work`) |
| TS packages | `pnpm --filter <package> run test` (also `lint`, `typecheck`) |
| Hot path (`core/runtime`) | `make bench` |
| Cross-cutting or before a large PR | `make lint`, `make test`, `make build` |

- First-time setup: `make bootstrap` (pinned toolchain in `.mise.toml`). `make tools` installs the protobuf plugins.
- `make dev` runs the canvas and a standalone runtime.

## Task Router

| Area | Path | Key rule |
|---|---|---|
| Runtime, control plane, deploy, MCP, AI | `core/` (`core/runtime`, `core/control-plane`, `core/deploy`, `core/mcp`, `core/ai`) | AGPL-3.0-only. Never imported by Apache-2.0 code. Tests in the same PR. |
| Canvas (web UI) | `core/web` | AGPL-3.0-only. Check with `pnpm --filter @nurtuo/web run test` / `lint` / `typecheck`. |
| Connectors | `connectors/` | Apache-2.0. Read [docs/connector-authoring.md](docs/connector-authoring.md) and `docs/connectors/` first. |
| Protobuf contracts | `proto/` | Apache-2.0. Run `buf breaking`; regenerate with `make gen`; contract change = Ask First. |
| CLI | `cli/` | Apache-2.0. Flags are public contract. See [docs/cli.md](docs/cli.md). |
| SDKs | `sdk/go`, `sdk/ts/*` | Apache-2.0. `sdk/go/gen` and `sdk/ts/proto/src/gen` are generated. |
| Language bridges | `bridges/jvm`, `bridges/python`, `bridges/dotnet` | Apache-2.0. JVM and .NET stubs are generated in the bridge build; Python stubs are committed (see Never). |
| Examples | `examples/` | Apache-2.0. Flow examples under `examples/flows/reference/` are validated by `make docs`; operation catalogs by `node scripts/check-example-operations.mjs`. |
| Specs and ADRs | `docs/specs/`, `docs/adr/` | Spec before code; see Always. |
| Docs | `docs/` | Apache-2.0 SPDX comment first (not in lesson records). Run `make docs`. |
| Lessons | `docs/lessons/` | Route first, open only matching records. Frontmatter `---` first, SPDX comment after it. |

## Skills

Imported third-party skills live in `.agents/skills/` (source of truth; MIT, see [NOTICE](.agents/skills/NOTICE.md)). Copies in `.claude/skills/` and `.cursor/skills/` are kept identical by `scripts/check-agent-skills-sync.sh`; edit only `.agents/skills/`.

| Skill | Use it when |
|---|---|
| `git-guardrails-claude-code` | Changing or debugging the git safety hook (`scripts/hooks/block-dangerous-git.sh`, tests: `scripts/test-git-guardrails.sh`). If a git command is blocked, stop and ask; never work around it. |
| `domain-modeling` | A term is fuzzy or overloaded, or you add to `docs/GLOSSARY.md` or write an ADR in `docs/adr/`. Both live under `docs/`, not at the repo root as the skill's examples show. Do not invent decisions. |
| `grill-with-docs` | Stress-testing a plan or design before writing a spec. Depends on `grilling`. |
| `grilling` | Interviewing the user round by round to stress-test a plan or decision. Dependency of `grill-with-docs`; also usable on its own. |
| `to-tickets` | Splitting an `approved` spec into issues. Does not replace the spec; sizeable work still needs one (see Always). |
| `setup-pre-commit` | Only when the owner asks. Do not run it on your own: pre-commit hooks are not installed in this repo. |
