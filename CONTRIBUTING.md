# Contributing to Nurtuo

Thanks for helping. Please read this before opening a pull request.

## Contributor License Agreement (CLA)

Every contributor must sign the **Nurtuo CLA** once. CLA Assistant comments on your first pull
request with a link; signing takes one click with your GitHub account. Pull requests cannot be
merged until the CLA check is green. The CLA grants the project the right to license your
contribution under AGPL-3.0, Apache-2.0 or other licenses in the future; you keep your copyright.

## Workflow

- Trunk-based: branch from `main`, keep branches short-lived, open a PR early.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat(runtime): ...`, `fix(canvas): ...`). PRs are squash-merged.
- Every PR must pass CI: lint, tests, proto checks, license scan, boundary check, CLA.
- Changes to `proto/` must pass `buf breaking`. Changes to Apache-2.0 packages that users consume
  need a changeset (`pnpm changeset`) or a note for the Go module tag.

## Licensing of your change

Put code in the right directory: server-side code under `core/` (AGPL-3.0); SDKs, connectors,
bridges, CLI and contracts in the Apache-2.0 directories. Apache-2.0 code must never import from
`core/`. Start every new source file with an SPDX header, for example:

```go
// SPDX-License-Identifier: AGPL-3.0-only
```

## Local setup

```sh
make bootstrap
make dev
make test
```

## Reporting security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
