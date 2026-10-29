"""Streaming TOC parsing with ijson. Memory stays bounded by one reporting_structure item."""

from __future__ import annotations

import gzip
import hashlib
import io
import shutil
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, BinaryIO

import ijson

from .models import MrfEntry

GZIP_MAGIC = b"\x1f\x8b"
ITEM_PREFIX = "reporting_structure.item"
SCALAR_EVENTS = {"string", "number", "boolean", "null"}
CHUNK = 1024 * 1024


def is_gzip(path: Path) -> bool:
    with open(path, "rb") as fh:
        return fh.read(2) == GZIP_MAGIC


def open_json(path: Path) -> BinaryIO:
    """Open a TOC file for reading, decompressing on the fly when it is gzipped."""
    return gzip.open(path, "rb") if is_gzip(path) else open(path, "rb")


def sha256_of_json(path: Path) -> str:
    """SHA-256 of the decompressed TOC JSON, so gzip wrapping does not change the hash."""
    h = hashlib.sha256()
    with open_json(path) as fh:
        while chunk := fh.read(CHUNK):
            h.update(chunk)
    return h.hexdigest()


def write_gzip_copy(src: Path, dest: Path) -> None:
    """Store the TOC as .json.gz. Already-gzipped input is copied byte for byte."""
    dest.parent.mkdir(parents=True, exist_ok=True)
    if is_gzip(src):
        shutil.copyfile(src, dest)
        return
    with open(src, "rb") as fin, open(dest, "wb") as raw:
        # mtime=0 keeps the output deterministic for identical input.
        with gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=0) as gz:
            shutil.copyfileobj(fin, gz, CHUNK)


@dataclass
class TocEvent:
    kind: str  # "item" or "header"
    index: int = -1
    value: Any = None


def iter_toc(fp: BinaryIO | bytes) -> Iterator[TocEvent]:
    """Yield each reporting_structure item as it completes, then the header scalars.

    Key order in a TOC is not guaranteed, so the header is only final at the end.
    The header dict also carries "_has_reporting_structure" and "_top_level_type".
    """
    if isinstance(fp, bytes):
        fp = io.BytesIO(fp)
    header: dict[str, Any] = {}
    builder: ijson.ObjectBuilder | None = None
    index = 0
    has_rs = False
    top_type = None
    for prefix, event, value in ijson.parse(fp, use_float=True):
        if top_type is None:
            top_type = event
        if builder is not None:
            builder.event(event, value)
            if prefix == ITEM_PREFIX and event in ("end_map", "end_array"):
                yield TocEvent("item", index, builder.value)
                index += 1
                builder = None
            continue
        if prefix == "reporting_structure" and event == "start_array":
            has_rs = True
        elif prefix == ITEM_PREFIX:
            if event in ("start_map", "start_array"):
                builder = ijson.ObjectBuilder()
                builder.event(event, value)
            elif event in SCALAR_EVENTS:
                yield TocEvent("item", index, value)
                index += 1
        elif prefix and "." not in prefix and event in SCALAR_EVENTS:
            header[prefix] = value
        elif prefix == "reporting_structure" and (event in SCALAR_EVENTS or event == "start_map"):
            header["reporting_structure"] = value if event in SCALAR_EVENTS else {}
    header["_has_reporting_structure"] = has_rs
    header["_top_level_type"] = top_type
    yield TocEvent("header", index, header)


@dataclass
class MrfCollector:
    """Collects unique MRF URLs and the plans that reference each one."""

    name_cap: int = 50
    entries: dict[str, MrfEntry] = field(default_factory=dict)
    _plan_keys: dict[str, set[int]] = field(default_factory=dict)

    def add_item(self, item: Any) -> None:
        if not isinstance(item, dict):
            return
        plans = [p for p in item.get("reporting_plans") or [] if isinstance(p, dict)]
        files: list[tuple[str, dict]] = []
        for f in item.get("in_network_files") or []:
            if isinstance(f, dict):
                files.append(("in-network", f))
        aa = item.get("allowed_amount_file")
        if isinstance(aa, dict):
            files.append(("allowed-amount", aa))
        for file_type, loc in files:
            url = loc.get("location")
            if not isinstance(url, str) or not url:
                continue
            entry = self.entries.get(url)
            if entry is None:
                entry = self.entries[url] = MrfEntry(url=url, file_type=file_type)
                self._plan_keys[url] = set()
            desc = loc.get("description")
            if isinstance(desc, str) and desc not in entry.descriptions and len(entry.descriptions) < 5:
                entry.descriptions.append(desc)
            keys = self._plan_keys[url]
            for p in plans:
                key = hash((p.get("plan_id_type"), p.get("plan_id"), p.get("plan_name")))
                if key in keys:
                    continue
                keys.add(key)
                entry.plan_count += 1
                _add_capped(entry.plan_id_types, p.get("plan_id_type"), 10)
                _add_capped(entry.plan_market_types, p.get("plan_market_type"), 10)
                name = p.get("plan_name")
                if isinstance(name, str) and name not in entry.plan_names:
                    if len(entry.plan_names) < self.name_cap:
                        entry.plan_names.append(name)
                    else:
                        entry.plans_truncated = True

    def __len__(self) -> int:
        return len(self.entries)


def _add_capped(values: list[str], value: Any, cap: int) -> None:
    if isinstance(value, str) and value not in values and len(values) < cap:
        values.append(value)
