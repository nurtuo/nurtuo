# SPDX-License-Identifier: Apache-2.0
from __future__ import annotations

import pickle

import pytest

from nurtuo_bridge import ConnectionSettings, Redactor, Secret


def test_secret_never_reveals_itself() -> None:
    s = Secret("hunter2-value")
    assert "hunter2" not in repr(s) and "hunter2" not in str(s) and "hunter2" not in f"{s}"
    assert s.reveal() == "hunter2-value"
    with pytest.raises(TypeError):
        pickle.dumps(s)


def test_settings_wrap_secret_keys_by_name_heuristic() -> None:
    cfg = ConnectionSettings({"host": "db", "password": "s3cr3t-pw", "nested": {"api_key": "k-12345"}})
    assert cfg["host"] == "db"
    assert isinstance(cfg["password"], Secret)
    assert cfg.public() == {"host": "db", "password": "***", "nested": {"api_key": "***"}}
    assert cfg.redactor("login s3cr3t-pw and k-12345 on db") == "login *** and *** on db"
    assert "s3cr3t" not in repr(cfg)


def test_explicit_secret_keys_override_the_heuristic() -> None:
    cfg = ConnectionSettings({"token": "visible-token", "custom": "very-secret"}, frozenset({"custom"}))
    assert cfg["token"] == "visible-token"
    assert isinstance(cfg["custom"], Secret)
    assert cfg.redactor("very-secret visible-token") == "*** visible-token"


def test_redactor_replaces_longest_first_and_ignores_tiny_values() -> None:
    r = Redactor({"abcd", "abcdefgh", "x"})
    assert r("abcdefgh abcd x") == "*** *** x"
