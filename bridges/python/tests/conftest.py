# SPDX-License-Identifier: Apache-2.0
from __future__ import annotations

from collections.abc import Callable, Iterator

import grpc
import pytest

from nurtuo_bridge import Connector, RunningServer, serve
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2_grpc as pb_grpc


class Env:
    def __init__(self, server: RunningServer, channel: grpc.Channel) -> None:
        self.server = server
        self.channel = channel
        self.stub = pb_grpc.ConnectorServiceStub(channel)


@pytest.fixture
def start() -> Iterator[Callable[..., Env]]:
    """Start a connector on an in-process gRPC server (loopback TCP) and connect a client."""
    started: list[tuple[RunningServer, grpc.Channel]] = []

    def _start(connector: Connector, *, token: str | None = None, chunk_size: int = 64 * 1024, metadata_token: str | None = None) -> Env:
        server = serve(connector, "tcp:127.0.0.1:0", token=token, chunk_size=chunk_size)
        channel = grpc.insecure_channel(server.address)
        if metadata_token is not None:
            channel = grpc.intercept_channel(channel, _TokenClientInterceptor(metadata_token))
        started.append((server, channel))
        grpc.channel_ready_future(channel).result(timeout=10) if metadata_token is None else None
        return Env(server, channel)

    yield _start
    for server, channel in started:
        channel.close()
        server.stop(0)


class _TokenClientInterceptor(
    grpc.UnaryUnaryClientInterceptor,
    grpc.UnaryStreamClientInterceptor,
    grpc.StreamUnaryClientInterceptor,
    grpc.StreamStreamClientInterceptor,
):
    def __init__(self, token: str) -> None:
        self._md = ("x-nurtuo-bridge-token", token)

    def _with(self, details: grpc.ClientCallDetails) -> grpc.ClientCallDetails:
        class D(grpc.ClientCallDetails):
            def __init__(s, base: grpc.ClientCallDetails, md: list[tuple[str, str]]) -> None:
                s.method, s.timeout, s.metadata = base.method, base.timeout, md
                s.credentials, s.wait_for_ready, s.compression = base.credentials, base.wait_for_ready, base.compression

        return D(details, [*(details.metadata or []), self._md])

    def intercept_unary_unary(self, continuation, details, request):  # type: ignore[no-untyped-def]
        return continuation(self._with(details), request)

    def intercept_unary_stream(self, continuation, details, request):  # type: ignore[no-untyped-def]
        return continuation(self._with(details), request)

    def intercept_stream_unary(self, continuation, details, request_iterator):  # type: ignore[no-untyped-def]
        return continuation(self._with(details), request_iterator)

    def intercept_stream_stream(self, continuation, details, request_iterator):  # type: ignore[no-untyped-def]
        return continuation(self._with(details), request_iterator)


def execute_requests(
    start: pb.ExecuteStart, docs: list[tuple[str, bytes]], chunk: int = 64 * 1024, props: dict[str, str] | None = None
) -> Iterator[pb.ExecuteRequest]:
    yield pb.ExecuteRequest(start=start)
    for doc_id, data in docs:
        pieces = [data[i : i + chunk] for i in range(0, len(data), chunk)] or [b""]
        for n, piece in enumerate(pieces):
            c = pb.DocumentChunk(document_id=doc_id, data=piece, last=n == len(pieces) - 1)
            if n == 0 and props:
                c.properties.update(props)
            yield pb.ExecuteRequest(chunk=c)
    yield pb.ExecuteRequest(end=pb.ExecuteEnd())


class ExecResult:
    def __init__(self) -> None:
        self.results: dict[str, pb.DocumentResult] = {}
        self.outputs: dict[str, bytearray] = {}
        self.output_chunks: dict[str, list[pb.DocumentChunk]] = {}


def run_execute(env: Env, start: pb.ExecuteStart, docs: list[tuple[str, bytes]], **kw: object) -> ExecResult:
    out = ExecResult()
    for resp in env.stub.Execute(execute_requests(start, docs, **kw)):  # type: ignore[arg-type]
        kind = resp.WhichOneof("msg")
        if kind == "result":
            assert resp.result.document_id not in out.results, "duplicate result"
            out.results[resp.result.document_id] = resp.result
        elif kind == "output":
            out.outputs.setdefault(resp.output.document_id, bytearray()).extend(resp.output.data)
            out.output_chunks.setdefault(resp.output.document_id, []).append(resp.output)
    return out
