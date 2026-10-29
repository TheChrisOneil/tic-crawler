import csv
import gzip
from datetime import date

import httpx
import pytest
import respx

from tests.conftest import no_sleep
from tic_crawler.fetch import HttpClient
from tic_crawler.models import Payer, render_toc_urls
from tic_crawler.pipeline import RawDownloadPipeline, TocCrawlPipeline
from tic_crawler.state import StateStore
from tic_crawler.storage import LocalRawStore

TODAY = date(2026, 10, 29)
SITE = "https://www.some_site.com"
TOC = "https://mrf.example.com/2026-10-01_index.json"


def mock_site(cms_sample_bytes, toc_body=None, gz=False):
    respx.get("https://mrf.example.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.get(f"{SITE}/robots.txt").mock(return_value=httpx.Response(404))
    body = toc_body if toc_body is not None else cms_sample_bytes
    if gz:
        body = gzip.compress(body)
    respx.get(TOC).mock(return_value=httpx.Response(200, content=body))
    respx.head(url__startswith=f"{SITE}/files/").mock(
        return_value=httpx.Response(200, headers={"Content-Length": "1000", "ETag": '"e"',
                                                  "Last-Modified": "Wed, 01 Oct 2026 00:00:00 GMT"}))


def payer(toc=TOC):
    return Payer("acme", "Acme Health", toc_url=toc)


@pytest.fixture
def state(settings):
    s = StateStore(settings.state_db)
    yield s
    s.close()


def test_render_toc_urls():
    p = "https://x/{date:%Y-%m}-01_index.json"
    assert render_toc_urls(p, date(2026, 1, 15)) == [
        "https://x/2026-01-01_index.json", "https://x/2025-12-01_index.json"]
    assert render_toc_urls("https://x/a.json", TODAY) == ["https://x/a.json"]
    assert render_toc_urls("", TODAY) == []


@respx.mock
@pytest.mark.parametrize("gz", [False, True])
async def test_crawl_writes_toc_and_manifest(settings, state, tmp_path, cms_sample_bytes, gz):
    mock_site(cms_sample_bytes, gz=gz)
    out = tmp_path / "out"
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, out, today=TODAY).run(payer())
    assert r.status == "ok", r.error
    assert r.schema_valid is True
    assert r.mrf_count == 5
    assert r.total_bytes == 5000
    assert r.toc_last_updated_on == "2023-04-19"
    toc = out / "toc/acme/2026-10-29.json.gz"
    assert gzip.decompress(toc.read_bytes()) == cms_sample_bytes
    rows = list(csv.DictReader((out / "manifests/acme/2026-10-29.csv").open()))
    assert len(rows) == 5
    row = next(x for x in rows if x["url"].endswith("in-network-file-123456.json"))
    assert row["plan_count"] == "2"
    assert row["plan_names"] == "medicaid | medicare"
    assert row["http_status"] == "200" and row["check_method"] == "HEAD"
    assert row["content_length"] == "1000"
    assert row["toc_file_version"] == "2.0.0" and row["pinned_schema_version"] == "2.2.1"
    assert row["toc_sha256"] == r.toc_sha256
    assert {x["file_type"] for x in rows} == {"in-network", "allowed-amount"}


@respx.mock
async def test_second_run_unchanged_skips(settings, state, tmp_path, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    out = tmp_path / "out"
    async with HttpClient(settings, sleep=no_sleep) as http:
        pipe = TocCrawlPipeline(settings, http, state, out, today=TODAY)
        r1 = await pipe.run(payer())
        state.record_result("run1", r1)
        r2 = await pipe.run(payer())
        assert r2.status == "unchanged"
        assert r2.toc_sha256 == r1.toc_sha256
        state.record_result("run2", r2)
        latest = state.latest("acme")
        assert latest["mrf_count"] == 5 and latest["status"] == "unchanged"
        forced = await TocCrawlPipeline(settings, http, state, out, force=True, today=TODAY).run(payer())
        assert forced.status == "ok"


@respx.mock
async def test_invalid_toc_still_cataloged(settings, state, tmp_path):
    body = b'{"reporting_structure": [{"reporting_plans": [], "in_network_files": ' \
           b'[{"description": "x", "location": "https://www.some_site.com/files/a.json"}]}]}'
    mock_site(b"", toc_body=body)
    out = tmp_path / "out"
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, out, today=TODAY).run(payer())
    assert r.status == "ok" and r.schema_valid is False
    assert r.validation.error_count >= 4
    rows = list(csv.DictReader((out / "manifests/acme/2026-10-29.csv").open()))
    assert rows[0]["toc_schema_valid"] == "False"
    assert "required property" in rows[0]["toc_validation_errors"]


@respx.mock
async def test_malformed_json_recorded(settings, state, tmp_path):
    mock_site(b"", toc_body=b'{"reporting_structure": [ {"a": ')
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, tmp_path / "o", today=TODAY).run(payer())
    assert r.status == "ok" and r.schema_valid is False
    assert any("JSON parse error" in e for e in r.validation.errors)
    rows = list(csv.DictReader((tmp_path / "o/manifests/acme/2026-10-29.csv").open()))
    assert len(rows) == 1 and rows[0]["url"] == ""


async def test_no_toc_url_skipped(settings, state, tmp_path):
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, tmp_path, today=TODAY).run(payer(""))
    assert r.status == "skipped" and r.error == "no_toc_url"


@respx.mock
async def test_pattern_falls_back_to_previous_month(settings, state, tmp_path, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    respx.get("https://mrf.example.com/2026-10-01_x.json").mock(return_value=httpx.Response(404))
    respx.get("https://mrf.example.com/2026-09-01_x.json").mock(
        return_value=httpx.Response(200, content=cms_sample_bytes))
    p = payer("https://mrf.example.com/{date:%Y-%m}-01_x.json")
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, tmp_path, today=TODAY).run(p)
    assert r.status == "ok"
    assert r.toc_url.endswith("2026-09-01_x.json")


@respx.mock
async def test_fetch_failure_marks_failed(settings, state, tmp_path):
    respx.get("https://mrf.example.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.get(TOC).mock(return_value=httpx.Response(404))
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, tmp_path, today=TODAY).run(payer())
    assert r.status == "failed" and "404" in r.error


@respx.mock
async def test_robots_blocked_toc(settings, state, tmp_path):
    respx.get("https://mrf.example.com/robots.txt").mock(
        return_value=httpx.Response(200, text="User-agent: *\nDisallow: /\n"))
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, tmp_path, today=TODAY).run(payer())
    assert r.status == "failed" and "robots_disallowed" in r.error


@respx.mock
async def test_toc_too_large_skipped(settings, state, tmp_path, cms_sample_bytes):
    from dataclasses import replace

    mock_site(cms_sample_bytes)
    small = replace(settings, max_toc_bytes=100)
    async with HttpClient(small, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(small, http, state, tmp_path, today=TODAY).run(payer())
    assert r.status == "skipped" and r.error.startswith("toc_too_large")


@respx.mock
async def test_raw_download_to_local_store(settings, state, tmp_path, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    respx.get(url__startswith=f"{SITE}/files/").mock(return_value=httpx.Response(200, content=b"x" * 10))
    raw = LocalRawStore(tmp_path / "raw")
    async with HttpClient(settings, sleep=no_sleep) as http:
        pipe = RawDownloadPipeline(settings, http, state, tmp_path / "out", today=TODAY,
                                   raw_store=raw, max_run_bytes=10_000)
        r = await pipe.run(payer())
    assert r.status == "ok", r.error
    assert r.raw_bytes == 50
    assert len(list((tmp_path / "raw/raw/acme/2026-10-29").iterdir())) == 5


@respx.mock
async def test_raw_download_refuses_over_cap(settings, state, tmp_path, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    raw = LocalRawStore(tmp_path / "raw")
    async with HttpClient(settings, sleep=no_sleep) as http:
        pipe = RawDownloadPipeline(settings, http, state, tmp_path / "out", today=TODAY,
                                   raw_store=raw, max_run_bytes=4000)
        r = await pipe.run(payer())
    assert r.status == "skipped" and r.error.startswith("raw_download_over_cap")
    assert not (tmp_path / "raw").exists()


@respx.mock
async def test_toc_link_pattern_scrapes_landing_page(settings, state, tmp_path, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    html = ('<a href="/json/2026-08-28_x_index.json">old</a>'
            '<a href="https://mrf.example.com/json/2026-09-28_x_index.json">new</a>')
    respx.get("https://mrf.example.com/tic").mock(return_value=httpx.Response(200, text=html))
    respx.get("https://mrf.example.com/json/2026-09-28_x_index.json").mock(
        return_value=httpx.Response(200, content=cms_sample_bytes))
    p = Payer("acme", "Acme", landing_url="https://mrf.example.com/tic",
              toc_link_pattern=r"[^\"']*\d{4}-\d{2}-\d{2}_x_index\.json")
    async with HttpClient(settings, sleep=no_sleep) as http:
        r = await TocCrawlPipeline(settings, http, state, tmp_path, today=TODAY).run(p)
    assert r.status == "ok", r.error
    assert r.toc_url == "https://mrf.example.com/json/2026-09-28_x_index.json"
