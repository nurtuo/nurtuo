# SPDX-License-Identifier: Apache-2.0
"""Process-level tests: handshake line, stdout hygiene, graceful shutdown, contract negotiation."""

from __future__ import annotations

import os
import subprocess
import sys
import time
from pathlib import Path

import grpc
import pytest

from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2_grpc as pb_grpc
from nurtuo_bridge.server import ListenSpec, format_handshake, negotiate_contract

SRC = str(Path(__file__).resolve().parents[1] / "src")
SPEC = "nurtuo_bridge.examples.echo:EchoConnector"


def spawn(env_extra: dict[str, str]) -> subprocess.Popen[str]:
    env = {**os.environ, "PYTHONPATH": SRC, "PYTHONUNBUFFERED": "1", **env_extra}
    return subprocess.Popen(
        [sys.executable, "-m", "nurtuo_bridge", SPEC],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env,
    )


def test_handshake_format_and_stdout_only_has_the_handshake() -> None:
    p = spawn({"NURTUO_BRIDGE_LISTEN": "tcp:127.0.0.1:0", "NURTUO_BRIDGE_WATCH_STDIN": "1"})
    try:
        assert p.stdout is not None and p.stdin is not None
        line = p.stdout.readline().strip()
        parts = line.split("|")
        assert parts[:2] == ["NURTUO-BRIDGE", "1"] and parts[2] == "tcp" and parts[4] == "nurtuo.connector.v1"
        assert parts[5].startswith("python/")
        with grpc.insecure_channel(parts[3]) as ch:
            d = pb_grpc.ConnectorServiceStub(ch).Describe(pb.DescribeRequest(), timeout=10).descriptor
            assert d.id == "example-echo"
        # closing stdin = the runtime went away: graceful exit with code 0
        p.stdin.close()
        assert p.wait(timeout=15) == 0
        assert p.stdout.read() == ""  # nothing but the handshake ever hits stdout
    finally:
        if p.poll() is None:
            p.kill()
        p.wait()
        for f in (p.stdout, p.stderr, p.stdin):
            if f:
                f.close()


def test_contract_mismatch_exits_3_without_handshake() -> None:
    p = spawn({"NURTUO_BRIDGE_CONTRACT": "nurtuo.connector.v2", "NURTUO_BRIDGE_LISTEN": "tcp:127.0.0.1:0"})
    out, err = p.communicate(timeout=30)
    assert p.returncode == 3 and out == "" and "is supported" in err and "nurtuo.connector.v2" in err


def test_negotiation_picks_first_supported_in_runtime_preference_order() -> None:
    assert negotiate_contract(None) == "nurtuo.connector.v1"
    assert negotiate_contract("nurtuo.connector.v2,nurtuo.connector.v1") == "nurtuo.connector.v1"
    assert negotiate_contract("nurtuo.connector.v2") is None


def test_listen_spec_validation() -> None:
    assert ListenSpec.parse("tcp:127.0.0.1:0").network == "tcp"
    with pytest.raises(ValueError):
        ListenSpec.parse("tcp:0.0.0.0:0")  # never listen beyond loopback
    with pytest.raises(ValueError):
        ListenSpec.parse("bogus")
    if sys.platform == "win32":
        with pytest.raises(ValueError):
            ListenSpec.parse("unix:/tmp/x.sock")
    else:
        assert ListenSpec.parse("unix:/tmp/x.sock").address == "/tmp/x.sock"


def test_handshake_line_shape() -> None:
    assert format_handshake("tcp", "127.0.0.1:1234").startswith("NURTUO-BRIDGE|1|tcp|127.0.0.1:1234|nurtuo.connector.v1|python/")


@pytest.mark.skipif(sys.platform == "win32", reason="unix sockets and SIGTERM are POSIX only")
def test_unix_socket_and_sigterm(tmp_path: Path) -> None:
    sock = tmp_path / "b.sock"
    p = spawn({"NURTUO_BRIDGE_LISTEN": f"unix:{sock}"})
    try:
        assert p.stdout is not None
        parts = p.stdout.readline().strip().split("|")
        assert parts[2] == "unix" and parts[3] == str(sock)
        p.terminate()
        deadline = time.time() + 15
        while p.poll() is None and time.time() < deadline:
            time.sleep(0.05)
        assert p.returncode == 0 and not sock.exists()
    finally:
        if p.poll() is None:
            p.kill()
        p.wait()
