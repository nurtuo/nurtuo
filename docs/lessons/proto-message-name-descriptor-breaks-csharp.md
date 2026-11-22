<!-- SPDX-License-Identifier: Apache-2.0 -->
---
title: "Do not name a proto message Descriptor"
packages: ["proto", "bridges"]
areas: ["contracts", "ci"]
topics: ["proto", "csharp", "compatibility"]
---

# Do not name a proto message Descriptor

## Context
`proto/nurtuo/connector/v1/connector.proto` defines the connector message `Descriptor`. The .NET bridge (`bridges/dotnet`) compiles the C# code protoc generates from it.

## Problem
protoc's C# generator gives every message class a static `Descriptor` property, so a message named `Descriptor` yields a member with the same name as its class (CS0542) and the .NET bridge does not compile. The generator has no option to rename the class. Tracked in https://github.com/nurtuo/nurtuo/issues/38; the `bridge-dotnet` job in `.github/workflows/ci.yml` is `continue-on-error: true` until it is fixed.

## Rule
A proto message MUST NOT be named `Descriptor`. Before naming a new proto message, check that the name does not collide with a member protoc generates for the .NET bridge. Renaming a message is a contract change: it needs owner approval and a `buf breaking` decision (see issue #38).

## How to verify
`buf lint` and `buf breaking` for the proto change; the `bridge-dotnet` CI job passes with `continue-on-error` removed.
