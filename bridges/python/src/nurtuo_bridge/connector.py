# SPDX-License-Identifier: Apache-2.0
"""The connector base class: describe / test / browse / execute.

Implement :class:`Connector`, then run it with :func:`nurtuo_bridge.server.run` (or
``python -m nurtuo_bridge package.module:ClassName``). The simplest connector overrides
``describe``, ``test`` and ``execute_document``; the default ``execute`` calls
``execute_document`` once per input document and turns its outcome (or exception) into the
per-document ``DocumentResult`` the contract requires.
"""

from __future__ import annotations

import abc
import dataclasses
import logging
import sys
from collections.abc import Callable, Iterable, Iterator, Mapping
from dataclasses import dataclass, field
from typing import Any, ClassVar

from google.protobuf import struct_pb2

from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb
from nurtuo_bridge.documents import InputDocument, OutputDocument, ProtocolError, chunk_output
from nurtuo_bridge.secrets import ConnectionSettings, Redactor

log = logging.getLogger("nurtuo_bridge")

#: Aliases so connector code does not need to import the generated module.
Status = pb.DocumentStatus
OperationType = pb.OperationType
ConnectorDescriptor = pb.ConnectorDescriptor
Host = pb.Host

SUCCESS = pb.DOCUMENT_STATUS_SUCCESS
APPLICATION_ERROR = pb.DOCUMENT_STATUS_APPLICATION_ERROR
FAILURE = pb.DOCUMENT_STATUS_FAILURE


class ConnectorError(Exception):
    """Technical failure of one document (becomes DOCUMENT_STATUS_FAILURE)."""


class ApplicationError(ConnectorError):
    """The target system rejected the document (becomes DOCUMENT_STATUS_APPLICATION_ERROR).

    The message is returned to the flow, so it must not contain secrets (known secret values
    are redacted regardless).
    """


class BrowseNotSupportedError(Exception):
    """Raised (or inherited) by connectors without browse support; maps to UNIMPLEMENTED."""


@dataclass
class TestResult:
    __test__ = False  # not a pytest test class

    ok: bool
    #: Human readable and redacted: never put secrets in here.
    diagnostic: str = ""


@dataclass
class BrowseContext:
    settings: ConnectionSettings
    operation_type: int
    custom_type: str
    object_type_id: str  # empty: list object types


@dataclass
class Profile:
    """A request/response profile; ``definition`` is a JSON Schema or XSD document."""

    definition: bytes
    format: int = pb.Profile.FORMAT_JSON_SCHEMA


@dataclass
class BrowseResult:
    #: (id, label) pairs
    object_types: list[tuple[str, str]] = field(default_factory=list)
    request_profile: Profile | None = None
    response_profile: Profile | None = None


@dataclass
class ExecuteContext:
    execution_id: str
    step_id: str
    settings: ConnectionSettings
    operation_type: int
    custom_type: str
    object_type_id: str
    options: dict[str, Any]
    chunk_size: int

    @property
    def redact(self) -> Redactor:
        return self.settings.redactor


@dataclass
class Outcome:
    """Result of one document."""

    status: int = SUCCESS
    message: str = ""
    #: Values for ``ConnectorDescriptor.tracked_properties`` (Process Reporting). May be a callable, which
    #: is evaluated after the output documents were streamed (for values only known by then).
    tracked: Mapping[str, str] | Callable[[], Mapping[str, str]] = field(default_factory=dict)
    outputs: Iterable[OutputDocument | bytes] = ()


@dataclass
class Result:
    """Emitted by ``execute``: exactly one per input document."""

    document_id: str
    status: int = SUCCESS
    message: str = ""
    tracked: Mapping[str, str] = field(default_factory=dict)
    output_document_ids: Iterable[str] = ()


#: Items yielded by ``Connector.execute``: results, output chunks, or whole output documents
#: (which the server chunks; an error while chunking one fails the whole batch, so connectors
#: that stream lazily should chunk themselves with ``documents.chunk_output``).
Emit = Result | OutputDocument | pb.DocumentChunk


def to_struct(value: Mapping[str, Any] | None) -> struct_pb2.Struct | None:
    if value is None:
        return None
    s = struct_pb2.Struct()
    s.update(dict(value))
    return s


def operation(
    type_: int,
    *,
    custom_type: str = "",
    supports_browse: bool = False,
    options_schema: Mapping[str, Any] | None = None,
) -> pb.OperationDescriptor:
    """Build an ``OperationDescriptor``."""
    op = pb.OperationDescriptor(type=type_, custom_type=custom_type, supports_browse=supports_browse)
    if options_schema is not None:
        op.options_schema.update(dict(options_schema))
    return op


def make_descriptor(
    *,
    id: str,
    version: str,
    display_name: str,
    operations: Iterable[pb.OperationDescriptor],
    connection_schema: Mapping[str, Any] | None = None,
    tracked_properties: Iterable[str] = (),
    streaming: bool = True,
    max_batch_size: int = 0,
    transactional: bool = False,
) -> pb.ConnectorDescriptor:
    """Build a ``ConnectorDescriptor`` with ``host=HOST_BRIDGE_PYTHON``."""
    d = pb.ConnectorDescriptor(
        id=id,
        version=version,
        host=pb.HOST_BRIDGE_PYTHON,
        display_name=display_name,
        operations=list(operations),
        tracked_properties=list(tracked_properties),
        capabilities=pb.Capabilities(streaming=streaming, max_batch_size=max_batch_size, transactional=transactional),
    )
    if connection_schema is not None:
        d.connection_schema.update(dict(connection_schema))
    return d


class Connector(abc.ABC):
    """Base class of every Python connector."""

    #: Connection setting keys that hold secrets. ``None``: guess from the key name.
    secret_keys: ClassVar[frozenset[str] | None] = None

    @abc.abstractmethod
    def describe(self) -> pb.ConnectorDescriptor:
        """Operations, connection schema and capabilities. Must be cheap and side-effect free."""

    @abc.abstractmethod
    def test(self, settings: ConnectionSettings) -> TestResult:
        """Check that the target system is reachable with ``settings``. Redact the diagnostic."""

    def browse(self, ctx: BrowseContext) -> BrowseResult:
        """List object types / return profiles. Optional."""
        raise BrowseNotSupportedError

    def execute_document(self, ctx: ExecuteContext, doc: InputDocument) -> Outcome | None:
        """Process one document (simple connectors override this instead of ``execute``).

        Return an :class:`Outcome` (``None`` means success without output). Raise
        :class:`ApplicationError` for business rejections; any other exception is a technical
        failure. The payload is a stream: read ``doc`` as needed; what is left unread is
        discarded afterwards. Output documents may be generators that read ``doc`` lazily.
        """
        raise NotImplementedError("override execute_document or execute")

    def execute(self, ctx: ExecuteContext, documents: Iterator[InputDocument]) -> Iterator[Emit]:
        """Process a batch. Default: ``execute_document`` per document, errors isolated.

        Contract: exactly one :class:`Result` per input document, in any order, plus any number
        of :class:`OutputDocument` items. A technical failure of the whole batch is signalled
        by raising (the server turns it into a non-OK gRPC status).
        """
        for doc in documents:
            yield from self._execute_one(ctx, doc)
            doc.drain()

    def _execute_one(self, ctx: ExecuteContext, doc: InputDocument) -> Iterator[Emit]:
        redact = ctx.redact
        try:
            outcome = self.execute_document(ctx, doc) or Outcome()
            ids: list[str] = []
            for n, out in enumerate(outcome.outputs, start=1):
                o = out if isinstance(out, OutputDocument) else OutputDocument(data=out)
                if not o.id:
                    o = dataclasses.replace(o, id=f"{doc.id}.out{n}")
                ids.append(o.id)
                # Chunked here (inside the try) so that an error while a lazily produced output
                # is streamed fails this document only. Output goes out before the result so
                # generators can still read ``doc``.
                yield from chunk_output(o.id, o.properties, o.data, ctx.chunk_size)
            tracked = outcome.tracked() if callable(outcome.tracked) else outcome.tracked
            yield Result(doc.id, outcome.status, redact(outcome.message), dict(tracked), ids)
        except ProtocolError:
            raise  # the stream itself is broken: fail the batch, not just this document
        except ApplicationError as e:
            yield Result(doc.id, APPLICATION_ERROR, redact(str(e)))
        except Exception as e:
            # Details go to stderr (redacted); the flow only sees the exception type + message.
            log.error("document %s failed: %s", doc.id, redact(repr(e)), exc_info=_redacted_exc_info(redact))
            yield Result(doc.id, FAILURE, redact(f"{type(e).__name__}: {e}"))


def _redacted_exc_info(redact: Redactor) -> bool:
    # Tracebacks can embed secrets in frame reprs; only print them when nothing is secret.
    return not redact and sys.exc_info()[0] is not None
