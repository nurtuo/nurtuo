<!-- SPDX-License-Identifier: Apache-2.0 -->
# Lessons

Durable rules learned from real incidents in this repository. Hard boundaries live in
[AGENTS.md](../../AGENTS.md); a lesson holds the evidence behind one rule.

## How to use

- Route the task first: package, then area, then topic.
- Open only the records whose `packages`, `areas` or `topics` match. Never bulk-read `docs/lessons/`.
- After a correction, add or update **one** record. Do not add batches of speculative records.
- Every record points at a real file, issue, CI job or commit. No evidence, no lesson.

## Record format

File: `docs/lessons/<kebab-case-slug>.md`, starting with the frontmatter. Lesson records are the exception to the "SPDX first" rule: `---` comes first so the YAML is recognized as frontmatter, and the SPDX comment follows the closing `---`.

```markdown
---
title: "Short imperative or factual rule"
packages: ["core", "connectors"]
areas: ["architecture", "testing"]
topics: ["contracts", "sftp", "migrations"]
---
<!-- SPDX-License-Identifier: Apache-2.0 -->

# {Same title}

## Context
{Where in the system this showed up, in one short paragraph.}

## Problem
{What went wrong, with a pointer to the failing test, review, issue, or code.}

## Rule
{The durable rule, written so an agent can follow it next time. MUST / MUST NOT where it is a hard boundary.}

## How to verify
{The command, test, or review check that proves the rule holds.}
```

## Catalog

Row shape: `- [Title](slug.md) — area:a,b; package:x,y; topic:p,q`

- [Do not name a proto message Descriptor](proto-message-name-descriptor-breaks-csharp.md) — area:contracts,ci; package:proto,bridges; topic:proto,csharp,compatibility
