# SPDX-License-Identifier: Apache-2.0
from __future__ import annotations

import pytest

from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb
from nurtuo_bridge.documents import (
    DocumentTooLargeError,
    ProtocolError,
    chunk_output,
    iter_input_documents,
)


def chunk(doc: str, data: bytes, last: bool, **props: str) -> pb.ExecuteRequest:
    c = pb.DocumentChunk(document_id=doc, data=data, last=last)
    c.properties.update(props)
    return pb.ExecuteRequest(chunk=c)


END = pb.ExecuteRequest(end=pb.ExecuteEnd())


def test_chunk_output_splits_and_marks_last() -> None:
    chunks = list(chunk_output("d", {"k": "v"}, b"x" * 25, 10))
    assert [len(c.data) for c in chunks] == [10, 10, 5]
    assert [c.last for c in chunks] == [False, False, True]
    assert dict(chunks[0].properties) == {"k": "v"}
    assert all(not c.properties for c in chunks[1:])


def test_chunk_output_empty_payload_is_one_last_chunk() -> None:
    chunks = list(chunk_output("d", {}, b"", 10))
    assert len(chunks) == 1 and chunks[0].last and chunks[0].data == b""
    assert len(list(chunk_output("d", {}, iter([b"", b""]), 10))) == 1


def test_chunk_output_streams_iterables_with_bounded_lookahead() -> None:
    produced: list[int] = []

    def gen():  # type: ignore[no-untyped-def]
        for i in range(5):
            produced.append(i)
            yield b"abc"

    it = chunk_output("d", {}, gen(), 3)
    next(it)
    assert produced == [0, 1]  # one chunk of lookahead, not the whole payload
    rest = list(it)
    assert rest[-1].last and len(rest) == 4


def test_input_documents_are_lazy_and_sequential() -> None:
    reqs = iter([chunk("a", b"he", False, k="v"), chunk("a", b"llo", True), chunk("b", b"", True), END])
    docs = iter_input_documents(reqs)
    a = next(docs)
    assert a.id == "a" and a.properties == {"k": "v"}
    assert a.read() == b"hello"
    b = next(docs)
    assert b.id == "b" and b.read() == b""
    assert list(docs) == []


def test_unread_remainder_is_drained() -> None:
    reqs = iter([chunk("a", b"1", False), chunk("a", b"2", False), chunk("a", b"3", True), chunk("b", b"x", True), END])
    docs = iter_input_documents(reqs)
    a = next(docs)
    assert next(a.iter_chunks()) == b"1"
    b = next(docs)  # drains a
    assert b.id == "b" and b.read() == b"x"


def test_open_returns_a_file_object() -> None:
    reqs = iter([chunk("a", b"line1\nli", False), chunk("a", b"ne2\n", True), END])
    a = next(iter_input_documents(reqs))
    assert a.open().read().splitlines() == [b"line1", b"line2"]


def test_read_limit() -> None:
    reqs = iter([chunk("a", b"x" * 10, True), END])
    with pytest.raises(DocumentTooLargeError):
        next(iter_input_documents(reqs)).read(limit=5)


@pytest.mark.parametrize(
    "msgs",
    [
        [chunk("a", b"1", False), chunk("b", b"2", True)],  # interleaved
        [chunk("a", b"1", True), chunk("a", b"2", True)],  # id reused
        [chunk("a", b"1", False)],  # stream ends mid-document
        [pb.ExecuteRequest(start=pb.ExecuteStart())],  # second start
    ],
)
def test_protocol_violations(msgs: list[pb.ExecuteRequest]) -> None:
    with pytest.raises(ProtocolError):
        for d in iter_input_documents(iter(msgs)):
            d.read()
