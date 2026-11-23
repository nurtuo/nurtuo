# SPDX-License-Identifier: Apache-2.0
"""gRPC server runner and the bridge process protocol (handshake, shutdown, health).

Process protocol (see bridges/README.md for the normative description):

* The runtime starts the bridge with these environment variables:
  ``NURTUO_BRIDGE_LISTEN`` (``unix:/path/to.sock`` or ``tcp:127.0.0.1:0``),
  ``NURTUO_BRIDGE_CONTRACT`` (comma separated acceptable contract packages, default
  ``nurtuo.connector.v1``), ``NURTUO_BRIDGE_TOKEN`` (optional shared secret that every RPC must
  carry in the ``x-nurtuo-bridge-token`` metadata) and ``NURTUO_BRIDGE_WATCH_STDIN=1`` (shut
  down when stdin reaches EOF, i.e. when the runtime dies or closes the pipe).
* When the server listens, exactly one handshake line is written to stdout:
  ``NURTUO-BRIDGE|1|<network>|<address>|<contract>|<bridge name/version>``.
  Everything else (logs, stray ``print``) goes to stderr.
* SIGTERM / SIGINT / stdin EOF start a graceful shutdown (``grace`` seconds for in-flight RPCs).
"""

from __future__ import annotations

import argparse
import hmac
import importlib
import logging
import os
import signal
import sys
import threading
from collections.abc import Callable, Iterator, Sequence
from concurrent import futures
from dataclasses import dataclass
from typing import Any, TextIO

import grpc
from google.protobuf import json_format
from grpc_health.v1 import health, health_pb2, health_pb2_grpc

from nurtuo_bridge import __version__
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2_grpc as pb_grpc
from nurtuo_bridge.connector import (
    BrowseContext,
    BrowseNotSupportedError,
    Connector,
    ExecuteContext,
    Result,
)
from nurtuo_bridge.documents import DEFAULT_CHUNK_SIZE, OutputDocument, ProtocolError, chunk_output, iter_input_documents
from nurtuo_bridge.secrets import ConnectionSettings

log = logging.getLogger("nurtuo_bridge")

CONTRACT = "nurtuo.connector.v1"
SUPPORTED_CONTRACTS: tuple[str, ...] = (CONTRACT,)
HANDSHAKE_PREFIX = "NURTUO-BRIDGE"
HANDSHAKE_VERSION = 1
TOKEN_METADATA_KEY = "x-nurtuo-bridge-token"
SERVICE_NAME = "nurtuo.connector.v1.ConnectorService"

ENV_LISTEN = "NURTUO_BRIDGE_LISTEN"
ENV_CONTRACT = "NURTUO_BRIDGE_CONTRACT"
ENV_TOKEN = "NURTUO_BRIDGE_TOKEN"
ENV_WATCH_STDIN = "NURTUO_BRIDGE_WATCH_STDIN"

DEFAULT_MAX_MESSAGE_BYTES = 16 * 1024 * 1024
_LOOPBACK = {"127.0.0.1", "localhost", "::1", "[::1]"}


def _settings(connector: Connector, cfg: pb.ConnectionConfig) -> ConnectionSettings:
    raw = json_format.MessageToDict(cfg.settings) if cfg.HasField("settings") else {}
    return ConnectionSettings(raw, connector.secret_keys)


class ConnectorServicer(pb_grpc.ConnectorServiceServicer):
    """Adapts a :class:`Connector` to the generated gRPC service."""

    def __init__(self, connector: Connector, chunk_size: int = DEFAULT_CHUNK_SIZE) -> None:
        self._c = connector
        self._chunk_size = chunk_size

    def Describe(self, request: pb.DescribeRequest, context: grpc.ServicerContext) -> pb.DescribeResponse:
        try:
            d = pb.ConnectorDescriptor()
            d.CopyFrom(self._c.describe())
        except Exception as e:
            log.exception("describe failed")
            context.abort(grpc.StatusCode.INTERNAL, f"describe failed: {type(e).__name__}")
        if d.host == pb.HOST_UNSPECIFIED:
            d.host = pb.HOST_BRIDGE_PYTHON
        return pb.DescribeResponse(descriptor=d)

    def Test(self, request: pb.TestRequest, context: grpc.ServicerContext) -> pb.TestResponse:
        settings = _settings(self._c, request.connection)
        try:
            r = self._c.test(settings)
            return pb.TestResponse(ok=r.ok, diagnostic=settings.redactor(r.diagnostic))
        except Exception as e:  # noqa: BLE001 - a failed test is a result, not an RPC error
            log.error("test failed: %s", settings.redactor(repr(e)))
            return pb.TestResponse(ok=False, diagnostic=settings.redactor(f"{type(e).__name__}: {e}"))

    def Browse(self, request: pb.BrowseRequest, context: grpc.ServicerContext) -> pb.BrowseResponse:
        settings = _settings(self._c, request.connection)
        ctx = BrowseContext(settings, request.operation_type, request.custom_type, request.object_type_id)
        try:
            r = self._c.browse(ctx)
        except (BrowseNotSupportedError, NotImplementedError):
            context.abort(grpc.StatusCode.UNIMPLEMENTED, "browse is not supported by this connector")
        except Exception as e:  # noqa: BLE001 - boundary
            log.error("browse failed: %s", settings.redactor(repr(e)))
            context.abort(grpc.StatusCode.INTERNAL, settings.redactor(f"{type(e).__name__}: {e}"))
        resp = pb.BrowseResponse(object_types=[pb.ObjectType(id=i, label=lbl) for i, lbl in r.object_types])
        if r.request_profile:
            resp.request_profile.CopyFrom(pb.Profile(format=r.request_profile.format, definition=r.request_profile.definition))
        if r.response_profile:
            resp.response_profile.CopyFrom(pb.Profile(format=r.response_profile.format, definition=r.response_profile.definition))
        return resp

    def Execute(
        self, request_iterator: Iterator[pb.ExecuteRequest], context: grpc.ServicerContext
    ) -> Iterator[pb.ExecuteResponse]:
        requests = iter(request_iterator)
        first = next(requests, None)
        if first is None or first.WhichOneof("msg") != "start":
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "the first Execute message must be ExecuteStart")
        assert first is not None  # context.abort above raises
        start = first.start
        settings = _settings(self._c, start.connection)
        ctx = ExecuteContext(
            execution_id=start.execution_id,
            step_id=start.step_id,
            settings=settings,
            operation_type=start.operation_type,
            custom_type=start.custom_type,
            object_type_id=start.object_type_id,
            options=json_format.MessageToDict(start.options),
            chunk_size=self._chunk_size,
        )
        redact = settings.redactor
        out_seq = 0
        try:
            for item in self._c.execute(ctx, iter_input_documents(requests)):
                if isinstance(item, Result):
                    r = pb.DocumentResult(
                        document_id=item.document_id,
                        status=item.status,
                        message=redact(item.message),
                        output_document_ids=list(item.output_document_ids),
                    )
                    r.tracked.update({k: redact(str(v)) for k, v in item.tracked.items()})
                    yield pb.ExecuteResponse(result=r)
                elif isinstance(item, pb.DocumentChunk):
                    yield pb.ExecuteResponse(output=item)
                elif isinstance(item, OutputDocument):
                    out_seq += 1
                    doc_id = item.id or f"out{out_seq}"
                    for ch in chunk_output(doc_id, item.properties, item.data, self._chunk_size):
                        yield pb.ExecuteResponse(output=ch)
                else:
                    raise TypeError(f"execute yielded {type(item).__name__}, expected Result or OutputDocument")
        except ProtocolError as e:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, redact(str(e)))
        except Exception as e:
            if isinstance(e, grpc.RpcError):
                raise
            log.error("execute failed: %s", redact(repr(e)))
            context.abort(grpc.StatusCode.INTERNAL, redact(f"{type(e).__name__}: {e}"))


class _TokenInterceptor(grpc.ServerInterceptor):
    """Rejects every RPC that does not carry the shared bridge token (loopback TCP hardening)."""

    def __init__(self, token: str) -> None:
        self._token = token.encode()

        def deny(request: Any, context: grpc.ServicerContext) -> Any:
            context.abort(grpc.StatusCode.UNAUTHENTICATED, "missing or invalid bridge token")

        self._deny = grpc.unary_unary_rpc_method_handler(deny)

    def intercept_service(self, continuation: Callable[..., Any], handler_call_details: Any) -> Any:
        for k, v in handler_call_details.invocation_metadata or ():
            if k == TOKEN_METADATA_KEY:
                got = v.encode() if isinstance(v, str) else bytes(v)
                if hmac.compare_digest(got, self._token):
                    return continuation(handler_call_details)
        return self._deny


@dataclass
class ListenSpec:
    network: str  # "unix" | "tcp"
    address: str  # path | host:port

    @classmethod
    def parse(cls, spec: str) -> ListenSpec:
        network, _, rest = spec.partition(":")
        if network == "unix" and rest:
            if sys.platform == "win32":
                raise ValueError("unix sockets are not supported by grpcio on Windows; use tcp:127.0.0.1:0")
            return cls("unix", rest)
        if network == "tcp" and rest:
            host, _, port = rest.rpartition(":")
            if host not in _LOOPBACK or not port.isdigit():
                raise ValueError(f"tcp listen address must be loopback host:port, got {rest!r}")
            return cls("tcp", rest)
        raise ValueError(f"invalid listen spec {spec!r}; expected unix:/path or tcp:127.0.0.1:port")


class RunningServer:
    """A started gRPC server. ``address`` is what clients dial (``host:port`` or a socket path)."""

    def __init__(self, server: grpc.Server, health_servicer: health.HealthServicer, spec: ListenSpec, address: str) -> None:
        self._server = server
        self._health = health_servicer
        self.network = spec.network
        self.address = address
        self._socket_path = spec.address if spec.network == "unix" else None

    @property
    def target(self) -> str:
        """gRPC dial target."""
        return f"unix:{self.address}" if self.network == "unix" else self.address

    def wait(self, timeout: float | None = None) -> bool:
        return self._server.wait_for_termination(timeout)

    def stop(self, grace: float | None = 5.0) -> None:
        """Mark NOT_SERVING, let in-flight RPCs finish for ``grace`` seconds, then cancel them."""
        self._health.enter_graceful_shutdown()
        self._server.stop(grace).wait()
        if self._socket_path:
            try:
                os.unlink(self._socket_path)
            except OSError:
                pass


def serve(
    connector: Connector,
    listen: str = "tcp:127.0.0.1:0",
    *,
    token: str | None = None,
    max_workers: int = 16,
    chunk_size: int = DEFAULT_CHUNK_SIZE,
    max_message_bytes: int = DEFAULT_MAX_MESSAGE_BYTES,
) -> RunningServer:
    """Start serving ``connector`` (non-blocking). ``listen`` is ``unix:/path`` or ``tcp:127.0.0.1:port``."""
    spec = ListenSpec.parse(listen)
    interceptors = [_TokenInterceptor(token)] if token else []
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="nurtuo-bridge"),
        interceptors=interceptors,
        options=[
            ("grpc.max_receive_message_length", max_message_bytes),
            ("grpc.max_send_message_length", max_message_bytes),
        ],
    )
    pb_grpc.add_ConnectorServiceServicer_to_server(ConnectorServicer(connector, chunk_size), server)
    health_servicer = health.HealthServicer()
    health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)
    health_servicer.set(SERVICE_NAME, health_pb2.HealthCheckResponse.SERVING)
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)

    if spec.network == "unix":
        try:
            os.unlink(spec.address)  # stale socket from a crashed predecessor
        except FileNotFoundError:
            pass
        if server.add_insecure_port(f"unix:{spec.address}") == 0:
            raise OSError(f"cannot listen on unix:{spec.address}")
        address = spec.address
    else:
        host, _, port = spec.address.rpartition(":")
        bound = server.add_insecure_port(f"{host}:{port}")
        if bound == 0:
            raise OSError(f"cannot listen on tcp:{spec.address}")
        address = f"{host}:{bound}"
    server.start()
    return RunningServer(server, health_servicer, spec, address)


def format_handshake(network: str, address: str, contract: str = CONTRACT) -> str:
    return "|".join([HANDSHAKE_PREFIX, str(HANDSHAKE_VERSION), network, address, contract, f"python/{__version__}"])


def negotiate_contract(requested: str | None, supported: Sequence[str] = SUPPORTED_CONTRACTS) -> str | None:
    """Pick the first contract in the runtime's preference list that this bridge supports."""
    wanted = [c.strip() for c in (requested or CONTRACT).split(",") if c.strip()]
    for c in wanted:
        if c in supported:
            return c
    return None


def run(
    connector: Connector,
    *,
    env: dict[str, str] | None = None,
    stdout: TextIO | None = None,
    install_signal_handlers: bool = True,
    grace: float = 5.0,
) -> int:
    """Run ``connector`` as a bridge process: handshake, serve, shut down gracefully. Returns the exit code."""
    env = dict(os.environ) if env is None else env
    handshake_out = stdout or sys.stdout
    if stdout is None:
        sys.stdout = sys.stderr  # stdout belongs to the handshake; stray prints must not corrupt it
    logging.basicConfig(
        stream=sys.stderr, level=env.get("NURTUO_BRIDGE_LOG", "INFO").upper(), format="%(asctime)s %(levelname)s %(name)s: %(message)s"
    )

    contract = negotiate_contract(env.get(ENV_CONTRACT))
    if contract is None:
        log.error("none of the requested contracts %r is supported (supported: %s)", env.get(ENV_CONTRACT), ", ".join(SUPPORTED_CONTRACTS))
        return 3
    try:
        server = serve(connector, env.get(ENV_LISTEN) or "tcp:127.0.0.1:0", token=env.get(ENV_TOKEN) or None)
    except (ValueError, OSError) as e:
        log.error("cannot start: %s", e)
        return 2

    stop = threading.Event()
    if install_signal_handlers:
        for name in ("SIGTERM", "SIGINT", "SIGBREAK"):
            sig = getattr(signal, name, None)
            if sig is not None:
                signal.signal(sig, lambda *_: stop.set())
    if env.get(ENV_WATCH_STDIN) == "1":
        threading.Thread(target=_watch_stdin, args=(stop,), name="stdin-watch", daemon=True).start()

    print(format_handshake(server.network, server.address, contract), file=handshake_out, flush=True)
    log.info("bridge listening on %s %s (contract %s)", server.network, server.address, contract)
    while not stop.wait(0.2):
        pass
    log.info("shutting down")
    server.stop(grace)
    return 0


def _watch_stdin(stop: threading.Event) -> None:
    try:
        while sys.stdin.buffer.read(4096):
            pass
    except (OSError, ValueError):
        pass
    stop.set()


def load_connector(spec: str) -> Connector:
    """Instantiate ``package.module:Factory`` (a Connector subclass or a zero-argument callable)."""
    module_name, _, attr = spec.partition(":")
    if not module_name or not attr:
        raise ValueError(f"connector spec must be 'package.module:Name', got {spec!r}")
    obj = getattr(importlib.import_module(module_name), attr)
    c = obj()
    if not isinstance(c, Connector):
        raise TypeError(f"{spec} did not produce a Connector")
    return c


def main(argv: Sequence[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="python -m nurtuo_bridge", description="Run a connector as a Nurtuo bridge process.")
    ap.add_argument("connector", help="package.module:ClassName of the connector")
    ap.add_argument("--grace", type=float, default=5.0, help="seconds to let in-flight RPCs finish on shutdown")
    args = ap.parse_args(argv)
    return run(load_connector(args.connector), grace=args.grace)
