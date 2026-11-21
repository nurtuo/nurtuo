# SPDX-License-Identifier: Apache-2.0
from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path

import grpc
import pytest
from google.protobuf import struct_pb2
from grpc_health.v1 import health_pb2, health_pb2_grpc

from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb
from nurtuo_bridge.examples.csv_reader import CsvReaderConnector
from nurtuo_bridge.examples.echo import EchoConnector

from .conftest import Env, run_execute

StartFn = Callable[..., Env]


def conn(**settings: object) -> pb.ConnectionConfig:
    s = struct_pb2.Struct()
    s.update(settings)
    return pb.ConnectionConfig(settings=s)


def exec_start(custom: str = "ECHO", op: int = pb.OPERATION_TYPE_EXECUTE, **settings: object) -> pb.ExecuteStart:
    return pb.ExecuteStart(execution_id="e1", step_id="s1", connection=conn(**settings), operation_type=op, custom_type=custom)


def test_describe(start: StartFn) -> None:
    env = start(EchoConnector())
    d = env.stub.Describe(pb.DescribeRequest()).descriptor
    assert d.id == "example-echo" and d.host == pb.HOST_BRIDGE_PYTHON
    assert [o.custom_type for o in d.operations] == ["ECHO", "UPPERCASE"]
    assert d.capabilities.streaming


def test_test_redacts_secrets(start: StartFn) -> None:
    class Leaky(EchoConnector):
        def test(self, settings):  # type: ignore[no-untyped-def]
            raise RuntimeError(f"cannot login with {settings.secret('api_token').reveal()}")

    env = start(Leaky())
    r = env.stub.Test(pb.TestRequest(connection=conn(api_token="tok-SECRET-123")))
    assert not r.ok
    assert "tok-SECRET-123" not in r.diagnostic and "***" in r.diagnostic


def test_execute_per_document_statuses_and_isolation(start: StartFn) -> None:
    env = start(EchoConnector())
    out = run_execute(
        env,
        exec_start("UPPERCASE", api_token="tok-SECRET-123"),
        [("ok1", b"hello"), ("rej", b"!reject please"), ("crash", b"!crash"), ("leak", b"!leak"), ("ok2", "zażółć".encode())],
    )
    st = {k: v.status for k, v in out.results.items()}
    assert st == {
        "ok1": pb.DOCUMENT_STATUS_SUCCESS,
        "rej": pb.DOCUMENT_STATUS_APPLICATION_ERROR,
        "crash": pb.DOCUMENT_STATUS_FAILURE,
        "leak": pb.DOCUMENT_STATUS_FAILURE,
        "ok2": pb.DOCUMENT_STATUS_SUCCESS,
    }
    assert out.outputs["ok1.out1"] == b"HELLO"
    assert out.outputs["ok2.out1"].decode() == "ZAŻÓŁĆ"
    assert list(out.results["ok1"].output_document_ids) == ["ok1.out1"]
    assert out.results["ok1"].tracked["bytes"] == "5"
    assert "tok-SECRET-123" not in out.results["leak"].message
    assert "boom" in out.results["crash"].message


def test_large_payload_is_chunked_both_ways_with_bounded_chunks(start: StartFn) -> None:
    env = start(EchoConnector(), chunk_size=10_000)
    payload = bytes(range(256)) * 4096  # 1 MiB
    out = run_execute(env, exec_start("ECHO"), [("big", payload)], chunk=30_000, props={"a": "b"})
    assert bytes(out.outputs["big.out1"]) == payload
    chunks = out.output_chunks["big.out1"]
    assert len(chunks) == -(-len(payload) // 10_000)
    assert all(len(c.data) <= 10_000 for c in chunks)
    assert [c.last for c in chunks] == [False] * (len(chunks) - 1) + [True]
    assert dict(chunks[0].properties) == {"a": "b", "echo.operation": "ECHO"}


def test_empty_document_and_empty_batch(start: StartFn) -> None:
    env = start(EchoConnector())
    out = run_execute(env, exec_start("ECHO"), [("empty", b"")])
    assert out.results["empty"].status == pb.DOCUMENT_STATUS_SUCCESS
    assert bytes(out.outputs["empty.out1"]) == b""
    assert run_execute(env, exec_start("ECHO"), []).results == {}


def test_prefix_setting(start: StartFn) -> None:
    out = run_execute(start(EchoConnector()), exec_start("ECHO", prefix=">> "), [("d", b"x")])
    assert bytes(out.outputs["d.out1"]) == b">> x"


def test_execute_must_start_with_start(start: StartFn) -> None:
    env = start(EchoConnector())
    with pytest.raises(grpc.RpcError) as e:
        list(env.stub.Execute(iter([pb.ExecuteRequest(end=pb.ExecuteEnd())])))
    assert e.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_interleaved_documents_fail_the_batch(start: StartFn) -> None:
    env = start(EchoConnector())

    def reqs():  # type: ignore[no-untyped-def]
        yield pb.ExecuteRequest(start=exec_start("ECHO"))
        yield pb.ExecuteRequest(chunk=pb.DocumentChunk(document_id="a", data=b"1"))
        yield pb.ExecuteRequest(chunk=pb.DocumentChunk(document_id="b", data=b"2", last=True))
        yield pb.ExecuteRequest(end=pb.ExecuteEnd())

    with pytest.raises(grpc.RpcError) as e:
        list(env.stub.Execute(reqs()))
    assert e.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_browse_unimplemented_for_echo(start: StartFn) -> None:
    env = start(EchoConnector())
    with pytest.raises(grpc.RpcError) as e:
        env.stub.Browse(pb.BrowseRequest())
    assert e.value.code() == grpc.StatusCode.UNIMPLEMENTED


def test_listen_is_unimplemented(start: StartFn) -> None:
    env = start(EchoConnector())
    with pytest.raises(grpc.RpcError) as e:
        list(env.stub.Listen(iter([pb.ListenRequest(start=pb.ListenStart(listener_id="l"))])))
    assert e.value.code() == grpc.StatusCode.UNIMPLEMENTED


def test_health(start: StartFn) -> None:
    env = start(EchoConnector())
    h = health_pb2_grpc.HealthStub(env.channel)
    for name in ("", "nurtuo.connector.v1.ConnectorService"):
        assert h.Check(health_pb2.HealthCheckRequest(service=name)).status == health_pb2.HealthCheckResponse.SERVING


def test_token_is_enforced(start: StartFn) -> None:
    env = start(EchoConnector(), token="s3cret-token", metadata_token="wrong")
    with pytest.raises(grpc.RpcError) as e:
        env.stub.Describe(pb.DescribeRequest())
    assert e.value.code() == grpc.StatusCode.UNAUTHENTICATED
    with pytest.raises(grpc.RpcError) as e2:
        list(env.stub.Execute(iter([pb.ExecuteRequest(start=exec_start())])))
    assert e2.value.code() == grpc.StatusCode.UNAUTHENTICATED

    ok = start(EchoConnector(), token="s3cret-token", metadata_token="s3cret-token")
    assert ok.stub.Describe(pb.DescribeRequest()).descriptor.id == "example-echo"
    assert run_execute(ok, exec_start("ECHO"), [("d", b"x")]).results["d"].status == pb.DOCUMENT_STATUS_SUCCESS

    anon = start(EchoConnector(), token="s3cret-token")
    with pytest.raises(grpc.RpcError) as e3:
        anon.stub.Describe(pb.DescribeRequest())
    assert e3.value.code() == grpc.StatusCode.UNAUTHENTICATED


# ---- CSV example ----


@pytest.fixture
def csv_dir(tmp_path: Path) -> Path:
    (tmp_path / "people.csv").write_text("name,age\nAda,36\nGrace,45\n", encoding="utf-8")
    (tmp_path / "other.csv").write_text("a;b\n1;2\n", encoding="utf-8")
    return tmp_path


def test_csv_browse_and_test(start: StartFn, csv_dir: Path) -> None:
    env = start(CsvReaderConnector())
    assert env.stub.Test(pb.TestRequest(connection=conn(base_dir=str(csv_dir)))).ok
    assert not env.stub.Test(pb.TestRequest(connection=conn())).ok
    ids = env.stub.Browse(pb.BrowseRequest(connection=conn(base_dir=str(csv_dir)))).object_types
    assert [o.id for o in ids] == ["other.csv", "people.csv"]
    r = env.stub.Browse(pb.BrowseRequest(connection=conn(base_dir=str(csv_dir)), object_type_id="people.csv"))
    schema = json.loads(r.response_profile.definition)
    assert list(schema["properties"]) == ["name", "age"]
    assert r.response_profile.format == pb.Profile.FORMAT_JSON_SCHEMA


def test_csv_query_and_traversal(start: StartFn, csv_dir: Path) -> None:
    env = start(CsvReaderConnector())
    s = exec_start("", op=pb.OPERATION_TYPE_QUERY, base_dir=str(csv_dir))
    out = run_execute(env, s, [("q1", b'{"file": "people.csv"}'), ("q2", b'{"file": "../secret.csv"}'), ("q3", b"not json"), ("q4", b'{"file": "nope.csv"}')])
    assert out.results["q1"].status == pb.DOCUMENT_STATUS_SUCCESS
    rows = [json.loads(line) for line in bytes(out.outputs["q1.out1"]).splitlines()]
    assert rows == [{"name": "Ada", "age": "36"}, {"name": "Grace", "age": "45"}]
    assert out.results["q1"].tracked["rows"] == "2"
    for k in ("q2", "q3", "q4"):
        assert out.results[k].status == pb.DOCUMENT_STATUS_APPLICATION_ERROR


def test_csv_to_jsonl_streams_large_input(start: StartFn) -> None:
    env = start(CsvReaderConnector(), chunk_size=4096)
    body = "id,txt\n" + "".join(f"{i},row {i}\n" for i in range(20_000))
    s = exec_start("CSV_TO_JSONL")
    s.options.update({"max_rows": 15000})
    out = run_execute(env, s, [("c", body.encode())], chunk=1000)
    lines = bytes(out.outputs["c.out1"]).splitlines()
    assert len(lines) == 15000 and json.loads(lines[-1]) == {"id": "14999", "txt": "row 14999"}
    assert out.results["c"].tracked["rows"] == "15000"
    assert max(len(c.data) for c in out.output_chunks["c.out1"]) <= 4096


def test_csv_bad_delimiter_option(start: StartFn) -> None:
    s = exec_start("CSV_TO_JSONL")
    s.options.update({"delimiter": "ab"})
    out = run_execute(start(CsvReaderConnector()), s, [("c", b"a,b\n1,2\n")])
    assert out.results["c"].status == pb.DOCUMENT_STATUS_APPLICATION_ERROR
