"""Payer discovery. Each source is a Strategy; new sources register themselves on subclassing."""

from __future__ import annotations

import csv
import io
import re
import tempfile
import zipfile
from abc import ABC, abstractmethod
from datetime import date
from pathlib import Path
from typing import ClassVar
from urllib.parse import urlsplit

import yaml

from .config import Settings
from .fetch import FetchError, HttpClient, RobotsDisallowed
from .models import Payer

_SUFFIXES = {
    "inc", "incorporated", "co", "company", "corp", "corporation", "llc", "lp", "llp", "ltd",
    "the", "pc", "pa",
}


def normalize_name(name: str) -> str:
    """Normalized legal name used for de-duplication."""
    s = name.lower().replace("&", " and ")
    s = re.sub(r"[^a-z0-9 ]+", " ", s)
    words = s.split()
    while words and words[-1] in _SUFFIXES:
        words.pop()
    while words and words[0] == "the":
        words.pop(0)
    return " ".join(words)


def slugify(name: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", normalize_name(name)).strip("-")[:60] or "payer"


class DiscoverySource(ABC):
    """A way to find payers. Subclasses register automatically by `name`."""

    name: ClassVar[str] = ""
    order: ClassVar[int] = 100
    registry: ClassVar[dict[str, type[DiscoverySource]]] = {}

    def __init_subclass__(cls, **kw):
        super().__init_subclass__(**kw)
        if cls.name:
            DiscoverySource.registry[cls.name] = cls

    def __init__(self, settings: Settings):
        self.settings = settings

    def is_active(self, today: date) -> bool:
        return True

    @abstractmethod
    async def discover(self, http: HttpClient, known: list[Payer]) -> list[Payer]:
        """Return payers found by this source. `known` holds payers from earlier sources."""


class SeedFileSource(DiscoverySource):
    """Hand-maintained seeds/payers.yaml."""

    name = "seed"
    order = 10

    def __init__(self, settings: Settings, path: str | Path | None = None):
        super().__init__(settings)
        self.path = Path(path or settings.seed_file)

    async def discover(self, http: HttpClient, known: list[Payer]) -> list[Payer]:
        return load_seed_file(self.path)


def load_seed_file(path: Path) -> list[Payer]:
    data = yaml.safe_load(path.read_text()) or {}
    payers = []
    for entry in data.get("payers", []):
        legal = entry["legal_name"]
        payers.append(
            Payer(
                payer_id=entry.get("payer_id") or slugify(legal),
                legal_name=legal,
                hios_ids=[str(h) for h in entry.get("hios_ids", [])],
                states=list(entry.get("states", [])),
                landing_url=entry.get("landing_url", ""),
                toc_url=entry.get("toc_url", ""),
                toc_link_pattern=entry.get("toc_link_pattern", ""),
                toc_file=entry.get("toc_file", ""),
                kind=entry.get("kind", "issuer"),
                aliases=list(entry.get("aliases", [])),
                sources=["seed"],
                notes=entry.get("notes", ""),
            )
        )
    return payers


class ExchangePufSource(DiscoverySource):
    """CMS Exchange Public Use Files: issuer names, HIOS issuer IDs and states.

    Covers HealthCare.gov issuers only; state-based exchanges and group-only carriers
    must come from the seed file.
    """

    name = "exchange_puf"
    order = 20
    ID_COLUMNS = ("IssuerId", "Issuer ID", "Issuer_ID", "HIOS Issuer ID", "HIOS_ISSUER_ID")
    NAME_COLUMNS = ("IssuerMarketPlaceMarketingName", "Issuer Name", "Issuer_Name", "IssuerName",
                    "Issuer Marketing Name")
    STATE_COLUMNS = ("StateCode", "State", "State Code", "STATE")

    def __init__(self, settings: Settings, url: str | None = None):
        super().__init__(settings)
        self.url = url if url is not None else settings.exchange_puf_url

    async def discover(self, http: HttpClient, known: list[Payer]) -> list[Payer]:
        if not self.url:
            return []
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "puf"
            size = 0
            async with http.stream_get(self.url) as resp:
                with path.open("wb") as fh:
                    async for chunk in resp.aiter_bytes():
                        size += len(chunk)
                        if size > self.settings.exchange_puf_max_bytes:
                            raise FetchError(self.url, resp.status_code, "PUF exceeds size cap")
                        fh.write(chunk)
            return list(parse_puf(path).values())


def _pick(header: list[str], options: tuple[str, ...]) -> str | None:
    lowered = {h.strip().lower(): h for h in header}
    for o in options:
        if o.lower() in lowered:
            return lowered[o.lower()]
    return None


def _iter_csv_texts(path: Path):
    if zipfile.is_zipfile(path):
        with zipfile.ZipFile(path) as zf:
            for info in zf.infolist():
                if info.filename.lower().endswith(".csv"):
                    with zf.open(info) as raw:
                        yield io.TextIOWrapper(raw, encoding="utf-8-sig", errors="replace", newline="")
    else:
        with path.open(encoding="utf-8-sig", errors="replace", newline="") as fh:
            yield fh


def parse_puf(path: Path) -> dict[str, Payer]:
    """Stream PUF CSV rows into one Payer per HIOS issuer ID."""
    by_id: dict[str, Payer] = {}
    for text in _iter_csv_texts(path):
        reader = csv.DictReader(text)
        header = reader.fieldnames or []
        id_col = _pick(header, ExchangePufSource.ID_COLUMNS)
        name_col = _pick(header, ExchangePufSource.NAME_COLUMNS)
        state_col = _pick(header, ExchangePufSource.STATE_COLUMNS)
        if not id_col or not name_col:
            continue
        for row in reader:
            hios = (row.get(id_col) or "").strip()
            name = (row.get(name_col) or "").strip()
            if not hios or not name:
                continue
            p = by_id.get(hios)
            if p is None:
                p = by_id[hios] = Payer(
                    payer_id=f"hios-{hios}", legal_name=name, hios_ids=[hios],
                    sources=["exchange_puf"],
                )
            state = (row.get(state_col) or "").strip().upper() if state_col else ""
            if state and state not in p.states:
                p.states.append(state)
    return by_id


class RootTxtSource(DiscoverySource):
    """Root .txt file required on payer sites from 2027-09-06.

    Stub: until the date applies it returns nothing. After that it reads
    https://<payer host>/<root_txt_filename> and takes https lines as TOC locations and an
    email as the contact. Confirm the file name and format against the final rule text
    before relying on it.
    """

    name = "root_txt"
    order = 90

    def is_active(self, today: date) -> bool:
        return today >= self.settings.root_txt_from

    async def discover(self, http: HttpClient, known: list[Payer]) -> list[Payer]:
        found: list[Payer] = []
        for payer in known:
            if not payer.landing_url:
                continue
            parts = urlsplit(payer.landing_url)
            url = f"{parts.scheme}://{parts.netloc}/{self.settings.root_txt_filename}"
            try:
                text = await http.get_text(url)
            except (FetchError, RobotsDisallowed):
                continue
            tocs, contact = parse_root_txt(text)
            if tocs:
                found.append(Payer(
                    payer_id=payer.payer_id, legal_name=payer.legal_name, toc_url=tocs[0],
                    sources=["root_txt"], notes=f"contact: {contact}" if contact else "",
                ))
        return found


def parse_root_txt(text: str) -> tuple[list[str], str]:
    tocs = re.findall(r"https://\S+", text)
    m = re.search(r"[\w.+-]+@[\w-]+\.[\w.-]+", text)
    return tocs, m.group(0) if m else ""


def dedupe_payers(payers: list[Payer]) -> list[Payer]:
    """Merge payers sharing a normalized legal name (or alias) or any HIOS ID.

    Earlier entries win for scalar fields, so seed data overrides the PUF.
    """
    parent = list(range(len(payers)))

    def find(i: int) -> int:
        while parent[i] != i:
            parent[i] = parent[parent[i]]
            i = parent[i]
        return i

    def union(a: int, b: int) -> None:
        ra, rb = find(a), find(b)
        if ra != rb:
            parent[max(ra, rb)] = min(ra, rb)

    first_by_key: dict[str, int] = {}
    for i, p in enumerate(payers):
        keys = {"n:" + normalize_name(n) for n in [p.legal_name, *p.aliases] if n}
        keys |= {"h:" + h for h in p.hios_ids}
        for k in keys:
            if k in first_by_key:
                union(first_by_key[k], i)
            else:
                first_by_key[k] = i

    groups: dict[int, list[Payer]] = {}
    for i, p in enumerate(payers):
        groups.setdefault(find(i), []).append(p)

    merged: list[Payer] = []
    for root in sorted(groups):
        group = groups[root]
        base = Payer.from_dict(group[0].to_dict())
        for other in group[1:]:
            for f in ("landing_url", "toc_url", "toc_link_pattern", "toc_file", "notes"):
                if not getattr(base, f) and getattr(other, f):
                    setattr(base, f, getattr(other, f))
            for f in ("hios_ids", "states", "aliases", "sources"):
                cur = getattr(base, f)
                cur.extend(v for v in getattr(other, f) if v not in cur)
            if normalize_name(other.legal_name) != normalize_name(base.legal_name):
                if other.legal_name not in base.aliases:
                    base.aliases.append(other.legal_name)
        base.hios_ids.sort()
        base.states.sort()
        merged.append(base)
    return merged


class DiscoveryRegistry:
    def __init__(self, settings: Settings, sources: list[DiscoverySource] | None = None):
        self.settings = settings
        if sources is None:
            classes = sorted(DiscoverySource.registry.values(), key=lambda c: c.order)
            sources = [c(settings) for c in classes]
        self.sources = sources

    async def run(self, http: HttpClient, today: date,
                  only: list[str] | None = None) -> tuple[list[Payer], dict[str, str]]:
        payers: list[Payer] = []
        status: dict[str, str] = {}
        for src in self.sources:
            if only and src.name not in only:
                continue
            if not src.is_active(today):
                status[src.name] = "inactive"
                continue
            try:
                found = await src.discover(http, dedupe_payers(payers))
                status[src.name] = f"ok:{len(found)}"
                payers.extend(found)
            except Exception as e:  # one source failing must not stop the others
                status[src.name] = f"error:{type(e).__name__}: {e}"[:300]
        return dedupe_payers(payers), status
