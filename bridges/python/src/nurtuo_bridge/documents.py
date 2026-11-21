# SPDX-License-Identifier: Apache-2.0
"""Document streaming primitives for ``Execute``.

Input documents arrive as ``DocumentChunk`` messages on the request stream. They are exposed
lazily: a connector can iterate chunks (``InputDocument.iter_chunks``), wrap them as a file
object (``open``) or read everything (``read``). Memory stays bounded as long as the connector
streams. Output documents are produced from ``bytes`` or an iterable of ``bytes`` and are cut
into chunks of at most ``chunk_size`` bytes.
"""

from __future__ import annotations

import io
from collections.abc import Iterable, Iterator, Mapping
from dataclasses import dataclass, field
from typing import Any

from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb

#: Default size of output chunks. gRPC's default message limit is 4 MiB; stay far below it.
DEFAULT_CHUNK_SIZE = 64 * 1024


class ProtocolError(Exception):
    """The client violated the Execute stream protocol (maps to gRPC INVALID_ARGUMENT)."""


class DocumentTooLargeError(Exception):
    """``InputDocument.read`` exceeded its limit; stream the document instead."""


class _Source:
    """The request iterator shared by consecutive documents."""

    def __init__(self, requests: Iterable[pb.ExecuteRequest]) -> None:
        self._it = iter(requests)

    def next(self) -> pb.ExecuteRequest | None:
        try:
            return next(self._it)
        except StopIteration:
            return None


class InputDocument:
    """One input document. Single pass: chunks can be consumed once, in order."""

    def __init__(self, source: _Source, first: pb.DocumentChunk) -> None:
        self.id: str = first.document_id
        #: Properties are sent with the first chunk.
        self.properties: dict[str, str] = dict(first.properties)
        self._src = source
        self._first: pb.DocumentChunk | None = first
        self._done = False
        self.bytes_read = 0

    def _next(self) -> bytes | None:
        if self._done:
            return None
        if self._first is not None:
            ch, self._first = self._first, None
        else:
            msg = self._src.next()
            if msg is None:
                raise ProtocolError(f"stream ended in the middle of document {self.id!r}")
            if msg.WhichOneof("msg") != "chunk":
                raise ProtocolError(f"expected a chunk of document {self.id!r}, got {msg.WhichOneof('msg')}")
            ch = msg.chunk
            if ch.document_id != self.id:
                raise ProtocolError(f"chunk of document {ch.document_id!r} interleaved with {self.id!r}")
        if ch.last:
            self._done = True
        self.bytes_read += len(ch.data)
        return ch.data

    def iter_chunks(self) -> Iterator[bytes]:
        """Yield the payload chunk by chunk (empty chunks are skipped)."""
        while (data := self._next()) is not None:
            if data:
                yield data

    def read(self, limit: int | None = None) -> bytes:
        """Read the whole payload into memory; raises DocumentTooLargeError above ``limit`` bytes."""
        parts: list[bytes] = []
        total = 0
        for data in self.iter_chunks():
            total += len(data)
            if limit is not None and total > limit:
                raise DocumentTooLargeError(f"document {self.id!r} is larger than {limit} bytes")
            parts.append(data)
        return b"".join(parts)

    def open(self) -> io.BufferedReader:
        """The payload as a binary file object (wrap in ``io.TextIOWrapper`` for text)."""
        return io.BufferedReader(_ChunkReader(self.iter_chunks()))

    def drain(self) -> None:
        """Discard whatever the connector did not read, so the next document can start."""
        while self._next() is not None:
            pass


class _ChunkReader(io.RawIOBase):
    def __init__(self, chunks: Iterator[bytes]) -> None:
        self._chunks = chunks
        self._rest = b""

    def readable(self) -> bool:
        return True

    def readinto(self, b: Any) -> int:
        while not self._rest:
            try:
                self._rest = next(self._chunks)
            except StopIteration:
                return 0
        n = min(len(b), len(self._rest))
        b[:n] = self._rest[:n]
        self._rest = self._rest[n:]
        return n


def iter_input_documents(requests: Iterable[pb.ExecuteRequest]) -> Iterator[InputDocument]:
    """Turn the request stream (after ``ExecuteStart``) into lazily readable documents.

    Stops at ``ExecuteEnd`` (or when the client half-closes). Raises :class:`ProtocolError`
    for duplicate ``ExecuteStart``, reused document IDs or interleaved documents.
    """
    src = _Source(requests)
    seen: set[str] = set()
    while True:
        msg = src.next()
        if msg is None:
            return
        kind = msg.WhichOneof("msg")
        if kind == "end":
            return
        if kind != "chunk":
            raise ProtocolError(f"unexpected {kind} message after ExecuteStart")
        if msg.chunk.document_id in seen:
            raise ProtocolError(f"document id {msg.chunk.document_id!r} used twice")
        seen.add(msg.chunk.document_id)
        doc = InputDocument(src, msg.chunk)
        yield doc
        doc.drain()


@dataclass
class OutputDocument:
    """An output document. ``data`` may be ``bytes`` or an iterable of ``bytes`` (streamed)."""

    data: bytes | Iterable[bytes] = b""
    properties: Mapping[str, str] = field(default_factory=dict)
    #: Assigned by the server as ``<input id>.out<N>`` when empty.
    id: str = ""


def chunk_output(
    doc_id: str,
    properties: Mapping[str, str],
    data: bytes | Iterable[bytes],
    chunk_size: int = DEFAULT_CHUNK_SIZE,
) -> Iterator[pb.DocumentChunk]:
    """Cut ``data`` into chunks <= ``chunk_size``; the last chunk has ``last=True``.

    Always yields at least one chunk (an empty payload is one empty ``last`` chunk). Properties
    travel with the first chunk only. Holds one chunk of lookahead, so memory stays bounded.
    """
    if chunk_size <= 0:
        raise ValueError("chunk_size must be positive")
    pieces: Iterable[bytes] = [bytes(data)] if isinstance(data, (bytes, bytearray, memoryview)) else data

    def split() -> Iterator[bytes]:
        for p in pieces:
            mv = memoryview(p)
            for off in range(0, len(mv), chunk_size):
                yield bytes(mv[off : off + chunk_size])

    prev: pb.DocumentChunk | None = None
    first = True
    for piece in split():
        if prev is not None:
            yield prev
        prev = pb.DocumentChunk(document_id=doc_id, data=piece)
        if first:
            prev.properties.update(properties)
            first = False
    if prev is None:
        prev = pb.DocumentChunk(document_id=doc_id)
        prev.properties.update(properties)
    prev.last = True
    yield prev
