# SPDX-License-Identifier: Apache-2.0
"""Secret handling for connection settings.

The runtime resolves secret references and injects the values *in memory only* into
``ConnectionConfig.settings`` on every call. A bridge must never log or persist them and must
never echo them back (``Test`` diagnostics, ``DocumentResult.message``, exceptions). This module
gives connector authors three tools for that:

* :class:`Secret` - a wrapper whose ``repr``/``str`` never reveal the value.
* :class:`ConnectionSettings` - read-only mapping over the settings struct that hands out
  :class:`Secret` objects for secret keys.
* :class:`Redactor` - replaces every known secret value in a string with ``***``; the server
  applies it to everything that leaves the process (diagnostics, error messages, logs).
"""

from __future__ import annotations

import re
from collections.abc import Iterator, Mapping
from typing import Any

#: Keys whose values are treated as secrets unless the connector lists its own ``secret_keys``.
DEFAULT_SECRET_KEY_PATTERN = re.compile(
    r"(pass(word|wd|phrase)?|secret|token|api[_-]?key|credential|private[_-]?key|authorization)",
    re.IGNORECASE,
)

#: Values shorter than this are not redacted (redacting a 1-char value would mangle messages).
MIN_REDACT_LENGTH = 4

REDACTED = "***"


class Secret:
    """An opaque secret value. Call :meth:`reveal` at the exact point of use."""

    __slots__ = ("_value",)

    def __init__(self, value: str) -> None:
        self._value = value

    def reveal(self) -> str:
        return self._value

    def __repr__(self) -> str:
        return "Secret(***)"

    def __str__(self) -> str:
        return REDACTED

    def __format__(self, spec: str) -> str:
        return REDACTED

    def __bool__(self) -> bool:
        return bool(self._value)

    def __eq__(self, other: object) -> bool:
        return isinstance(other, Secret) and other._value == self._value

    def __hash__(self) -> int:
        return hash(("Secret", self._value))

    def __reduce__(self) -> Any:  # pragma: no cover - defensive
        raise TypeError("Secret objects cannot be pickled")


class Redactor:
    """Masks known secret values in text."""

    def __init__(self, values: set[str] | None = None) -> None:
        self._values: list[str] = []
        for v in values or ():
            self.add(v)

    def add(self, value: str) -> None:
        if len(value) >= MIN_REDACT_LENGTH and value not in self._values:
            self._values.append(value)
            self._values.sort(key=len, reverse=True)  # longest first: no partial leftovers

    def __call__(self, text: str) -> str:
        for v in self._values:
            if v in text:
                text = text.replace(v, REDACTED)
        return text

    def __bool__(self) -> bool:
        return bool(self._values)


def _is_secret_key(key: str, secret_keys: frozenset[str] | None) -> bool:
    if secret_keys is not None:
        return key in secret_keys
    return DEFAULT_SECRET_KEY_PATTERN.search(key) is not None


def _walk_secret_strings(value: Any, secret_keys: frozenset[str] | None, under_secret: bool, out: set[str]) -> None:
    if isinstance(value, Mapping):
        for k, v in value.items():
            _walk_secret_strings(v, secret_keys, under_secret or _is_secret_key(str(k), secret_keys), out)
    elif isinstance(value, (list, tuple)):
        for v in value:
            _walk_secret_strings(v, secret_keys, under_secret, out)
    elif isinstance(value, str) and under_secret:
        out.add(value)


class ConnectionSettings(Mapping[str, Any]):
    """Read-only view of ``ConnectionConfig.settings``.

    Top-level keys that look like secrets (or are listed in ``secret_keys``) are returned as
    :class:`Secret`; everything else as plain JSON values. ``redactor`` knows every secret
    string found anywhere in the settings, including nested ones.
    """

    def __init__(self, raw: Mapping[str, Any] | None = None, secret_keys: frozenset[str] | None = None) -> None:
        self._raw: dict[str, Any] = dict(raw or {})
        self._secret_keys = secret_keys
        values: set[str] = set()
        _walk_secret_strings(self._raw, secret_keys, False, values)
        self.redactor = Redactor(values)

    def __getitem__(self, key: str) -> Any:
        v = self._raw[key]
        if _is_secret_key(key, self._secret_keys) and isinstance(v, str):
            return Secret(v)
        return v

    def __iter__(self) -> Iterator[str]:
        return iter(self._raw)

    def __len__(self) -> int:
        return len(self._raw)

    def secret(self, key: str) -> Secret:
        """Return ``key`` as a :class:`Secret` (raises ``KeyError`` if missing)."""
        v = self._raw[key]
        if not isinstance(v, str):
            raise TypeError(f"setting {key!r} is not a string")
        return Secret(v)

    def public(self) -> dict[str, Any]:
        """A copy with every secret string (at any depth) replaced by ``***``; safe to log."""
        out: dict[str, Any] = _scrub(self._raw, self._secret_keys, False)
        return out

    def __repr__(self) -> str:
        return f"ConnectionSettings({self.public()!r})"


def _scrub(value: Any, secret_keys: frozenset[str] | None, under_secret: bool) -> Any:
    if isinstance(value, Mapping):
        return {k: _scrub(v, secret_keys, under_secret or _is_secret_key(str(k), secret_keys)) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_scrub(v, secret_keys, under_secret) for v in value]
    if isinstance(value, str) and under_secret:
        return REDACTED
    return value
