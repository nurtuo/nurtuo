---
title: "Do not name a proto message Descriptor"
packages: ["proto", "bridges"]
areas: ["contracts", "ci"]
topics: ["proto", "csharp", "compatibility"]
---
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Do not name a proto message Descriptor

## Context
`proto/nurtuo/connector/v1/connector.proto` defines the connector message `ConnectorDescriptor` (formerly `Descriptor`, renamed in https://github.com/nurtuo/nurtuo/pull/41). The .NET bridge (`bridges/dotnet`) compiles the C# code protoc generates from it.

## Problem
protoc's C# generator gives every message class a static `Descriptor` property, so a message named `Descriptor` yields a member with the same name as its class (CS0542) and the .NET bridge does not compile. The generator has no option to rename the class. Tracked in https://github.com/nurtuo/nurtuo/issues/38 and fixed by the rename in #41; the `bridge-dotnet` job in `.github/workflows/ci.yml` is now a blocking check.

## Rule
A proto message MUST NOT be named `Descriptor`. Before naming a new proto message, check that the name does not collide with a member protoc generates for the .NET bridge. Renaming a message is a contract change: it needs owner approval and a `buf breaking` decision (see issue #38).

## How to verify
`buf lint` and `buf breaking` for the proto change; the blocking `bridge-dotnet` CI job passes.
