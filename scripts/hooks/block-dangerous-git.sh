#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Claude Code PreToolUse hook (matcher: Bash). Blocks destructive git commands.
# Adapted from mattpocock/skills git-guardrails-claude-code (MIT), see .agents/skills/NOTICE.md.
# Unlike upstream it allows ordinary feature-branch pushes and blocks everything else that is
# destructive. Input: hook JSON on stdin. Exit 2 + message on stderr = block, exit 0 = allow.
#
# Best-effort token matching, not a sandbox: it stops accidents, not a determined bypass
# (aliases, scripts, variables). Boundary: `git restore <path>` and `git checkout <branch>` are
# allowed; only whole-tree forms (`.`, `:/`, `*`) are blocked.
#
# Test override: GIT_GUARDRAILS_CURRENT_BRANCH replaces the branch lookup.
set -uo pipefail

if ! command -v jq >/dev/null 2>&1; then
  echo "BLOCKED: git guardrails need 'jq' to parse the tool input (failing closed)." >&2
  exit 2
fi

INPUT=$(cat)
COMMAND=$(printf '%s' "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null) || COMMAND=""
CWD=$(printf '%s' "$INPUT" | jq -r '.cwd // empty' 2>/dev/null) || CWD=""
[ -z "$COMMAND" ] && exit 0

block() {
  echo "BLOCKED: '$ORIGINAL' is not allowed: $1. The user has prevented you from doing this; do not retry or work around it, ask the user instead." >&2
  exit 2
}

current_branch() {
  if [ -n "${GIT_GUARDRAILS_CURRENT_BRANCH+x}" ]; then
    printf '%s' "$GIT_GUARDRAILS_CURRENT_BRANCH"
    return
  fi
  git -C "${CWD:-.}" symbolic-ref --short -q HEAD 2>/dev/null || true
}

is_protected() { # main/master, optionally as HEAD:x, +x, refs/heads/x
  local r=${1#+}
  r=${r##*:}
  r=${r#refs/heads/}
  [ "$r" = main ] || [ "$r" = master ]
}

# Short-option cluster check: has_short "-fdx" f -> true if the cluster contains f.
has_short() { [[ $1 =~ ^-[A-Za-z]+$ && $1 == *"$2"* ]]; }

check_git() { # $1 = subcommand, rest = its arguments
  local sub=$1; shift
  local a p pos=() has_f=0 has_n=0 has_staged=0 has_worktree=0 has_d=0 has_D=0 dd=0
  for a in "$@"; do
    if [ "$dd" = 1 ]; then pos+=("$a"); continue; fi
    case "$a" in
      --) dd=1 ;;
      --force|--force-with-lease*|--force-if-includes) has_f=1 ;;
      --dry-run) has_n=1 ;;
      --staged) has_staged=1 ;;
      --worktree) has_worktree=1 ;;
      --delete) has_d=1 ;;
      --*) ;;
      -*) has_short "$a" f && has_f=1
          has_short "$a" n && has_n=1
          has_short "$a" S && has_staged=1
          has_short "$a" W && has_worktree=1
          has_short "$a" d && has_d=1
          has_short "$a" D && has_D=1 ;;
      *) pos+=("$a") ;;
    esac
  done
  case "$sub" in
    push)
      [ $has_f = 1 ] && block "force push"
      [ $has_d = 1 ] && block "branch deletion via push"
      for a in "$@"; do
        case "$a" in --mirror|--prune|--all) block "push option $a touches every ref" ;; esac
      done
      local i=0 target_seen=0
      for p in "${pos[@]+"${pos[@]}"}"; do
        i=$((i + 1))
        [ $i -eq 1 ] && continue # first positional is the remote
        target_seen=1
        [[ $p == +* ]] && block "forced refspec '$p'"
        [[ $p == :* ]] && block "remote ref deletion '$p'"
        if [ "$p" = HEAD ]; then
          is_protected "$(current_branch)" && block "push of main/master"
        elif is_protected "$p"; then
          block "push to main/master"
        fi
      done
      if [ $target_seen = 0 ] && is_protected "$(current_branch)"; then
        block "push while on main/master"
      fi
      ;;
    reset)
      for a in "$@"; do [ "$a" = --hard ] && block "git reset --hard"; done
      ;;
    clean)
      if [ $has_f = 1 ] && [ $has_n = 0 ]; then block "git clean with force"; fi
      ;;
    branch)
      [ $has_D = 1 ] && block "git branch -D"
      [ $has_d = 1 ] && [ $has_f = 1 ] && block "forced branch delete"
      ;;
    checkout|restore)
      if [ "$sub" = restore ] && [ $has_staged = 1 ] && [ $has_worktree = 0 ]; then return 0; fi
      [ "$sub" = checkout ] && [ $has_f = 1 ] && block "git checkout --force discards local changes"
      for p in "${pos[@]+"${pos[@]}"}"; do
        case "$p" in
          .|./|:/|:/.|\*|:\(top\)|:\(top\).|:/\*) block "git $sub of the whole worktree discards all local changes" ;;
        esac
      done
      ;;
  esac
  return 0
}

# One simple command (already split on separators).
check_segment() {
  local seg=$1 depth=$2
  local -a t
  read -r -a t <<<"$seg"
  local i=0 n=${#t[@]}
  # Strip leading VAR=val assignments and common wrappers.
  while [ $i -lt $n ]; do
    case "${t[$i]}" in
      *=*|sudo|env|command|nohup|time|exec|xargs|builtin) i=$((i + 1)) ;;
      *) break ;;
    esac
  done
  [ $i -ge $n ] && return 0
  local first=${t[$i]##*/}
  case "$first" in
    bash|sh|zsh|dash|eval)
      if [ "$depth" -lt 4 ]; then
        local rest="${t[*]:$((i + 1))}"
        rest=${rest//[\"\']/}
        [ "$first" != eval ] && rest=${rest#-c }
        check_command "$rest" $((depth + 1))
      fi
      return 0 ;;
    git) ;;
    *) return 0 ;;
  esac
  i=$((i + 1))
  # Skip git global options.
  while [ $i -lt $n ]; do
    case "${t[$i]}" in
      -C|-c|--git-dir|--work-tree|--namespace) i=$((i + 2)) ;;
      -*) i=$((i + 1)) ;;
      *) break ;;
    esac
  done
  [ $i -ge $n ] && return 0
  check_git "${t[$i]}" "${t[@]:$((i + 1))}"
}

check_command() {
  local cmd=$1 depth=${2:-0}
  cmd=${cmd//\$\(/$'\n'}
  cmd=${cmd//\`/$'\n'}
  cmd=${cmd//&&/$'\n'}
  cmd=${cmd//||/$'\n'}
  cmd=${cmd//;/$'\n'}
  cmd=${cmd//|/$'\n'}
  cmd=${cmd//&/$'\n'}
  cmd=${cmd//(/ }
  cmd=${cmd//)/ }
  local line
  while IFS= read -r line; do
    [ -n "${line//[[:space:]]/}" ] && check_segment "$line" "$depth"
  done <<<"$cmd"
  return 0
}

ORIGINAL=$COMMAND
check_command "$COMMAND" 0
exit 0
