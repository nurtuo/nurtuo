#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Fails if Apache-2.0 code depends on AGPL code (core/), or if SPDX headers do not match the directory.
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
APACHE_DIRS=(proto sdk connectors bridges cli examples docs)

# 1. Go: Apache modules must not require or import the core module.
for d in sdk/go connectors; do
  if grep -rq "github.com/nurtuo/nurtuo/core" "$d" --include='*.go' --include='go.mod'; then
    echo "ERROR: $d (Apache-2.0) references github.com/nurtuo/nurtuo/core (AGPL)"; fail=1
  fi
done
# 2. TS: Apache packages must not depend on AGPL packages.
for p in sdk/ts/*/package.json cli/package.json; do
  if grep -Eq '"@nurtuo/(control-plane|web|mcp|ai)"' "$p"; then
    echo "ERROR: $p (Apache-2.0) depends on an AGPL package"; fail=1
  fi
done
# 3. No relative imports from Apache dirs into core/.
for d in "${APACHE_DIRS[@]}"; do
  if grep -rEn --include='*.ts' --include='*.tsx' "from ['\"](\.\./)+core/" "$d" 2>/dev/null; then
    echo "ERROR: relative import into core/ from $d"; fail=1
  fi
done
# 4. SPDX headers match the directory license.
while IFS= read -r f; do
  case "$f" in
    core/*) want="AGPL-3.0-only" ;;
    proto/*|sdk/*|connectors/*|bridges/*|cli/*|examples/*|docs/*) want="Apache-2.0" ;;
    *) want="AGPL-3.0-only" ;;
  esac
  case "$f" in */gen/*) continue ;; esac
  if ! head -n 3 "$f" | grep -q "SPDX-License-Identifier: $want"; then
    echo "ERROR: $f missing 'SPDX-License-Identifier: $want'"; fail=1
  fi
done < <(git ls-files '*.go' '*.ts' '*.tsx' '*.proto' 2>/dev/null || find . -name '*.go' -o -name '*.ts' -o -name '*.proto' | sed 's#^\./##' | grep -v -e node_modules -e /dist/)
[ $fail -eq 0 ] && echo "license boundaries OK"
exit $fail
