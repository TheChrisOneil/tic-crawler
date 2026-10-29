"""Validate TOCs against the pinned CMS JSON Schema, one reporting_structure item at a time."""

from __future__ import annotations

import copy
import hashlib
import json
import re
from functools import cache
from importlib import resources
from typing import Any

from jsonschema import Draft7Validator, FormatChecker

from .models import ValidationReport

TOC_SCHEMA = "table-of-contents.json"
MAX_KINDS = 200
_INDEX = re.compile(r"\[\d+\]")
_QUOTED_URL = re.compile(r"'https?://[^']*'")


@cache
def load_schema(version: str, name: str = TOC_SCHEMA) -> dict:
    path = resources.files("tic_crawler") / "schemas" / f"v{version}" / name
    if not path.is_file():
        raise FileNotFoundError(f"schema {name} for version {version} is not vendored")
    return json.loads(path.read_text())


class TocValidator:
    """Streaming-friendly validator.

    The full schema is split into an item schema (reporting_structure entry) and a header
    schema (everything else), so a multi-GB TOC never has to be loaded at once.
    uniqueItems on reporting_structure is checked with item hashes.
    """

    def __init__(self, version: str, max_errors: int = 50):
        schema = load_schema(version)
        self.version = version
        self.max_errors = max_errors
        defs = schema["definitions"]
        fc = FormatChecker()
        self._item = Draft7Validator(
            {"definitions": defs, "$ref": "#/definitions/reporting_structure"}, format_checker=fc
        )
        header = copy.deepcopy(schema)
        header["properties"].pop("reporting_structure", None)
        header["required"] = [r for r in header["required"] if r != "reporting_structure"]
        self._header = Draft7Validator(header, format_checker=fc)
        self.report = ValidationReport(pinned_version=version)
        self._seen: set[bytes] = set()
        self._kinds: dict[str, dict] = {}

    def _add(self, msg: str) -> None:
        self.report.error_count += 1
        self.report.valid = False
        if len(self.report.errors) < self.max_errors:
            self.report.errors.append(msg[:300])
        location, _, detail = msg.partition(": ")
        kind = f"{_INDEX.sub('[*]', location)}: {_QUOTED_URL.sub("'<url>'", detail)}"[:240]
        entry = self._kinds.get(kind)
        if entry is None:
            if len(self._kinds) >= MAX_KINDS:
                kind = "(other errors)"
                entry = self._kinds.setdefault(kind, {"kind": kind, "count": 0, "examples": []})
            else:
                entry = self._kinds[kind] = {"kind": kind, "count": 0, "examples": []}
        entry["count"] += 1
        if len(entry["examples"]) < 5:
            entry["examples"].append(location)

    def validate_item(self, index: int, item: Any) -> None:
        self.report.item_count += 1
        before = self.report.error_count
        digest = hashlib.sha256(json.dumps(item, sort_keys=True, default=str).encode()).digest()
        if digest in self._seen:
            self._add(f"reporting_structure[{index}]: duplicate item (uniqueItems)")
        self._seen.add(digest)
        for err in sorted(self._item.iter_errors(item), key=lambda e: list(e.absolute_path)):
            self._add(f"reporting_structure[{index}]{_path(err.absolute_path)}: {err.message}")
        if self.report.error_count > before:
            self.report.invalid_items += 1

    def validate_header(self, header: dict) -> None:
        top_type = header.get("_top_level_type")
        if top_type not in (None, "start_map"):
            self._add("$: top level is not a JSON object")
            return
        clean = {k: v for k, v in header.items() if not k.startswith("_")}
        if "reporting_structure" in clean:
            self._add("reporting_structure: is not of type 'array'")
            clean.pop("reporting_structure")
        version = clean.get("version")
        self.report.file_version = "" if version is None else str(version)
        for err in sorted(self._header.iter_errors(clean), key=lambda e: list(e.absolute_path)):
            self._add(f"${_path(err.absolute_path)}: {err.message}")
        if not header.get("_has_reporting_structure"):
            if "reporting_structure" not in header:
                self._add("$: 'reporting_structure' is a required property")
        elif self.report.item_count == 0:
            self._add("reporting_structure: [] should be non-empty")

    def finish(self) -> ValidationReport:
        self.report.error_kinds = sorted(self._kinds.values(), key=lambda k: (-k["count"], k["kind"]))
        return self.report


def _path(parts) -> str:
    out = ""
    for p in parts:
        out += f"[{p}]" if isinstance(p, int) else f".{p}"
    return out
