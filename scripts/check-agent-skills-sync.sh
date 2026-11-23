#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# .agents/skills is the source of truth for agent skills. .claude/skills (Claude Code) and
# .cursor/skills (Cursor) are plain copies, not symlinks: Windows checkouts without core.symlinks
# turn symlinks into text files and Cursor's symlink discovery is not verified.
# Fails when a copy drifts from the source.
#
#   scripts/check-agent-skills-sync.sh          # check
#   scripts/check-agent-skills-sync.sh --fix    # re-copy from .agents/skills
set -euo pipefail
cd "$(dirname "$0")/.."
SRC=.agents/skills
DESTS=(.claude/skills .cursor/skills)
fail=0
for d in "${DESTS[@]}"; do
  if [ "${1:-}" = "--fix" ]; then
    rm -rf "$d"
    mkdir -p "$d"
    cp -R "$SRC"/. "$d"/
  fi
  if [ ! -d "$d" ]; then
    echo "ERROR: $d is missing; run scripts/check-agent-skills-sync.sh --fix"; fail=1; continue
  fi
  if [ -n "$(find "$d" -type l)" ]; then
    echo "ERROR: $d contains symlinks; it must be a plain copy of $SRC"; fail=1
  fi
  if ! diff -r "$SRC" "$d" >/dev/null; then
    echo "ERROR: $d differs from $SRC; run scripts/check-agent-skills-sync.sh --fix"
    diff -rq "$SRC" "$d" || true
    fail=1
  fi
done
[ "$fail" -eq 0 ] && echo "agent skills in sync"
exit "$fail"
