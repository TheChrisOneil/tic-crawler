"""Crawl pipeline (Template Method).

CrawlPipeline.run() fixes the order of steps; subclasses override hooks.
TocCrawlPipeline is the v1 behaviour (HEAD only). RawDownloadPipeline adds raw downloads.
"""

from __future__ import annotations

import asyncio
import csv
import hashlib
import re
import tempfile
from dataclasses import dataclass, field
from datetime import UTC, date, datetime
from html import unescape
from pathlib import Path
from urllib.parse import urljoin

from .config import Settings
from .fetch import FetchError, HttpClient, RobotsDisallowed
from .models import MrfEntry, Payer, PayerResult, UrlCheck, ValidationReport, render_toc_urls
from .parse import MrfCollector, iter_toc, open_json, sha256_of_json, write_gzip_copy
from .state import StateStore
from .storage import RawStore
from .validate import TocValidator

MANIFEST_COLUMNS = (
    "payer_id", "toc_url", "toc_sha256", "toc_last_updated_on", "toc_file_version",
    "pinned_schema_version", "toc_schema_valid", "toc_validation_error_count",
    "toc_validation_errors", "file_type", "url", "description", "plan_count", "plan_names",
    "plans_truncated", "plan_id_types", "plan_market_types", "http_status", "check_method",
    "content_length", "last_modified", "etag", "check_error",
)


class SkipPayer(Exception):
    """Raised by a step to end the run for a payer without marking it failed."""


class TooLarge(Exception):
    pass


@dataclass
class CrawlContext:
    payer: Payer
    run_date: date
    work_dir: Path
    result: PayerResult
    toc_path: Path | None = None
    header: dict = field(default_factory=dict)
    validation: ValidationReport | None = None
    collector: MrfCollector | None = None
    unchanged: bool = False


class CrawlPipeline:
    """Template method: subclasses fill in the steps, run() owns the order."""

    def __init__(self, settings: Settings, http: HttpClient, state: StateStore, out_dir: Path,
                 force: bool = False, today: date | None = None):
        self.settings = settings
        self.http = http
        self.state = state
        self.out_dir = Path(out_dir)
        self.force = force
        self.today = today or datetime.now(UTC).date()

    async def run(self, payer: Payer) -> PayerResult:
        result = PayerResult(payer_id=payer.payer_id, crawled_at=_now())
        with tempfile.TemporaryDirectory(prefix="tic-") as tmp:
            ctx = CrawlContext(payer=payer, run_date=self.today, work_dir=Path(tmp), result=result)
            try:
                await self.fetch_toc(ctx)
                self.parse_and_validate(ctx)
                if self.is_unchanged(ctx):
                    ctx.unchanged = True
                    result.status = "unchanged"
                    return result
                await self.check_mrfs(ctx)
                await self.after_checks(ctx)
                self.write_outputs(ctx)
                result.status = "ok"
            except SkipPayer as e:
                result.status = "skipped"
                result.error = str(e)
            except Exception as e:
                result.status = "failed"
                result.error = f"{type(e).__name__}: {e}"[:500]
        return result

    async def fetch_toc(self, ctx: CrawlContext) -> None:
        raise NotImplementedError

    def parse_and_validate(self, ctx: CrawlContext) -> None:
        raise NotImplementedError

    def is_unchanged(self, ctx: CrawlContext) -> bool:
        return False

    async def check_mrfs(self, ctx: CrawlContext) -> None:
        raise NotImplementedError

    async def after_checks(self, ctx: CrawlContext) -> None:
        """Hook: runs after MRF checks, before outputs are written. No-op by default."""

    def write_outputs(self, ctx: CrawlContext) -> None:
        raise NotImplementedError


class TocCrawlPipeline(CrawlPipeline):
    async def fetch_toc(self, ctx: CrawlContext) -> None:
        candidates = render_toc_urls(ctx.payer.toc_url, self.today)
        if not candidates and ctx.payer.toc_link_pattern and ctx.payer.landing_url:
            candidates = await self.scrape_toc_links(ctx.payer)
        if not candidates:
            raise SkipPayer("no_toc_url")
        errors: list[str] = []
        for url in candidates:
            path = ctx.work_dir / "toc.raw"
            try:
                size = 0
                async with self.http.stream_get(url) as resp:
                    declared = resp.headers.get("content-length", "")
                    if declared.isdigit() and int(declared) > self.settings.max_toc_bytes:
                        raise TooLarge(f"TOC is {declared} bytes, over max_toc_bytes")
                    with path.open("wb") as fh:
                        async for chunk in resp.aiter_bytes():
                            size += len(chunk)
                            if size > self.settings.max_toc_bytes:
                                raise TooLarge("TOC exceeded max_toc_bytes while streaming")
                            fh.write(chunk)
            except RobotsDisallowed as e:
                errors.append(f"robots_disallowed {e}")
                continue
            except FetchError as e:
                errors.append(str(e))
                continue
            except TooLarge as e:
                raise SkipPayer(f"toc_too_large: {e}") from e
            ctx.toc_path = path
            ctx.result.toc_url = url
            ctx.result.toc_bytes = size
            return
        raise FetchError(candidates[0], None, "; ".join(errors)[:400])

    async def scrape_toc_links(self, payer: Payer) -> list[str]:
        """Find TOC links on the landing page. Newest first, assuming dated names sort."""
        html = await self.http.get_text(payer.landing_url)
        found = {urljoin(payer.landing_url, unescape(m.group(0)))
                 for m in re.finditer(payer.toc_link_pattern, html)}
        return sorted(found, reverse=True)[:3]

    def parse_and_validate(self, ctx: CrawlContext) -> None:
        assert ctx.toc_path is not None
        ctx.result.toc_sha256 = sha256_of_json(ctx.toc_path)
        validator = TocValidator(self.settings.schema_version, self.settings.max_errors_per_toc)
        collector = MrfCollector(name_cap=self.settings.manifest_plan_name_cap)
        try:
            with open_json(ctx.toc_path) as fh:
                for ev in iter_toc(fh):
                    if ev.kind == "item":
                        validator.validate_item(ev.index, ev.value)
                        collector.add_item(ev.value)
                    else:
                        ctx.header = ev.value
                        validator.validate_header(ev.value)
        except Exception as e:  # malformed JSON is a validation result, not a crash
            validator._add(f"$: JSON parse error: {type(e).__name__}: {e}")
        ctx.validation = validator.finish()
        ctx.collector = collector
        ctx.result.validation = ctx.validation
        ctx.result.schema_valid = ctx.validation.valid
        ctx.result.mrf_count = len(collector)
        lu = ctx.header.get("last_updated_on")
        ctx.result.toc_last_updated_on = lu if isinstance(lu, str) else ""

    def is_unchanged(self, ctx: CrawlContext) -> bool:
        if self.force:
            return False
        return self.state.latest_sha(ctx.payer.payer_id) == ctx.result.toc_sha256

    async def check_mrfs(self, ctx: CrawlContext) -> None:
        assert ctx.collector is not None
        entries = list(ctx.collector.entries.values())

        async def check(entry: MrfEntry) -> None:
            if not entry.url.lower().startswith("https://"):
                # Placeholders like "Missing file" appear in real TOCs; never request them.
                entry.check = UrlCheck(error="invalid_url")
                return
            entry.check = await self.http.head_or_range(entry.url)

        await asyncio.gather(*(check(e) for e in entries))
        ctx.result.total_bytes = sum(e.check.content_length or 0 for e in entries)

    def write_outputs(self, ctx: CrawlContext) -> None:
        assert ctx.toc_path is not None and ctx.collector is not None and ctx.validation
        pid = ctx.payer.payer_id
        day = ctx.run_date.isoformat()
        toc_rel = f"toc/{pid}/{day}.json.gz"
        write_gzip_copy(ctx.toc_path, self.out_dir / toc_rel)
        man_rel = f"manifests/{pid}/{day}.csv"
        self._write_manifest(ctx, self.out_dir / man_rel)
        ctx.result.outputs = [toc_rel, man_rel]

    def _write_manifest(self, ctx: CrawlContext, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        v = ctx.validation
        base = {
            "payer_id": ctx.payer.payer_id,
            "toc_url": ctx.result.toc_url,
            "toc_sha256": ctx.result.toc_sha256,
            "toc_last_updated_on": ctx.header.get("last_updated_on", ""),
            "toc_file_version": v.file_version,
            "pinned_schema_version": v.pinned_version,
            "toc_schema_valid": v.valid,
            "toc_validation_error_count": v.error_count,
            "toc_validation_errors": " | ".join(v.errors[:5]),
        }
        entries = sorted(ctx.collector.entries.values(), key=lambda e: (e.file_type, e.url))
        with path.open("w", newline="", encoding="utf-8") as fh:
            w = csv.DictWriter(fh, fieldnames=MANIFEST_COLUMNS)
            w.writeheader()
            if not entries:
                w.writerow(base)
            for e in entries:
                c = e.check
                w.writerow({
                    **base,
                    "file_type": e.file_type,
                    "url": e.url,
                    "description": " | ".join(e.descriptions),
                    "plan_count": e.plan_count,
                    "plan_names": " | ".join(e.plan_names),
                    "plans_truncated": e.plans_truncated,
                    "plan_id_types": "|".join(e.plan_id_types),
                    "plan_market_types": "|".join(e.plan_market_types),
                    "http_status": c.status if c.status is not None else "",
                    "check_method": c.method,
                    "content_length": c.content_length if c.content_length is not None else "",
                    "last_modified": c.last_modified,
                    "etag": c.etag,
                    "check_error": c.error,
                })


class RawDownloadPipeline(TocCrawlPipeline):
    """Adds --download-raw: stream each MRF to the raw store (GCS or local, never Drive)."""

    def __init__(self, *args, raw_store: RawStore, max_run_bytes: int, **kw):
        super().__init__(*args, **kw)
        self.raw_store = raw_store
        self.max_run_bytes = max_run_bytes
        self.bytes_used = 0

    async def after_checks(self, ctx: CrawlContext) -> None:
        estimate = ctx.result.total_bytes
        if self.bytes_used + estimate > self.max_run_bytes:
            raise SkipPayer(
                f"raw_download_over_cap: needs {estimate} bytes, "
                f"{self.max_run_bytes - self.bytes_used} left in this run"
            )
        for entry in ctx.collector.entries.values():
            if entry.check.error:
                continue
            name = hashlib.sha1(entry.url.encode()).hexdigest()[:16]
            rel = f"raw/{ctx.payer.payer_id}/{ctx.run_date.isoformat()}/{name}"
            async with self.http.stream_get(entry.url) as resp:
                writer = self.raw_store.open_writer(rel)
                try:
                    async for chunk in resp.aiter_bytes():
                        self.bytes_used += len(chunk)
                        ctx.result.raw_bytes += len(chunk)
                        if self.bytes_used > self.max_run_bytes:
                            raise SkipPayer("raw_download_over_cap while streaming")
                        writer.write(chunk)
                finally:
                    writer.close()


def _now() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


class ProbePipeline(TocCrawlPipeline):
    """Fetch, parse and validate a TOC without checking MRF URLs (for payer requests)."""

    def is_unchanged(self, ctx: CrawlContext) -> bool:
        return False

    async def check_mrfs(self, ctx: CrawlContext) -> None:
        return None
