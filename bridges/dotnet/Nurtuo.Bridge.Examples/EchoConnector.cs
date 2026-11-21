// SPDX-License-Identifier: Apache-2.0
using System.Runtime.CompilerServices;
using System.Text;
using Google.Protobuf.WellKnownTypes;
using Nurtuo.Bridge;
using Nurtuo.Connector.V1;

namespace Nurtuo.Bridge.Examples;

/// <summary>
/// Echo/uppercase example (same behaviour as the Python example). Operations (EXECUTE):
/// <c>ECHO</c> (streamed copy) and <c>UPPERCASE</c> (UTF-8 aware, streamed).
/// A payload starting with <c>!reject</c> is an application error, <c>!crash</c> throws (technical
/// failure), <c>!leak</c> throws with the connection's <c>api_token</c> in the message (to show redaction).
/// </summary>
public sealed class EchoConnector : ConnectorBase
{
    public override IReadOnlySet<string>? SecretKeys { get; } = new HashSet<string> { "api_token" };

    public override Descriptor Describe() => MakeDescriptor(
        id: "example-echo-dotnet",
        version: "0.1.0",
        displayName: "Echo (.NET example)",
        operations:
        [
            Operation(OperationType.Execute, "ECHO"),
            Operation(OperationType.Execute, "UPPERCASE"),
        ],
        connectionSchema: Struct.Parser.ParseJson("""
            {"type":"object","properties":{"api_token":{"type":"string"},"prefix":{"type":"string"}}}
            """),
        trackedProperties: ["bytes"]);

    public override Task<TestResult> TestAsync(ConnectionSettings settings, CancellationToken ct) =>
        Task.FromResult(new TestResult(true, $"echo is always reachable (api_token {(settings.Contains("api_token") ? "set" : "not set")})"));

    public override async Task<Outcome?> ExecuteDocumentAsync(ExecuteContext ctx, InputDocument doc, CancellationToken ct)
    {
        var chunks = doc.ChunksAsync(ct).GetAsyncEnumerator(ct);
        var first = await chunks.MoveNextAsync() ? chunks.Current : ReadOnlyMemory<byte>.Empty;
        var head = Encoding.UTF8.GetString(first.Span[..Math.Min(first.Length, 16)]);
        if (head.StartsWith("!reject", StringComparison.Ordinal)) throw new ApplicationErrorException("document rejected by echo");
        if (head.StartsWith("!crash", StringComparison.Ordinal)) throw new InvalidOperationException("boom");
        if (head.StartsWith("!leak", StringComparison.Ordinal))
            throw new InvalidOperationException($"auth failed for token {ctx.Settings.Secret("api_token").Reveal()}");

        IAsyncEnumerable<ReadOnlyMemory<byte>> data = Rest(first, chunks);
        if (ctx.CustomType == "UPPERCASE") data = Upper(data);
        if (ctx.Settings.Get<string>("prefix") is { Length: > 0 } prefix) data = Prepend(Encoding.UTF8.GetBytes(prefix), data);

        var props = new Dictionary<string, string>(doc.Properties) { ["echo.operation"] = ctx.CustomType is { Length: > 0 } ? ctx.CustomType : "ECHO" };
        return new Outcome
        {
            Outputs = [new OutputDocument(data, props)],
            Tracked = () => new Dictionary<string, string> { ["bytes"] = doc.BytesRead.ToString() },
        };
    }

    private static async IAsyncEnumerable<ReadOnlyMemory<byte>> Rest(ReadOnlyMemory<byte> first, IAsyncEnumerator<ReadOnlyMemory<byte>> rest)
    {
        if (!first.IsEmpty) yield return first;
        while (await rest.MoveNextAsync()) yield return rest.Current;
    }

    private static async IAsyncEnumerable<ReadOnlyMemory<byte>> Prepend(byte[] head, IAsyncEnumerable<ReadOnlyMemory<byte>> rest)
    {
        yield return head;
        await foreach (var c in rest) yield return c;
    }

    private static async IAsyncEnumerable<ReadOnlyMemory<byte>> Upper(
        IAsyncEnumerable<ReadOnlyMemory<byte>> input, [EnumeratorCancellation] CancellationToken ct = default)
    {
        var decoder = Encoding.UTF8.GetDecoder(); // keeps multi-byte characters that straddle chunk borders intact
        await foreach (var c in input.WithCancellation(ct))
        {
            var chars = new char[Encoding.UTF8.GetMaxCharCount(c.Length)];
            var n = decoder.GetChars(c.Span, chars, flush: false);
            if (n > 0) yield return Encoding.UTF8.GetBytes(new string(chars, 0, n).ToUpperInvariant());
        }
    }
}
