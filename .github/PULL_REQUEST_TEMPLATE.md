## What and why

## Source spec
Source spec: docs/specs/YYYY-MM-DD-<topic>.md <!-- or "n/a" for small fixes (bug fix, refactor, dependency bump, CI tweak, docs-only) -->

## Linked issue
Closes #<!-- issue number -->

## How it was tested

## Tests
- [ ] Tests are in this PR
- [ ] Commands I ran are listed under "How it was tested"
- [ ] n/a (docs-only or no behaviour change)

## Checklist
- [ ] Code is in the right license directory (`core/` = AGPL-3.0, others = Apache-2.0) with SPDX headers
- [ ] `proto/` changes pass `buf breaking` and generated code is updated (`make gen`)
- [ ] Hot-path changes include benchmark results (`make bench`)
- [ ] CLA signed (CLA Assistant comments on your first PR)
