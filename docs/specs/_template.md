<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: <title>

Status: draft | approved | in progress | implemented. Date: YYYY-MM-DD.

## TLDR

Two or three sentences: what this is and the one thing a reader must know.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/N (squash commit `abcdef1`) |
| ADRs | ADR 000N as a relative link, e.g. `[ADR 0007](../../adr/0007-queue-dlq-via-advisory.md)` from `implemented/` |
| Issues | https://github.com/nurtuo/nurtuo/issues/N |
| Code | `path/to/package/` |
| User docs | relative link to the user-facing page in `docs/` |

## Problem and goals

What problem this solves and for whom.

- Goals: bullet list.
- Non-goals: what is explicitly out of scope.

## Design (as built)

How it works today: main packages and files, data flow, key types and functions. Diagrams only if
they help.

## Contracts

Everything other code or users depend on: APIs, proto messages, file formats, config keys, CLI
flags, environment variables, events. Mark each one stable, experimental or internal.

## Compatibility

Only when a contract changes: what breaks, for whom, migration steps.

## Security

Authentication, authorization, secrets, input limits, sandboxing, TLS, safe defaults.

## Testing

| Test | What it covers | How to run | Notes (skipped, env-gated) |
|---|---|---|---|
| `TestName` in `path/x_test.go` | ... | `cd module && GOWORK=off go test ./...` | |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | ... | high | ... | issue or draft spec |

## Improvement opportunities

Things that are not defects but would make the feature better, cheaper or safer.

## References

- Open Mercato specs with a similar scope (for ideas, not requirements).

## Changelog

- YYYY-MM-DD: created.
