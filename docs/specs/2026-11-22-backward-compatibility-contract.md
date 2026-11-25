<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Backward compatibility contract

Status: draft. Date: 2026-11-22.

## TLDR

nurtuo has seven kinds of public contract (gRPC protos, the flow format, the bundle format, the
control plane REST API, the CLIs, the MCP tools, runtime configuration), but only `proto/` is
guarded, by `buf breaking`. This spec classifies every contract surface as FROZEN, STABLE,
ADDITIVE-ONLY or EXPERIMENTAL, defines a deprecation protocol (deprecate, bridge for at least one
minor release, upgrade notes, spec reference), an emergency security exception, and the CI checks
that enforce it. PR #41 is the worked example of a breaking proto change and what this protocol
would have asked of it.

## Status and links

| | |
|---|---|
| Pull requests | none yet; worked example: https://github.com/nurtuo/nurtuo/pull/41 (in review) |
| ADRs | [ADR 0001](../adr/0001-monorepo-license-split.md) (module layout), [ADR 0006](../adr/0006-bundle-format-and-domain-separated-signature.md) (bundle format), [ADR 0009](../adr/0009-outbound-only-runtime-gateway.md) and [ADR 0010](../adr/0010-connections-and-secrets.md) (contracts they introduce); a new ADR "compatibility classes and deprecation protocol" records the decision |
| Issues | https://github.com/nurtuo/nurtuo/issues/38 (the cause of PR #41) |
| Code (today) | `buf.yaml` (`breaking: use: [FILE]`), `proto` job in `.github/workflows/ci.yml` (`bufbuild/buf-action`, `breaking` only on pull requests, against `main`) |
| User docs | [ARCHITECTURE.md](../ARCHITECTURE.md) (section 10, versioning), [proto/README.md](../../proto/README.md), [bridges/README.md](../../bridges/README.md) (contract negotiation) |

## Problem and goals

Runtimes are installed inside customer networks and update later than the control plane
(ARCHITECTURE.md promises N-1 minor versions between them, not enforced or tested). Third parties
write connectors against `nurtuo.connector.v1` in four languages, CI pipelines script `nurtuoctl`,
AI agents call MCP tools by name and schema, and signed bundles are stored for years. Any silent
break strands one of them.

Today:

- `proto/`: `buf breaking` with FILE rules on pull requests. The only automated guard, and it can
  be skipped per PR with the `buf skip breaking` label (PR #41 used it).
- `nurtuo.flow/v1` (`sdk/ts/flow-schema/schema/flow.v1.schema.json`, `sdk/go/flow`), `nurtuo.bundle/v1`
  (`core/deploy/bundle`, `FormatVersion`), `nurtuo.connection/v1`: versioned by a schema string,
  no check that a change is compatible.
- REST API (`core/control-plane/openapi.yaml`, `info.version: 0.1.0`, paths under `/v1/`): no diff
  check.
- CLIs (`nurtuo`, `nurtuoctl`): no snapshot of commands, flags or exit codes.
- MCP tools (`core/mcp/src/server.ts`: `search`, `get_flow`, `apply_ops`, `validate`, `dry_run`,
  `list_step_kinds`, `get_step_schema`, `list_connectors`, `explain_diagnostic`,
  `suggest_next_steps`, `diff_flows`, `list_runtimes`, `deployment_status`, `deploy`): no schema
  snapshot.
- Runtime config keys (`runtime.Config` in `core/runtime/internal/runtime/config.go`) and
  environment variables: documented in operations.md only.

Goals: one written rule per surface; breaking changes are deliberate, documented and announced;
CI catches accidental breaks on every surface; a fast path exists for security fixes.
Non-goals: long-term support branches, compatibility of `internal/` Go packages or of the web UI
DOM.

## Design (proposed)

### Compatibility classes

| Class | Rule | May change only by |
|---|---|---|
| **FROZEN** | never changes meaning; a change creates a new identifier | a new version identifier (new proto package, new schema string, new handshake version) |
| **STABLE** | no removal, rename or semantic change without the deprecation protocol | deprecation protocol, or the emergency security exception |
| **ADDITIVE-ONLY** | new optional members may be added; existing ones never change or disappear inside the version | adding; consumers must ignore unknown members |
| **EXPERIMENTAL** | may change in any minor release; must be labelled as such where it is documented | release note |

Until `v1.0.0` (ARCHITECTURE.md: "`v0.x` until the M1 acceptance criteria pass") STABLE surfaces
may break in a **minor** release, but only through the deprecation protocol with the bridge period
shortened to "documented in the same release"; FROZEN and ADDITIVE-ONLY rules apply from now on.

### Classification of nurtuo's surfaces

| Surface | Class | Notes |
|---|---|---|
| Proto field numbers, enum numbers, RPC names, package names (`nurtuo.connector.v1`, `nurtuo.runtime.v1`) | FROZEN | wire compatibility; renaming a message is not a wire change but breaks every generated binding, see the worked example |
| Proto packages `v1` as a whole | ADDITIVE-ONLY | as stated in each proto header; new fields, enum values, RPCs |
| Open string sets `DeploymentStatus.state`, `Command.name` | ADDITIVE-ONLY | receivers ignore or answer `ok=false` for unknown values |
| Bundle signature domain `nurtuo-bundle-v1\n`, manifest canonical form, `FormatVersion = "nurtuo.bundle/v1"` | FROZEN | stored, signed artifacts must verify forever |
| Flow format `nurtuo.flow/v1` | ADDITIVE-ONLY | new optional properties and step kinds; note that `additionalProperties: false` means old validators reject new properties, so the canvas, `nurtuoctl` and runtime validators must be released before flows use them |
| Connection component `nurtuo.connection/v1`, `secret://` syntax, secret name pattern | FROZEN (syntax), ADDITIVE-ONLY (component fields) | |
| On-disk formats: `secrets.enc` (`NSEC1`), `master.key`, standalone data directory layout | FROZEN | a new format gets a new magic and a migration on first start |
| Bridge handshake line and version `1`, `NURTUO_BRIDGE_*` variables, `x-nurtuo-bridge-token`, exit codes | FROZEN | a change is handshake version 2, negotiated |
| Control plane REST API under `/v1/` (`openapi.yaml`) | STABLE for paths, methods, required request fields, response field meaning; ADDITIVE-ONLY for optional request fields and response fields | clients must ignore unknown response fields |
| `nurtuo` subcommands, flags, exit codes (0, 1, 2), default config paths, service name `nurtuo` | STABLE | `role` and `standalone --dev` are EXPERIMENTAL |
| `nurtuo flow` subcommands and flags | EXPERIMENTAL | includes the Run output flags |
| `nurtuoctl` commands, flags, exit codes, config file location, `--json` output fields | STABLE (commands, flags, exit codes); ADDITIVE-ONLY (`--json` output) | human-readable output is not a contract |
| MCP tool names and input schemas | STABLE; output fields ADDITIVE-ONLY | agents are prompted with tool names |
| `runtime.yaml` keys and value sets, `NURTUO_SECRET_<NAME>`, `NURTUO_DATA_DIR`, control plane env (`DATABASE_URL`, `PORT`, `GATEWAY_*`, `NURTUO_AUTH_SECRET`, `NURTUO_BOOTSTRAP_*`) | STABLE | unknown keys: warn, do not fail (today `yaml.Unmarshal` ignores them silently) |
| Go SDK `sdk/go/connector`, `contracttest`, `sdk/go/flow`; TS `@nurtuo/client`; bridge SDKs | EXPERIMENTAL until each module tags `v1.0.0`, then STABLE (SemVer of the module) | |
| `core/**/internal/**`, generated code layout, web UI | not a contract | |

### Deprecation protocol (STABLE surfaces)

1. **Spec first.** The change is described in a spec (new or updated) with a Compatibility section:
   what breaks, for whom, migration steps.
2. **Deprecate.** Mark the old element: proto `[deprecated = true]` plus a comment naming the
   replacement; OpenAPI `deprecated: true`; CLI prints a one-line warning to stderr; MCP tool
   description starts with "Deprecated:"; config key logs a warning at start.
3. **Bridge.** Old and new work side by side for at least one minor release (both proto fields
   populated, old CLI flag mapped to the new one, old REST path served).
4. **Upgrade notes.** `docs/upgrade.md` gets an entry under the release: element, replacement,
   release that removes it, migration steps.
5. **Remove** no earlier than the release named in the notes, in a PR that links the spec.

FROZEN surfaces never go through this: they get a new identifier (`nurtuo.connector.v2`, handshake
`2`, `nurtuo.flow/v2`) and both versions are served during the support window (contract
negotiation for bridges, ADR-documented migration code in `flow-schema` for flows).

### Emergency security exception

A STABLE or ADDITIVE-ONLY surface may break without the bridge period when keeping it exposes users
to a vulnerability with severity high or critical. Requirements: a security advisory (SECURITY.md
process), the upgrade note in the same release, a spec or ADR update within 7 days, and the
smallest possible break (for example reject an insecure value instead of removing a key). FROZEN
formats are not changed even then: the fix ships a new version and the old one is refused by
configuration.

### Enforcement in CI

| Surface | Check | Where |
|---|---|---|
| Proto | `buf breaking` (exists); the `buf skip breaking` label additionally requires a linked spec and an ADR or upgrade note (PR template checkbox, reviewed) | `proto` job |
| REST API | `oasdiff breaking` of `core/control-plane/openapi.yaml` against `main` | new job |
| Flow format | corpus test: every flow in `sdk/ts/flow-schema/testdata` and `examples/` from the last release validates with the new schema and Go validator | `ts` and `go` jobs |
| Bundles | golden signed bundle from the last release verifies (`bundle.Verify`) | `go` job |
| CLIs | snapshot of `--help` output and exit codes per command (`nurtuo`, `nurtuoctl`); a diff needs an upgrade note | `go` and `ts` jobs |
| MCP | snapshot of tool names and JSON input schemas from `core/mcp` | `ts` job |
| Runtime config | test that every documented key in operations.md parses, and that the last release's sample configs still load | `go` job |
| Runtime vs control plane N-1 | matrix job: last released `nurtuo` against the new control plane, and the reverse, through the interop harness (`core/runtime/internal/interop`) | nightly |

### Worked example: PR #41 (`Descriptor` to `ConnectorDescriptor`)

| Step | What PR #41 did | What this protocol asks |
|---|---|---|
| Classification | renamed a proto message inside `nurtuo.connector.v1` | message names are part of every generated binding: a STABLE source contract on top of a FROZEN wire contract; field numbers untouched, so the wire stays FROZEN-compliant |
| Justification | C# cannot compile a message named `Descriptor` (CS0542, issue #38); v1 unreleased | valid reason; pre-1.0 rule allows a STABLE break in a minor release |
| CI | `buf skip breaking` label | label allowed because the PR links issue #38; it should also link a spec (this one, or the connector contract spec) |
| Bridge period | none: all in-repo users updated in the same PR | acceptable pre-1.0 because no release shipped the old name; after 1.0 the alternative is `nurtuo.connector.v2`, or keeping `Descriptor` and setting the C# option `csharp_namespace`/a wrapper so the name does not collide |
| Upgrade note | not written | `docs/upgrade.md`: "Generated type `Descriptor` is now `ConnectorDescriptor` in Go, TS, Python, Java and .NET; wire format unchanged; rename references and regenerate stubs" |
| Spec reference | none | the connector contract spec's Compatibility section and changelog updated in the same PR |

## Contracts

This spec is itself the contract: the class table above. New files it introduces:
`docs/upgrade.md` (upgrade notes per release) and the CI checks listed. Changing a class of a
surface requires updating this spec.

## Compatibility

Introducing the classes breaks nothing. Adding the CI checks may fail existing open pull requests
that already break a surface; those PRs then need the protocol.

## Security

The emergency exception is the only path that skips the bridge period; it is bound to an advisory
so it cannot be used for convenience. FROZEN signed formats stay verifiable, so a security fix
never invalidates customers' stored bundles silently.

## Testing (acceptance)

| Test | Pass condition |
|---|---|
| `oasdiff` job | removing a response field from `openapi.yaml` in a test branch fails the job |
| flow corpus job | adding a required property to `startConfig` fails; adding an optional one passes |
| CLI snapshot | renaming `nurtuo run --config` fails with a pointer to the deprecation protocol |
| MCP snapshot | renaming tool `get_flow` fails |
| N-1 matrix | last release runtime deploys a flow through the new control plane |
| label policy | a PR with `buf skip breaking` and no linked spec is flagged by the PR template check |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation |
|---|---|---|---|---|
| 1 | Until the checks exist, a REST field rename in `openapi.yaml` merges green and breaks `nurtuoctl` users pinned to an older version | high | REST, CLI | ship `oasdiff` first; it is the cheapest check |
| 2 | `runtime.yaml` silently ignores unknown keys, so a renamed key (for example `http_addr`) is dropped without error and the listener falls back to `127.0.0.1:0` | medium | runtime config | warn on unknown keys (strict decoding with a warning) |
| 3 | The `buf skip breaking` label becomes routine because it is the fastest way to green CI | medium | process | require a linked spec, check in the PR template |
| 4 | Flow format `additionalProperties: false` turns every additive change into a break for older canvases and runtimes in the field | medium | flow format | document "validators before authors" ordering; consider tolerating unknown properties with a warning in runtime validation |

## Open questions

- Should STABLE apply to `nurtuoctl` human-readable output used in scripts today? Proposed: no;
  provide `--json` everywhere instead.
- Where do upgrade notes live long-term: `docs/upgrade.md` or the release changelog generated from
  Conventional Commits? Proposed: both, with `docs/upgrade.md` as the curated source.

## References

- Open Mercato: `BACKWARD_COMPATIBILITY.md` (root document: contract surface classes, deprecation
  protocol with a bridge period, upgrade notes, emergency exception).

## Changelog

- 2026-11-22: created (draft).
