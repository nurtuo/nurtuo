<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Third-party agent skills

The skill directories in this folder are imported, unmodified, from
[mattpocock/skills](https://github.com/mattpocock/skills) by Matt Pocock.

- Source: https://github.com/mattpocock/skills
- Upstream commit: `abeeaf95660ba9e8b7c753eb38f441ca707a8823` (2026-11-19)
- License: MIT, Copyright (c) 2026 Matt Pocock. Full text in [LICENSE](LICENSE). It applies to every
  file in the imported skill directories. They carry no SPDX header and are not relicensed.

| Local path | Upstream path |
|---|---|
| `git-guardrails-claude-code/` | `skills/misc/git-guardrails-claude-code` |
| `domain-modeling/` | `skills/engineering/domain-modeling` |
| `grill-with-docs/` | `skills/engineering/grill-with-docs` |
| `grilling/` | `skills/productivity/grilling` |
| `to-tickets/` | `skills/engineering/to-tickets` |
| `setup-pre-commit/` | `skills/misc/setup-pre-commit` |

## Not imported (upstream dependencies)

- `to-tickets` refers to `/setup-matt-pocock-skills` for tracker configuration, which is not imported.

## Local notes

- `.agents/skills/` is the source of truth. `.claude/skills/` and `.cursor/skills/` are byte-identical
  copies, checked by `scripts/check-agent-skills-sync.sh`. Edit only `.agents/skills/`, then run
  `scripts/check-agent-skills-sync.sh --fix`.
- The hook that Claude Code actually runs is first-party: `scripts/hooks/block-dangerous-git.sh`. It
  is adapted from the upstream `git-guardrails-claude-code/scripts/block-dangerous-git.sh` (MIT) but
  allows feature-branch pushes, which the upstream script blocks. The upstream script is kept here untouched
  and is not wired up.
- `setup-pre-commit/` is imported as documentation of a procedure only. Nothing in this repo installs
  or enables pre-commit hooks.
