// SPDX-License-Identifier: Apache-2.0
using System.Runtime.CompilerServices;
using Google.Protobuf;
using Grpc.Core;
using Nurtuo.Connector.V1;

namespace Nurtuo.Bridge;

/// <summary>The client violated the Execute stream protocol (maps to gRPC INVALID_ARGUMENT).</summary>
public sealed class ProtocolException(string message) : Exception(message);

/// <summary>The request stream after ExecuteStart; shared by consecutive documents.</summary>
internal sealed class RequestSource(IAsyncStreamReader<ExecuteRequest> reader, CancellationToken ct)
{
    public async Task<ExecuteRequest?> NextAsync() => await reader.MoveNext(ct) ? reader.Current : null;
}

/// <summary>
/// One input document. Single pass: the payload can be consumed once, in order, either chunk by
/// chunk (<see cref="ChunksAsync"/>), as a <see cref="Stream"/> (<see cref="OpenRead"/>) or whole
/// (<see cref="ReadAllAsync"/>). Whatever is left unread is discarded when the next document starts.
/// </summary>
public sealed class InputDocument
{
    private readonly RequestSource _src;
    private DocumentChunk? _first;
    private bool _done;

    internal InputDocument(RequestSource src, DocumentChunk first)
    {
        _src = src;
        _first = first;
        Id = first.DocumentId;
        Properties = first.Properties.ToDictionary(kv => kv.Key, kv => kv.Value);
    }

    public string Id { get; }

    /// <summary>Properties travel with the first chunk.</summary>
    public IReadOnlyDictionary<string, string> Properties { get; }

    public long BytesRead { get; private set; }

    private async Task<ByteString?> NextAsync()
    {
        if (_done) return null;
        DocumentChunk ch;
        if (_first is not null)
        {
            ch = _first;
            _first = null;
        }
        else
        {
            var msg = await _src.NextAsync() ?? throw new ProtocolException($"stream ended in the middle of document '{Id}'");
            if (msg.MsgCase != ExecuteRequest.MsgOneofCase.Chunk)
                throw new ProtocolException($"expected a chunk of document '{Id}', got {msg.MsgCase}");
            ch = msg.Chunk;
            if (ch.DocumentId != Id)
                throw new ProtocolException($"chunk of document '{ch.DocumentId}' interleaved with '{Id}'");
        }
        if (ch.Last) _done = true;
        BytesRead += ch.Data.Length;
        return ch.Data;
    }

    /// <summary>Yields the payload chunk by chunk (empty chunks are skipped).</summary>
    public async IAsyncEnumerable<ReadOnlyMemory<byte>> ChunksAsync([EnumeratorCancellation] CancellationToken ct = default)
    {
        while (await NextAsync() is { } data)
        {
            ct.ThrowIfCancellationRequested();
            if (data.Length > 0) yield return data.Memory;
        }
    }

    /// <summary>Reads the whole payload into memory; throws above <paramref name="limit"/> bytes.</summary>
    public async Task<byte[]> ReadAllAsync(long? limit = null, CancellationToken ct = default)
    {
        using var ms = new MemoryStream();
        await foreach (var c in ChunksAsync(ct))
        {
            if (limit is { } l && ms.Length + c.Length > l)
                throw new InvalidDataException($"document '{Id}' is larger than {l} bytes");
            ms.Write(c.Span);
        }
        return ms.ToArray();
    }

    /// <summary>The payload as a read-only stream (wrap in a StreamReader for text).</summary>
    public Stream OpenRead() => new ChunkStream(ChunksAsync());

    internal async Task DrainAsync()
    {
        while (await NextAsync() is not null) { }
    }

    private sealed class ChunkStream(IAsyncEnumerable<ReadOnlyMemory<byte>> chunks) : Stream
    {
        private readonly IAsyncEnumerator<ReadOnlyMemory<byte>> _e = chunks.GetAsyncEnumerator();
        private ReadOnlyMemory<byte> _rest;

        public override bool CanRead => true;
        public override bool CanSeek => false;
        public override bool CanWrite => false;
        public override long Length => throw new NotSupportedException();
        public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }
        public override void Flush() { }
        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();
        public override void SetLength(long value) => throw new NotSupportedException();
        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();

        public override int Read(byte[] buffer, int offset, int count) =>
            ReadAsync(buffer.AsMemory(offset, count)).AsTask().GetAwaiter().GetResult();

        public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken ct = default)
        {
            while (_rest.IsEmpty)
            {
                if (!await _e.MoveNextAsync()) return 0;
                _rest = _e.Current;
            }
            var n = Math.Min(buffer.Length, _rest.Length);
            _rest[..n].CopyTo(buffer);
            _rest = _rest[n..];
            return n;
        }

        protected override void Dispose(bool disposing)
        {
            if (disposing) _e.DisposeAsync().AsTask().GetAwaiter().GetResult();
            base.Dispose(disposing);
        }
    }

    /// <summary>Turns the request stream (after ExecuteStart) into lazily readable documents.</summary>
    internal static async IAsyncEnumerable<InputDocument> IterateAsync(
        IAsyncStreamReader<ExecuteRequest> requests, [EnumeratorCancellation] CancellationToken ct)
    {
        var src = new RequestSource(requests, ct);
        var seen = new HashSet<string>();
        while (await src.NextAsync() is { } msg)
        {
            if (msg.MsgCase == ExecuteRequest.MsgOneofCase.End) yield break;
            if (msg.MsgCase != ExecuteRequest.MsgOneofCase.Chunk)
                throw new ProtocolException($"unexpected {msg.MsgCase} message after ExecuteStart");
            if (!seen.Add(msg.Chunk.DocumentId))
                throw new ProtocolException($"document id '{msg.Chunk.DocumentId}' used twice");
            var doc = new InputDocument(src, msg.Chunk);
            yield return doc;
            await doc.DrainAsync();
        }
    }
}

/// <summary>An output document; the payload is <c>byte[]</c> or a stream of chunks.</summary>
public sealed class OutputDocument
{
    public OutputDocument(byte[] data, IReadOnlyDictionary<string, string>? properties = null, string id = "")
        : this(One(data), properties, id) { }

    public OutputDocument(IAsyncEnumerable<ReadOnlyMemory<byte>> data, IReadOnlyDictionary<string, string>? properties = null, string id = "")
    {
        Data = data;
        Properties = properties ?? new Dictionary<string, string>();
        Id = id;
    }

    /// <summary>Assigned as <c>&lt;input id&gt;.out&lt;N&gt;</c> when empty.</summary>
    public string Id { get; init; }
    public IReadOnlyDictionary<string, string> Properties { get; }
    public IAsyncEnumerable<ReadOnlyMemory<byte>> Data { get; }

    private static async IAsyncEnumerable<ReadOnlyMemory<byte>> One(byte[] data)
    {
        yield return data;
        await Task.CompletedTask;
    }
}

internal static class Chunker
{
    public const int DefaultChunkSize = 64 * 1024;

    /// <summary>
    /// Cuts the payload into chunks of at most <paramref name="chunkSize"/> bytes; the last one has
    /// <c>Last = true</c>; there is always at least one chunk; properties go with the first. One chunk of lookahead.
    /// </summary>
    public static async IAsyncEnumerable<DocumentChunk> ChunkAsync(
        string id, IReadOnlyDictionary<string, string> properties, IAsyncEnumerable<ReadOnlyMemory<byte>> data,
        int chunkSize, [EnumeratorCancellation] CancellationToken ct = default)
    {
        if (chunkSize <= 0) throw new ArgumentOutOfRangeException(nameof(chunkSize));
        DocumentChunk? prev = null;
        var first = true;
        await foreach (var piece in data.WithCancellation(ct))
        {
            for (var off = 0; off < piece.Length; off += chunkSize)
            {
                var part = piece.Slice(off, Math.Min(chunkSize, piece.Length - off));
                if (prev is not null) yield return prev;
                prev = new DocumentChunk { DocumentId = id, Data = ByteString.CopyFrom(part.Span) };
                if (first)
                {
                    foreach (var (k, v) in properties) prev.Properties[k] = v;
                    first = false;
                }
            }
        }
        if (prev is null)
        {
            prev = new DocumentChunk { DocumentId = id };
            foreach (var (k, v) in properties) prev.Properties[k] = v;
        }
        prev.Last = true;
        yield return prev;
    }
}
