// SPDX-License-Identifier: Apache-2.0
using Google.Protobuf.WellKnownTypes;
using Grpc.Core;
using Microsoft.Extensions.Logging;
using Nurtuo.Connector.V1;

namespace Nurtuo.Bridge;

/// <summary>Adapts a <see cref="ConnectorBase"/> to the generated gRPC service.</summary>
public sealed class ConnectorGrpcService(ConnectorBase connector, BridgeOptions options, ILogger<ConnectorGrpcService> log)
    : ConnectorService.ConnectorServiceBase
{
    private ConnectionSettings Settings(ConnectionConfig? cfg) => new(cfg?.Settings, connector.SecretKeys);

    public override Task<DescribeResponse> Describe(DescribeRequest request, ServerCallContext context)
    {
        Descriptor d;
        try
        {
            d = connector.Describe().Clone();
        }
        catch (Exception e)
        {
            log.LogError(e, "describe failed");
            throw new RpcException(new Status(StatusCode.Internal, $"describe failed: {e.GetType().Name}"));
        }
        if (d.Host == Host.Unspecified) d.Host = Host.BridgeDotnet;
        return Task.FromResult(new DescribeResponse { Descriptor_ = d });
    }

    public override async Task<TestResponse> Test(TestRequest request, ServerCallContext context)
    {
        var settings = Settings(request.Connection);
        try
        {
            var r = await connector.TestAsync(settings, context.CancellationToken);
            return new TestResponse { Ok = r.Ok, Diagnostic = settings.Redactor.Apply(r.Diagnostic) };
        }
        catch (Exception e) when (e is not OperationCanceledException)
        {
            // A failed test is a result, not an RPC error.
            log.LogError("test failed: {Error}", settings.Redactor.Apply(e.GetType().Name + ": " + e.Message));
            return new TestResponse { Ok = false, Diagnostic = settings.Redactor.Apply($"{e.GetType().Name}: {e.Message}") };
        }
    }

    public override async Task<BrowseResponse> Browse(BrowseRequest request, ServerCallContext context)
    {
        var settings = Settings(request.Connection);
        BrowseResult r;
        try
        {
            r = await connector.BrowseAsync(
                new BrowseContext(settings, request.OperationType, request.CustomType, request.ObjectTypeId), context.CancellationToken);
        }
        catch (Exception e) when (e is BrowseNotSupportedException or NotImplementedException)
        {
            throw new RpcException(new Status(StatusCode.Unimplemented, "browse is not supported by this connector"));
        }
        catch (Exception e) when (e is not OperationCanceledException)
        {
            log.LogError("browse failed: {Error}", settings.Redactor.Apply(e.GetType().Name + ": " + e.Message));
            throw new RpcException(new Status(StatusCode.Internal, settings.Redactor.Apply($"{e.GetType().Name}: {e.Message}")));
        }
        var resp = new BrowseResponse();
        foreach (var (id, label) in r.ObjectTypes ?? []) resp.ObjectTypes.Add(new ObjectType { Id = id, Label = label });
        if (r.RequestProfile is { } rp) resp.RequestProfile = new Profile { Format = rp.Format, Definition = Google.Protobuf.ByteString.CopyFrom(rp.Definition) };
        if (r.ResponseProfile is { } sp) resp.ResponseProfile = new Profile { Format = sp.Format, Definition = Google.Protobuf.ByteString.CopyFrom(sp.Definition) };
        return resp;
    }

    public override async Task Execute(IAsyncStreamReader<ExecuteRequest> requestStream, IServerStreamWriter<ExecuteResponse> responseStream, ServerCallContext context)
    {
        var ct = context.CancellationToken;
        if (!await requestStream.MoveNext(ct) || requestStream.Current.MsgCase != ExecuteRequest.MsgOneofCase.Start)
            throw new RpcException(new Status(StatusCode.InvalidArgument, "the first Execute message must be ExecuteStart"));
        var start = requestStream.Current.Start;
        var settings = Settings(start.Connection);
        var ctx = new ExecuteContext(
            start.ExecutionId, start.StepId, settings, start.OperationType, start.CustomType, start.ObjectTypeId,
            start.Options is null ? new Dictionary<string, object?>() : (Dictionary<string, object?>)Json.ToObject(start.Options),
            options.ChunkSize);
        var emitter = new Emitter(responseStream, settings.Redactor);
        try
        {
            await connector.ExecuteAsync(ctx, InputDocument.IterateAsync(requestStream, ct), emitter, ct);
        }
        catch (ProtocolException e)
        {
            throw new RpcException(new Status(StatusCode.InvalidArgument, settings.Redactor.Apply(e.Message)));
        }
        catch (Exception e) when (e is not (RpcException or OperationCanceledException))
        {
            log.LogError("execute failed: {Error}", settings.Redactor.Apply(e.GetType().Name + ": " + e.Message));
            throw new RpcException(new Status(StatusCode.Internal, settings.Redactor.Apply($"{e.GetType().Name}: {e.Message}")));
        }
    }

    private sealed class Emitter(IServerStreamWriter<ExecuteResponse> writer, Redactor redactor) : IEmitter
    {
        public Task ResultAsync(string documentId, DocumentStatus status, string message = "",
            IReadOnlyDictionary<string, string>? tracked = null, IEnumerable<string>? outputIds = null)
        {
            var r = new DocumentResult { DocumentId = documentId, Status = status, Message = redactor.Apply(message) };
            foreach (var (k, v) in tracked ?? new Dictionary<string, string>()) r.Tracked[k] = redactor.Apply(v);
            if (outputIds is not null) r.OutputDocumentIds.AddRange(outputIds);
            return writer.WriteAsync(new ExecuteResponse { Result = r });
        }

        public async Task OutputAsync(OutputDocument output, ExecuteContext ctx, CancellationToken ct)
        {
            await foreach (var chunk in Chunker.ChunkAsync(output.Id, output.Properties, output.Data, ctx.ChunkSize, ct))
                await writer.WriteAsync(new ExecuteResponse { Output = chunk }, ct);
        }
    }
}
