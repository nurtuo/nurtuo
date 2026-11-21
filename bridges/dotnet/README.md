# bridge-dotnet (Apache-2.0, optional)

.NET connector host (Grpc.AspNetCore), published self-contained per OS/arch. Implements the bridge
process protocol from [../README.md](../README.md).

> **Status: written but not built or run.** The machine this was authored on had no .NET SDK.
> Treat it as unverified: run `dotnet test Nurtuo.Bridge.Tests` first and expect small fixes
> (NuGet versions, generated member names such as `DescribeResponse.Descriptor_`).

## Layout

```
Directory.Build.props          net8.0, nullable, SPDX/license metadata
Nurtuo.Bridge/                 library
  ConnectorBase.cs             base class: Describe / TestAsync / BrowseAsync / ExecuteDocumentAsync / ExecuteAsync
  ConnectorGrpcService.cs      adapts ConnectorBase to the generated ConnectorService
  BridgeHost.cs                BridgeHost.RunAsync (process protocol) / StartAsync (in-process), token middleware, health
  Documents.cs                 lazy InputDocument, OutputDocument, chunker
  Secrets.cs                   Secret, ConnectionSettings, Redactor
Nurtuo.Bridge.Examples/        EchoConnector (ECHO, UPPERCASE) + Program.cs
Nurtuo.Bridge.Tests/           xunit, in-process Kestrel on loopback TCP + Grpc.Net.Client
```

Stubs are generated at build time by `Grpc.Tools` straight from `proto/nurtuo/connector/v1/connector.proto`
(no generated code is committed).

## Build, test, publish

```
dotnet test Nurtuo.Bridge.Tests
dotnet run --project Nurtuo.Bridge.Examples        # speaks the handshake on stdout
dotnet publish Nurtuo.Bridge.Examples -c Release -r win-x64 --self-contained -p:PublishSingleFile=true
```

## Writing a connector

Derive from `ConnectorBase`: `Describe()` (use `MakeDescriptor`/`Operation`), `TestAsync`, and
`ExecuteDocumentAsync(ctx, doc, ct)` returning an `Outcome` (throw `ApplicationErrorException` for
business rejections, anything else fails that document only). `doc.ChunksAsync()`, `doc.OpenRead()`
and `doc.ReadAllAsync(limit)` read the streamed payload; `OutputDocument` takes `byte[]` or an
`IAsyncEnumerable<ReadOnlyMemory<byte>>`. Override `ExecuteAsync(ctx, documents, emitter, ct)` for
batch-level control (exactly one result per document). Run with `BridgeHost.RunAsync(connector)`.
Unix sockets work on Windows 10+ with Kestrel, so `NURTUO_BRIDGE_LISTEN=unix:...` is available on
all platforms for this bridge; the launcher still defaults to loopback TCP on Windows.
