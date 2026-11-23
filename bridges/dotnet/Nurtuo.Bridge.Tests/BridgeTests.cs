// SPDX-License-Identifier: Apache-2.0
using Google.Protobuf;
using Google.Protobuf.WellKnownTypes;
using Grpc.Core;
using Grpc.Net.Client;
using Nurtuo.Bridge;
using Nurtuo.Bridge.Examples;
using Nurtuo.Connector.V1;
using Xunit;

namespace Nurtuo.Bridge.Tests;

/// <summary>In-process tests: the bridge listens on loopback TCP and a real gRPC client drives it.</summary>
public sealed class BridgeTests : IAsyncLifetime
{
    private RunningBridge _bridge = null!;
    private GrpcChannel _channel = null!;
    private ConnectorService.ConnectorServiceClient _client = null!;

    public async Task InitializeAsync()
    {
        _bridge = await BridgeHost.StartAsync(new EchoConnector(), new BridgeOptions { ChunkSize = 10_000 });
        _channel = GrpcChannel.ForAddress(_bridge.Uri);
        _client = new ConnectorService.ConnectorServiceClient(_channel);
    }

    public async Task DisposeAsync()
    {
        _channel.Dispose();
        await _bridge.DisposeAsync();
    }

    private static ConnectionConfig Conn(params (string Key, string Value)[] kv)
    {
        var s = new Struct();
        foreach (var (k, v) in kv) s.Fields[k] = Value.ForString(v);
        return new ConnectionConfig { Settings = s };
    }

    private sealed record Output(Dictionary<string, DocumentResult> Results, Dictionary<string, List<DocumentChunk>> Chunks)
    {
        public byte[] Data(string id) => Chunks[id].SelectMany(c => c.Data.ToByteArray()).ToArray();
    }

    private async Task<Output> RunAsync(string custom, ConnectionConfig conn, params (string Id, byte[] Data)[] docs)
    {
        using var call = _client.Execute();
        // Execute is full duplex: the server streams output while it still reads input. Drain the response
        // concurrently, otherwise both sides fill the HTTP/2 flow-control windows and deadlock on large payloads.
        var results = new Dictionary<string, DocumentResult>();
        var chunks = new Dictionary<string, List<DocumentChunk>>();
        var reading = Task.Run(async () =>
        {
            await foreach (var r in call.ResponseStream.ReadAllAsync())
            {
                if (r.MsgCase == ExecuteResponse.MsgOneofCase.Result)
                {
                    Assert.True(results.TryAdd(r.Result.DocumentId, r.Result), "duplicate result");
                }
                else if (r.MsgCase == ExecuteResponse.MsgOneofCase.Output)
                {
                    if (!chunks.TryGetValue(r.Output.DocumentId, out var l)) chunks[r.Output.DocumentId] = l = new();
                    l.Add(r.Output);
                }
            }
        });

        await call.RequestStream.WriteAsync(new ExecuteRequest
        {
            Start = new ExecuteStart { ExecutionId = "e", StepId = "s", Connection = conn, OperationType = OperationType.Execute, CustomType = custom },
        });
        foreach (var (id, data) in docs)
        {
            const int piece = 30_000;
            var n = Math.Max(1, (data.Length + piece - 1) / piece);
            for (var i = 0; i < n; i++)
            {
                var part = data.Skip(i * piece).Take(piece).ToArray();
                await call.RequestStream.WriteAsync(new ExecuteRequest
                {
                    Chunk = new DocumentChunk { DocumentId = id, Data = ByteString.CopyFrom(part), Last = i == n - 1 },
                });
            }
        }
        await call.RequestStream.WriteAsync(new ExecuteRequest { End = new ExecuteEnd() });
        await call.RequestStream.CompleteAsync();

        await reading;
        return new Output(results, chunks);
    }

    [Fact]
    public async Task Describe_reports_the_dotnet_host()
    {
        var d = (await _client.DescribeAsync(new DescribeRequest())).Descriptor_;
        Assert.Equal("example-echo-dotnet", d.Id);
        Assert.Equal(Host.BridgeDotnet, d.Host);
        Assert.Equal(new[] { "ECHO", "UPPERCASE" }, d.Operations.Select(o => o.CustomType).ToArray());
    }

    [Fact]
    public async Task Test_redacts_secrets()
    {
        var r = await _client.TestAsync(new TestRequest { Connection = Conn(("api_token", "tok-SECRET-123")) });
        Assert.True(r.Ok);
        Assert.DoesNotContain("tok-SECRET-123", r.Diagnostic);
    }

    [Fact]
    public async Task Execute_returns_per_document_statuses_and_isolates_failures()
    {
        var o = await RunAsync("UPPERCASE", Conn(("api_token", "tok-SECRET-123")),
            ("ok", "hello"u8.ToArray()),
            ("rej", "!reject please"u8.ToArray()),
            ("crash", "!crash"u8.ToArray()),
            ("leak", "!leak"u8.ToArray()),
            ("pl", "zażółć"u8.ToArray()));

        Assert.Equal(DocumentStatus.Success, o.Results["ok"].Status);
        Assert.Equal(DocumentStatus.ApplicationError, o.Results["rej"].Status);
        Assert.Equal(DocumentStatus.Failure, o.Results["crash"].Status);
        Assert.Equal(DocumentStatus.Failure, o.Results["leak"].Status);
        Assert.Equal(DocumentStatus.Success, o.Results["pl"].Status);
        Assert.Equal("HELLO", System.Text.Encoding.UTF8.GetString(o.Data("ok.out1")));
        Assert.Equal("ZAŻÓŁĆ", System.Text.Encoding.UTF8.GetString(o.Data("pl.out1")));
        Assert.Equal("5", o.Results["ok"].Tracked["bytes"]);
        Assert.DoesNotContain("tok-SECRET-123", o.Results["leak"].Message);
        Assert.Contains("boom", o.Results["crash"].Message);
    }

    [Fact]
    public async Task Large_payload_is_chunked_with_bounded_chunks()
    {
        var payload = Enumerable.Range(0, 1 << 20).Select(i => (byte)i).ToArray();
        var o = await RunAsync("ECHO", Conn(), ("big", payload));
        Assert.Equal(payload, o.Data("big.out1"));
        var chunks = o.Chunks["big.out1"];
        Assert.All(chunks, c => Assert.True(c.Data.Length <= 10_000));
        Assert.Equal(chunks.Count - 1, chunks.Count(c => !c.Last));
        Assert.True(chunks[^1].Last);
        Assert.Equal("ECHO", chunks[0].Properties["echo.operation"]);
    }

    [Fact]
    public async Task Empty_document_and_empty_batch()
    {
        var o = await RunAsync("ECHO", Conn(), ("empty", Array.Empty<byte>()));
        Assert.Equal(DocumentStatus.Success, o.Results["empty"].Status);
        Assert.Empty(o.Data("empty.out1"));
        Assert.Empty((await RunAsync("ECHO", Conn())).Results);
    }

    [Fact]
    public async Task Execute_must_start_with_ExecuteStart()
    {
        using var call = _client.Execute();
        await call.RequestStream.WriteAsync(new ExecuteRequest { End = new ExecuteEnd() });
        await call.RequestStream.CompleteAsync();
        var e = await Assert.ThrowsAsync<RpcException>(async () => { await foreach (var _ in call.ResponseStream.ReadAllAsync()) { } });
        Assert.Equal(StatusCode.InvalidArgument, e.StatusCode);
    }

    [Fact]
    public async Task Interleaved_documents_fail_the_batch()
    {
        using var call = _client.Execute();
        await call.RequestStream.WriteAsync(new ExecuteRequest { Start = new ExecuteStart { Connection = Conn() } });
        await call.RequestStream.WriteAsync(new ExecuteRequest { Chunk = new DocumentChunk { DocumentId = "a", Data = ByteString.CopyFromUtf8("1") } });
        await call.RequestStream.WriteAsync(new ExecuteRequest { Chunk = new DocumentChunk { DocumentId = "b", Data = ByteString.CopyFromUtf8("2"), Last = true } });
        await call.RequestStream.CompleteAsync();
        var e = await Assert.ThrowsAsync<RpcException>(async () => { await foreach (var _ in call.ResponseStream.ReadAllAsync()) { } });
        Assert.Equal(StatusCode.InvalidArgument, e.StatusCode);
    }

    [Fact]
    public async Task Browse_is_unimplemented_for_echo()
    {
        var e = await Assert.ThrowsAsync<RpcException>(async () => await _client.BrowseAsync(new BrowseRequest()));
        Assert.Equal(StatusCode.Unimplemented, e.StatusCode);
    }

    [Fact]
    public async Task Health_is_serving()
    {
        // Raw call so that the test needs no health client package: Check(HealthCheckRequest{}) -> {status: SERVING = 1}.
        var m = Marshallers.Create(b => b, b => b);
        var check = new Method<byte[], byte[]>(MethodType.Unary, "grpc.health.v1.Health", "Check", m, m);
        var resp = await _channel.CreateCallInvoker().AsyncUnaryCall(check, null, new CallOptions(), Array.Empty<byte>());
        Assert.Equal(new byte[] { 0x08, 0x01 }, resp);
    }

    [Fact]
    public async Task Token_is_enforced()
    {
        await using var secured = await BridgeHost.StartAsync(new EchoConnector(), new BridgeOptions { Token = "s3cret-token" });
        using var channel = GrpcChannel.ForAddress(secured.Uri);
        var client = new ConnectorService.ConnectorServiceClient(channel);

        var denied = await Assert.ThrowsAsync<RpcException>(async () => await client.DescribeAsync(new DescribeRequest()));
        Assert.Equal(StatusCode.Unauthenticated, denied.StatusCode);

        var ok = await client.DescribeAsync(new DescribeRequest(), new Metadata { { BridgeProtocol.TokenHeader, "s3cret-token" } });
        Assert.Equal("example-echo-dotnet", ok.Descriptor_.Id);
    }

    [Fact]
    public void Contract_negotiation_picks_the_first_supported_in_runtime_order()
    {
        Assert.Equal(BridgeProtocol.Contract, BridgeProtocol.Negotiate(null));
        Assert.Equal(BridgeProtocol.Contract, BridgeProtocol.Negotiate("nurtuo.connector.v2,nurtuo.connector.v1"));
        Assert.Null(BridgeProtocol.Negotiate("nurtuo.connector.v2"));
    }

    [Fact]
    public void Handshake_line_shape()
    {
        Assert.StartsWith("NURTUO-BRIDGE|1|tcp|127.0.0.1:1234|nurtuo.connector.v1|dotnet/", BridgeProtocol.FormatHandshake("tcp", "127.0.0.1:1234"));
    }

    [Fact]
    public async Task Listening_beyond_loopback_is_refused()
    {
        await Assert.ThrowsAsync<ArgumentException>(() => BridgeHost.StartAsync(new EchoConnector(), new BridgeOptions { Listen = "tcp:0.0.0.0:0" }));
    }

    [Fact]
    public void Secrets_are_masked_in_text_and_in_ToString()
    {
        var cfg = new ConnectionSettings(Conn(("password", "s3cr3t-pw"), ("host", "db")).Settings);
        Assert.Equal("login *** on db", cfg.Redactor.Apply("login s3cr3t-pw on db"));
        Assert.DoesNotContain("s3cr3t-pw", cfg.ToString());
        Assert.DoesNotContain("s3cr3t-pw", cfg.Secret("password").ToString());
        Assert.Throws<InvalidOperationException>(() => cfg.Get<string>("password"));
        Assert.Equal("db", cfg.Get<string>("host"));
    }
}
