// SPDX-License-Identifier: Apache-2.0
using Google.Protobuf.WellKnownTypes;
using Microsoft.Extensions.Logging;
using Nurtuo.Connector.V1;

namespace Nurtuo.Bridge;

/// <summary>Technical failure of one document (becomes DOCUMENT_STATUS_FAILURE).</summary>
public class ConnectorException(string message, Exception? inner = null) : Exception(message, inner);

/// <summary>
/// The target system rejected the document (becomes DOCUMENT_STATUS_APPLICATION_ERROR). The message
/// is returned to the flow, so keep secrets out of it (known secret values are redacted regardless).
/// </summary>
public sealed class ApplicationErrorException(string message) : ConnectorException(message);

/// <summary>Thrown (or inherited) by connectors without browse support; maps to UNIMPLEMENTED.</summary>
public sealed class BrowseNotSupportedException : Exception;

public sealed record TestResult(bool Ok, string Diagnostic = "");

public sealed record BrowseContext(ConnectionSettings Settings, OperationType OperationType, string CustomType, string ObjectTypeId);

public sealed record BrowseProfile(byte[] Definition, Profile.Types.Format Format = Profile.Types.Format.JsonSchema);

public sealed record BrowseResult(
    IReadOnlyList<(string Id, string Label)>? ObjectTypes = null,
    BrowseProfile? RequestProfile = null,
    BrowseProfile? ResponseProfile = null);

public sealed record ExecuteContext(
    string ExecutionId,
    string StepId,
    ConnectionSettings Settings,
    OperationType OperationType,
    string CustomType,
    string ObjectTypeId,
    IReadOnlyDictionary<string, object?> Options,
    int ChunkSize);

/// <summary>Result of one document.</summary>
public sealed class Outcome
{
    public DocumentStatus Status { get; init; } = DocumentStatus.Success;
    public string Message { get; init; } = "";
    /// <summary>Evaluated after the outputs were streamed (for values only known by then).</summary>
    public Func<IReadOnlyDictionary<string, string>>? Tracked { get; init; }
    public IReadOnlyList<OutputDocument> Outputs { get; init; } = Array.Empty<OutputDocument>();
}

/// <summary>Where <see cref="ConnectorBase.ExecuteAsync"/> sends results and output chunks.</summary>
public interface IEmitter
{
    /// <summary>Exactly one result per input document.</summary>
    Task ResultAsync(string documentId, DocumentStatus status, string message = "",
        IReadOnlyDictionary<string, string>? tracked = null, IEnumerable<string>? outputIds = null);

    /// <summary>An output document, chunked (at most <c>ChunkSize</c> bytes per chunk).</summary>
    Task OutputAsync(OutputDocument output, ExecuteContext ctx, CancellationToken ct);
}

/// <summary>Base class of every .NET connector.</summary>
public abstract class ConnectorBase
{
    /// <summary>Connection setting keys that hold secrets; null guesses from the key name.</summary>
    public virtual IReadOnlySet<string>? SecretKeys => null;

    /// <summary>Operations, connection schema and capabilities. Cheap and side-effect free.</summary>
    public abstract Descriptor Describe();

    /// <summary>Check reachability. Redact the diagnostic.</summary>
    public abstract Task<TestResult> TestAsync(ConnectionSettings settings, CancellationToken ct);

    /// <summary>List object types / return profiles. Optional.</summary>
    public virtual Task<BrowseResult> BrowseAsync(BrowseContext ctx, CancellationToken ct) =>
        throw new BrowseNotSupportedException();

    /// <summary>
    /// Process one document (simple connectors override this). Throw <see cref="ApplicationErrorException"/>
    /// for business rejections; any other exception is a technical failure of this document only.
    /// Outputs may stream lazily from <paramref name="doc"/>.
    /// </summary>
    public virtual Task<Outcome?> ExecuteDocumentAsync(ExecuteContext ctx, InputDocument doc, CancellationToken ct) =>
        throw new NotImplementedException("override ExecuteDocumentAsync or ExecuteAsync");

    /// <summary>
    /// Process a batch. Default: <see cref="ExecuteDocumentAsync"/> per document with errors isolated.
    /// Contract: one result per input document, plus any number of output chunks. Throwing fails the whole batch.
    /// </summary>
    public virtual async Task ExecuteAsync(ExecuteContext ctx, IAsyncEnumerable<InputDocument> documents, IEmitter emit, CancellationToken ct)
    {
        await foreach (var doc in documents.WithCancellation(ct))
        {
            await ExecuteOneAsync(ctx, doc, emit, ct);
        }
    }

    private async Task ExecuteOneAsync(ExecuteContext ctx, InputDocument doc, IEmitter emit, CancellationToken ct)
    {
        var redact = ctx.Settings.Redactor;
        try
        {
            var outcome = await ExecuteDocumentAsync(ctx, doc, ct) ?? new Outcome();
            var ids = new List<string>();
            var n = 0;
            foreach (var o in outcome.Outputs)
            {
                n++;
                var withId = string.IsNullOrEmpty(o.Id) ? new OutputDocument(o.Data, o.Properties, $"{doc.Id}.out{n}") : o;
                ids.Add(withId.Id);
                // Streamed here, inside the try: an error while a lazy output is produced fails this document only.
                await emit.OutputAsync(withId, ctx, ct);
            }
            var tracked = outcome.Tracked?.Invoke() ?? new Dictionary<string, string>();
            await emit.ResultAsync(doc.Id, outcome.Status, redact.Apply(outcome.Message), tracked, ids);
        }
        catch (ProtocolException)
        {
            throw; // the stream itself is broken: fail the batch, not just this document
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (ApplicationErrorException e)
        {
            await emit.ResultAsync(doc.Id, DocumentStatus.ApplicationError, redact.Apply(e.Message));
        }
        catch (Exception e)
        {
            // Details go to stderr (redacted); the flow only sees the exception type and message.
            BridgeLog.Logger.LogError("document {Id} failed: {Error}", doc.Id, redact.Apply(e.GetType().Name + ": " + e.Message));
            await emit.ResultAsync(doc.Id, DocumentStatus.Failure, redact.Apply($"{e.GetType().Name}: {e.Message}"));
        }
    }

    // ---- descriptor helpers ----

    protected static OperationDescriptor Operation(OperationType type, string customType = "", bool supportsBrowse = false, Struct? optionsSchema = null)
    {
        var op = new OperationDescriptor { Type = type, CustomType = customType, SupportsBrowse = supportsBrowse };
        if (optionsSchema is not null) op.OptionsSchema = optionsSchema;
        return op;
    }

    protected static Descriptor MakeDescriptor(
        string id, string version, string displayName, IEnumerable<OperationDescriptor> operations,
        Struct? connectionSchema = null, IEnumerable<string>? trackedProperties = null,
        bool streaming = true, uint maxBatchSize = 0, bool transactional = false)
    {
        var d = new Descriptor
        {
            Id = id,
            Version = version,
            Host = Host.BridgeDotnet,
            DisplayName = displayName,
            Capabilities = new Capabilities { Streaming = streaming, MaxBatchSize = maxBatchSize, Transactional = transactional },
        };
        d.Operations.AddRange(operations);
        if (trackedProperties is not null) d.TrackedProperties.AddRange(trackedProperties);
        if (connectionSchema is not null) d.ConnectionSchema = connectionSchema;
        return d;
    }
}

internal static class BridgeLog
{
    public static ILogger Logger { get; set; } = Microsoft.Extensions.Logging.Abstractions.NullLogger.Instance;
}
