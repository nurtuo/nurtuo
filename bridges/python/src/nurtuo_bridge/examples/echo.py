# SPDX-License-Identifier: Apache-2.0
"""Echo/uppercase example connector.

Run it:  ``python -m nurtuo_bridge nurtuo_bridge.examples.echo:EchoConnector``

Operations (all ``EXECUTE``):

* ``ECHO``      - output = input, streamed chunk by chunk.
* ``UPPERCASE`` - output = input upper-cased (UTF-8 aware, streamed).

Test documents: a payload starting with ``!reject`` is an application error, ``!crash`` raises
an unexpected exception (technical failure), ``!leak`` raises an exception that contains the
connection's ``api_token`` (to demonstrate redaction).
"""

from __future__ import annotations

import codecs
from collections.abc import Iterator

from nurtuo_bridge import (
    ApplicationError,
    ConnectionSettings,
    Connector,
    ExecuteContext,
    InputDocument,
    Outcome,
    OutputDocument,
    TestResult,
    make_descriptor,
    operation,
)
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb


class EchoConnector(Connector):
    secret_keys = frozenset({"api_token"})

    def describe(self) -> pb.Descriptor:
        return make_descriptor(
            id="example-echo",
            version="0.1.0",
            display_name="Echo (Python example)",
            connection_schema={
                "type": "object",
                "properties": {
                    "api_token": {"type": "string", "description": "Secret reference, injected at runtime"},
                    "prefix": {"type": "string"},
                },
            },
            operations=[
                operation(pb.OPERATION_TYPE_EXECUTE, custom_type="ECHO"),
                operation(pb.OPERATION_TYPE_EXECUTE, custom_type="UPPERCASE"),
            ],
            tracked_properties=["bytes"],
        )

    def test(self, settings: ConnectionSettings) -> TestResult:
        has_token = "api_token" in settings
        return TestResult(ok=True, diagnostic=f"echo is always reachable (api_token {'set' if has_token else 'not set'})")

    def execute_document(self, ctx: ExecuteContext, doc: InputDocument) -> Outcome:
        chunks = doc.iter_chunks()
        first = next(chunks, b"")
        if first.startswith(b"!reject"):
            raise ApplicationError("document rejected by echo")
        if first.startswith(b"!crash"):
            raise RuntimeError("boom")
        if first.startswith(b"!leak"):
            raise RuntimeError(f"auth failed for token {ctx.settings.secret('api_token').reveal()}")

        def stream() -> Iterator[bytes]:
            yield first
            yield from chunks

        data = stream() if ctx.custom_type != "UPPERCASE" else _upper(stream())
        prefix = ctx.settings.get("prefix", "")
        props = dict(doc.properties)
        props["echo.operation"] = ctx.custom_type or "ECHO"
        out = OutputDocument(data=_prepend(prefix.encode(), data) if prefix else data, properties=props)
        # ``bytes`` is only known once the output stream has been consumed: pass a callable.
        return Outcome(outputs=[out], tracked=lambda: {"bytes": str(doc.bytes_read)})


def _prepend(head: bytes, rest: Iterator[bytes]) -> Iterator[bytes]:
    yield head
    yield from rest


def _upper(chunks: Iterator[bytes]) -> Iterator[bytes]:
    dec = codecs.getincrementaldecoder("utf-8")(errors="replace")
    for c in chunks:
        text = dec.decode(c)
        if text:
            yield text.upper().encode()
    tail = dec.decode(b"", final=True)
    if tail:
        yield tail.upper().encode()
