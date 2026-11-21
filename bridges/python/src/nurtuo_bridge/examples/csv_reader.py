# SPDX-License-Identifier: Apache-2.0
"""CSV example connector (QUERY, browse, EXECUTE ``CSV_TO_JSONL``).

Run it:  ``python -m nurtuo_bridge nurtuo_bridge.examples.csv_reader:CsvReaderConnector``

Connection settings: ``base_dir`` (directory with ``*.csv`` files).

* ``QUERY`` - the input document is JSON ``{"file": "people.csv"}`` (relative to ``base_dir``,
  path traversal is rejected); the output is one JSON object per row, newline separated.
* ``EXECUTE CSV_TO_JSONL`` - the input document *is* a CSV payload (streamed); same output.
  Options: ``delimiter`` (default ``,``), ``max_rows``.
* Browse lists the CSV files as object types; browsing one returns a JSON Schema built from
  its header row.
"""

from __future__ import annotations

import csv
import io
import json
from collections.abc import Iterator
from pathlib import Path
from typing import IO

from nurtuo_bridge import (
    ApplicationError,
    BrowseContext,
    BrowseResult,
    ConnectionSettings,
    Connector,
    ExecuteContext,
    InputDocument,
    Outcome,
    OutputDocument,
    Profile,
    TestResult,
    make_descriptor,
    operation,
)
from nurtuo_bridge._gen.nurtuo.connector.v1 import connector_pb2 as pb


def _base_dir(settings: ConnectionSettings) -> Path:
    base = settings.get("base_dir")
    if not isinstance(base, str) or not base:
        raise ApplicationError("connection setting 'base_dir' is required")
    return Path(base).resolve()


def _resolve(base: Path, name: str) -> Path:
    p = (base / name).resolve()
    if base != p and base not in p.parents:
        raise ApplicationError("file is outside of base_dir")
    if not p.is_file():
        raise ApplicationError(f"file not found: {name}")
    return p


def _rows_jsonl(binary: IO[bytes], delimiter: str, max_rows: int | None) -> Iterator[bytes]:
    text = io.TextIOWrapper(binary, encoding="utf-8-sig", newline="")
    reader = csv.DictReader(text, delimiter=delimiter)
    for n, row in enumerate(reader, start=1):
        if max_rows is not None and n > max_rows:
            return
        yield (json.dumps(row, ensure_ascii=False) + "\n").encode()


class CsvReaderConnector(Connector):
    def describe(self) -> pb.Descriptor:
        return make_descriptor(
            id="example-csv",
            version="0.1.0",
            display_name="CSV reader (Python example)",
            connection_schema={
                "type": "object",
                "required": ["base_dir"],
                "properties": {"base_dir": {"type": "string"}},
            },
            operations=[
                operation(pb.OPERATION_TYPE_QUERY, supports_browse=True),
                operation(
                    pb.OPERATION_TYPE_EXECUTE,
                    custom_type="CSV_TO_JSONL",
                    options_schema={
                        "type": "object",
                        "properties": {"delimiter": {"type": "string"}, "max_rows": {"type": "integer"}},
                    },
                ),
            ],
            tracked_properties=["rows"],
        )

    def test(self, settings: ConnectionSettings) -> TestResult:
        try:
            base = _base_dir(settings)
        except ApplicationError as e:
            return TestResult(False, str(e))
        if not base.is_dir():
            return TestResult(False, "base_dir is not a directory")
        return TestResult(True, f"{sum(1 for _ in base.glob('*.csv'))} csv file(s) found")

    def browse(self, ctx: BrowseContext) -> BrowseResult:
        base = _base_dir(ctx.settings)
        if not ctx.object_type_id:
            return BrowseResult(object_types=[(p.name, p.stem) for p in sorted(base.glob("*.csv"))])
        path = _resolve(base, ctx.object_type_id)
        with path.open("r", encoding="utf-8-sig", newline="") as f:
            header = next(csv.reader(f), [])
        schema = {
            "type": "object",
            "properties": {h: {"type": "string"} for h in header},
        }
        return BrowseResult(response_profile=Profile(json.dumps(schema).encode()))

    def execute_document(self, ctx: ExecuteContext, doc: InputDocument) -> Outcome:
        delimiter = str(ctx.options.get("delimiter", ","))
        raw_max = ctx.options.get("max_rows")
        max_rows = int(raw_max) if raw_max is not None else None
        if len(delimiter) != 1:
            raise ApplicationError("option 'delimiter' must be a single character")
        if ctx.operation_type == pb.OPERATION_TYPE_QUERY:
            try:
                request = json.loads(doc.read(limit=64 * 1024))
                name = request["file"]
            except (ValueError, KeyError, TypeError) as e:
                raise ApplicationError("QUERY input must be JSON like {\"file\": \"name.csv\"}") from e
            path = _resolve(_base_dir(ctx.settings), str(name))
            # Opened here, consumed (and closed) while the output is streamed.
            data = _file_rows(path, delimiter, max_rows)
        elif ctx.custom_type == "CSV_TO_JSONL":
            data = _rows_jsonl(doc.open(), delimiter, max_rows)
        else:
            raise ApplicationError(f"unsupported operation {ctx.custom_type or ctx.operation_type}")

        rows = 0

        def counted() -> Iterator[bytes]:
            nonlocal rows
            for line in data:
                rows += 1
                yield line

        return Outcome(
            outputs=[OutputDocument(data=counted(), properties={"content-type": "application/x-ndjson"})],
            tracked=lambda: {"rows": str(rows)},
        )


def _file_rows(path: Path, delimiter: str, max_rows: int | None) -> Iterator[bytes]:
    with path.open("rb") as f:
        yield from _rows_jsonl(f, delimiter, max_rows)
