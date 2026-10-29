"""Plain data types shared across the crawler."""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from datetime import date, timedelta


@dataclass
class Payer:
    payer_id: str
    legal_name: str
    hios_ids: list[str] = field(default_factory=list)
    states: list[str] = field(default_factory=list)
    landing_url: str = ""
    # A literal URL or a pattern with strftime fields, e.g. ".../{date:%Y-%m}-01_index.json".
    toc_url: str = ""
    # Regex for TOC links on landing_url, for payers whose TOC name changes each month.
    toc_link_pattern: str = ""
    # A TOC saved by hand (repo-relative path) for payers whose site blocks automated access.
    toc_file: str = ""
    kind: str = "issuer"  # issuer | tpa
    aliases: list[str] = field(default_factory=list)
    sources: list[str] = field(default_factory=list)
    notes: str = ""

    def to_dict(self) -> dict:
        return asdict(self)

    @classmethod
    def from_dict(cls, d: dict) -> Payer:
        known = {f for f in cls.__dataclass_fields__}
        return cls(**{k: v for k, v in d.items() if k in known})


@dataclass
class UrlCheck:
    status: int | None = None
    method: str = ""  # HEAD | RANGE_GET
    content_length: int | None = None
    last_modified: str = ""
    etag: str = ""
    error: str = ""


@dataclass
class MrfEntry:
    """One unique MRF URL found in a TOC, with the plans that reference it."""

    url: str
    file_type: str  # in-network | allowed-amount
    descriptions: list[str] = field(default_factory=list)
    plan_names: list[str] = field(default_factory=list)
    plan_id_types: list[str] = field(default_factory=list)
    plan_market_types: list[str] = field(default_factory=list)
    plan_count: int = 0
    plans_truncated: bool = False
    check: UrlCheck = field(default_factory=UrlCheck)


@dataclass
class ValidationReport:
    pinned_version: str
    file_version: str = ""
    valid: bool = True
    error_count: int = 0
    errors: list[str] = field(default_factory=list)
    item_count: int = 0
    invalid_items: int = 0
    # Errors grouped by kind (indices replaced by [*]): {"kind", "count", "examples"}.
    error_kinds: list[dict] = field(default_factory=list)


@dataclass
class PayerResult:
    payer_id: str
    status: str = "pending"  # ok | unchanged | failed | skipped | deferred (retry next run)
    # ok | manual | robots_disallow | robots_unreachable | waf_blocked | http_error | network_error
    # | no_toc_url | too_large | parse_error
    access_status: str = ""
    toc_url: str = ""
    toc_sha256: str = ""
    toc_bytes: int = 0
    toc_last_updated_on: str = ""
    mrf_count: int = 0
    total_bytes: int = 0
    schema_valid: bool | None = None
    validation: ValidationReport | None = None
    error: str = ""
    crawled_at: str = ""
    outputs: list[str] = field(default_factory=list)
    raw_bytes: int = 0

    def to_dict(self) -> dict:
        return asdict(self)


@dataclass
class RegistryRow:
    payer_id: str
    legal_name: str
    hios_ids: str
    landing_url: str
    toc_url: str
    last_crawled: str
    toc_sha256: str
    mrf_count: int
    total_bytes: int
    schema_valid: str
    last_error: str

    COLUMNS = (
        "payer_id",
        "legal_name",
        "hios_ids",
        "landing_url",
        "toc_url",
        "last_crawled",
        "toc_sha256",
        "mrf_count",
        "total_bytes",
        "schema_valid",
        "last_error",
    )

    def values(self) -> list:
        return [getattr(self, c) for c in self.COLUMNS]


def render_toc_urls(pattern: str, today: date) -> list[str]:
    """Expand a TOC URL pattern into candidates: this month first, then last month."""
    if "{" not in pattern:
        return [pattern] if pattern else []
    this_month = today.replace(day=1)
    prev = (this_month - timedelta(days=1)).replace(day=1)
    out: list[str] = []
    for d in (this_month, prev):
        url = pattern.format(date=d)
        if url not in out:
            out.append(url)
    return out

