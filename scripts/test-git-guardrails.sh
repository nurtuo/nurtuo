#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Feeds Claude Code PreToolUse JSON into scripts/hooks/block-dangerous-git.sh and asserts that
# destructive git commands are blocked (exit 2) and ordinary ones allowed (exit 0).
#
#   scripts/test-git-guardrails.sh
set -uo pipefail
cd "$(dirname "$0")/.."
HOOK=scripts/hooks/block-dangerous-git.sh
fail=0 total=0

run() { # run <branch> <command>; prints exit code
  jq -n --arg c "$2" '{tool_name:"Bash",tool_input:{command:$c},cwd:"."}' \
    | GIT_GUARDRAILS_CURRENT_BRANCH="$1" "$HOOK" >/dev/null 2>&1
  echo $?
}
expect() { # expect <want-exit> <branch> <command>
  total=$((total + 1))
  local got; got=$(run "$2" "$3")
  if [ "$got" != "$1" ]; then
    echo "FAIL (want $1, got $got) [on $2]: $3"; fail=$((fail + 1))
  fi
}
blocked() { expect 2 "${BR:-feat/x}" "$1"; }
allowed() { expect 0 "${BR:-feat/x}" "$1"; }

# --- blocked ---
blocked 'git push --force'
blocked 'git push -f'
blocked 'git push -f origin feat/x'
blocked 'git push origin feat/x --force'
blocked 'git push --force-with-lease'
blocked 'git push --force-with-lease=feat/x origin feat/x'
blocked 'git push -uf origin feat/x'
blocked 'git push origin +feat/x'
blocked 'git push origin :feat/x'
blocked 'git push origin --delete feat/x'
blocked 'git push --mirror'
blocked 'git push --all'
blocked 'git push origin main'
blocked 'git push -u origin master'
blocked 'git push origin HEAD:main'
blocked 'git push origin feat/x:refs/heads/main'
blocked 'git -C . push origin main'
blocked '/usr/bin/git push --force'
blocked 'git reset --hard'
blocked 'git reset --hard HEAD~3'
blocked 'git clean -f'
blocked 'git clean -fd'
blocked 'git clean -xfd'
blocked 'git clean --force'
blocked 'git branch -D feat/x'
blocked 'git branch -fD feat/x'
blocked 'git branch -d -f feat/x'
blocked 'git branch --delete --force feat/x'
blocked 'git checkout .'
blocked 'git checkout -- .'
blocked 'git checkout HEAD -- .'
blocked 'git checkout -f'
blocked 'git restore .'
blocked 'git restore -- .'
blocked 'git restore --source=HEAD --worktree .'
blocked 'git restore :/'
blocked 'git restore --staged --worktree .'
blocked 'git status && git reset --hard'
blocked 'echo hi; git push -f'
blocked 'FOO=1 git reset --hard'
blocked 'sudo git clean -fd'
blocked 'bash -c "git reset --hard"'
blocked 'cd x && (git push --force)'
blocked 'echo $(git clean -fd)'
blocked 'git push origin "main"'
blocked "git push origin 'main'"
blocked 'git push origin "HEAD:main"'
blocked 'git push origin "refs/heads/main"'
blocked 'git reset "--hard"'
blocked "git reset '--hard'"
blocked 'git reset --har\d'
blocked 'git push "--force" origin feat/x'
blocked "git push '-f' origin feat/x"
blocked 'git push origin "+feat/x"'
blocked 'git push origin "+HEAD:feat/x"'
blocked 'git push origin "--delete" feat/x'
blocked 'git push origin \main'
blocked 'git clean "-fdx"'
blocked "git clean '-f'"
blocked 'git branch "-D" foo'
blocked "git branch '-D' foo"
blocked 'git checkout "."'
blocked 'git restore "."'
blocked 'git "push" --force'
blocked 'bash -c "git push origin \"main\""'
blocked 'git commit -m "a; git push -f"' # known false positive: separators ignore quotes (pinned)
BR=main blocked 'git push origin "HEAD"'
BR=main blocked 'git push'
BR=main blocked 'git push origin'
BR=main blocked 'git push origin HEAD'
BR=master blocked 'git push -u origin HEAD'

# --- allowed ---
allowed 'git push'
allowed 'git push origin "feat/x"'
allowed "git push '-u' origin feat/x"
allowed 'git clean "-n"'
allowed 'git push -u origin feat/x'
allowed 'git push origin feat/agent-skills'
allowed 'git push -u origin feat/agent-skills'
allowed 'git push origin HEAD'
allowed 'git push origin feat/x:feat/x'
allowed 'git push --set-upstream origin feat/x'
allowed 'git push --dry-run origin feat/x'
allowed 'git push origin feat/domain-f'
allowed 'git status'
allowed 'git log --oneline -5'
allowed 'git diff'
allowed 'git add -A && git commit -m "feat: x"'
allowed 'git commit -m "docs: never git push -f or git reset --hard"'
allowed 'echo "git reset --hard"'
allowed 'git checkout feat/x'
allowed 'git checkout -b feat/y'
allowed 'git switch feat/x'
allowed 'git switch -c feat/y'
allowed 'git restore README.md'
allowed 'git restore --staged README.md'
allowed 'git restore --staged .'
allowed 'git checkout -- README.md'
allowed 'git reset HEAD~1'
allowed 'git reset --soft HEAD~1'
allowed 'git clean -n'
allowed 'git clean -nd'
allowed 'git clean -fn'
allowed 'git branch -d feat/merged'
allowed 'git branch feat/z'
allowed 'git branch --list'
allowed 'git fetch origin && git rebase origin/main'
allowed 'git stash'
allowed 'gh pr create --base docs/agents-md --title "chore: x" --body "git push -f is blocked"'
allowed 'ls -la'
BR=main allowed 'git status'
BR=main allowed 'git push origin feat/x'

# Empty/odd input must not crash the hook or block.
total=$((total + 1))
echo '{}' | "$HOOK" >/dev/null 2>&1 || { echo "FAIL: empty input should be allowed"; fail=$((fail + 1)); }

echo "git-guardrails: $((total - fail))/$total passed"
[ "$fail" -eq 0 ]
