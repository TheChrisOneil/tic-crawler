import json
from unittest.mock import MagicMock

import httpx
import pytest
import respx
from typer.testing import CliRunner

from tic_crawler import cli
from tic_crawler.config import load_settings
from tic_crawler.state import StateStore

runner = CliRunner()

SEED = """
payers:
  - payer_id: acme
    legal_name: Acme Health Inc.
    hios_ids: [11111]
    landing_url: https://mrf.example.com/
    toc_url: https://mrf.example.com/index.json
  - payer_id: nourl
    legal_name: No URL Health
"""


@pytest.fixture
def env(tmp_path, monkeypatch):
    cfg = tmp_path / "config.toml"
    cfg.write_text(f"""
[http]
requests_per_second_per_host = 0
backoff_base_seconds = 0
backoff_max_seconds = 0
max_retries = 1
[discovery]
seed_file = "{tmp_path / 'payers.yaml'}"
exchange_puf_url = ""
[output]
local_dir = "{tmp_path / 'out'}"
state_db = "{tmp_path / 'state.sqlite'}"
""")
    (tmp_path / "payers.yaml").write_text(SEED)
    monkeypatch.setenv("TIC_CONFIG", str(cfg))
    monkeypatch.setenv("TIC_CONTACT_EMAIL", "t@example.com")
    return tmp_path


def mock_site(cms_sample_bytes):
    respx.get("https://mrf.example.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://www.some_site.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://mrf.example.com/index.json").mock(
        return_value=httpx.Response(200, content=cms_sample_bytes))
    respx.head(url__startswith="https://www.some_site.com/").mock(
        return_value=httpx.Response(200, headers={"Content-Length": "10"}))


@respx.mock
def test_discover_crawl_report(env, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    r = runner.invoke(cli.app, ["discover"])
    assert r.exit_code == 0, r.output
    assert "payers: 2 (with TOC URL: 1)" in r.output
    assert "root_txt: inactive" in r.output

    r = runner.invoke(cli.app, ["crawl", "--all"])
    assert r.exit_code == 0, r.output
    assert "1 crawled now, 1 ok" in r.output
    out = env / "out"
    logs = list((out / "runs").glob("*.json"))
    assert len(logs) == 1
    log = json.loads(logs[0].read_text())
    assert log["summary"]["mrf_urls"] == 5 and log["summary"]["mrf_bytes"] == 50
    reg = (out / "registry.csv").read_text().splitlines()
    assert reg[0].startswith("payer_id,legal_name,hios_ids")
    assert reg[1].startswith("acme,Acme Health Inc.,11111")
    assert len(reg) == 3

    # Within the recrawl window --all skips; --payer crawls and finds it unchanged.
    r = runner.invoke(cli.app, ["crawl", "--all"])
    assert "0 crawled now" in r.output
    r = runner.invoke(cli.app, ["crawl", "--payer", "acme"])
    assert "unchanged" in r.output

    r = runner.invoke(cli.app, ["report"])
    assert r.exit_code == 0
    assert "payers 2, crawled 1, failed 0" in r.output
    r = runner.invoke(cli.app, ["report", "--json"])
    rows = {x["payer_id"]: x for x in json.loads(r.output)}
    assert rows["acme"]["mrf_count"] == 5 and rows["acme"]["stale"] is True
    assert rows["nourl"]["status"] == "never"


def test_crawl_needs_target(env):
    r = runner.invoke(cli.app, ["crawl"])
    assert r.exit_code != 0


def test_crawl_unknown_payer(env):
    r = runner.invoke(cli.app, ["crawl", "--payer", "zzz"])
    assert r.exit_code != 0


@respx.mock
def test_crawl_resumes_unfinished_run(env, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    settings = load_settings()
    state = StateStore(settings.state_db)
    run_id, resumed = state.open_run({"payers": ["acme"], "raw": False, "force": False})
    assert not resumed
    from tic_crawler.models import PayerResult
    state.record_result(run_id, PayerResult("acme", status="ok", crawled_at="2026-01-01T00:00:00Z"))
    r = runner.invoke(cli.app, ["crawl", "--payer", "acme"])
    assert f"resuming run {run_id}: 1 payers already done" in r.output
    assert "0 crawled now" in r.output


def test_sync_drive_requires_credentials(env, monkeypatch):
    monkeypatch.delenv("GOOGLE_APPLICATION_CREDENTIALS", raising=False)
    r = runner.invoke(cli.app, ["sync-drive"])
    assert r.exit_code != 0


@respx.mock
def test_sync_drive_uploads_once_and_updates_sheet(env, cms_sample_bytes, monkeypatch):
    mock_site(cms_sample_bytes)
    runner.invoke(cli.app, ["crawl", "--all"])
    monkeypatch.setenv("GOOGLE_APPLICATION_CREDENTIALS", "/dev/null")
    monkeypatch.setenv("TIC_DRIVE_FOLDER_ID", "ROOT")
    drive_adapter = MagicMock()
    drive_adapter.put_file.side_effect = lambda rel, path: f"remote:{rel}"
    drive_adapter.find_or_create_sheet.return_value = "SHEET"
    sheet = MagicMock()
    sheet.return_value.upsert.return_value = {"updated": 0, "appended": 2}
    monkeypatch.setattr(cli, "build_google_services", lambda p: (MagicMock(), MagicMock()))
    monkeypatch.setattr(cli, "DriveAdapter", lambda svc, fid: drive_adapter)
    monkeypatch.setattr(cli, "SheetRegistry", sheet)
    r = runner.invoke(cli.app, ["sync-drive"])
    assert r.exit_code == 0, r.output
    uploaded = sorted(c.args[0] for c in drive_adapter.put_file.call_args_list)
    assert [u.split("/")[0] for u in uploaded] == ["manifests", "runs", "toc"]
    assert "'uploaded': 3" in r.output
    rows = sheet.return_value.upsert.call_args.args[0]
    assert [x.payer_id for x in rows] == ["acme", "nourl"]
    r = runner.invoke(cli.app, ["sync-drive"])
    assert "'uploaded': 0, 'skipped': 3" in r.output


def test_settings_reject_unknown_keys(tmp_path):
    cfg = tmp_path / "c.toml"
    cfg.write_text("[http]\nbogus = 1\n")
    with pytest.raises(ValueError):
        load_settings(cfg, env={})


def test_settings_from_repo_config():
    s = load_settings("config.toml" if __import__("os").path.exists("config.toml") else None, env={
        "TIC_CONTACT_EMAIL": "a@b.c", "TIC_DRIVE_FOLDER_ID": "F"})
    assert s.user_agent == "EverBetter-TiC-Crawler/1.0 (+a@b.c)"
    assert s.drive_folder_id == "F"
    from datetime import date
    assert s.stale_days(date(2026, 10, 1)) == 35
    assert s.stale_days(date(2027, 3, 6)) == 100


@respx.mock
def test_verify_seeds(env):
    respx.get("https://mrf.example.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.head("https://mrf.example.com/").mock(return_value=httpx.Response(200))
    respx.head("https://mrf.example.com/index.json").mock(return_value=httpx.Response(404))
    r = runner.invoke(cli.app, ["verify-seeds"])
    assert r.exit_code == 0, r.output
    assert "checked 2 urls, 1 ok" in r.output
    text = (env / "out" / "seed_verification.csv").read_text()
    assert "http_404" in text


@respx.mock
def test_crawl_with_manual_toc_file(env, cms_sample_bytes):
    mock_site(cms_sample_bytes)
    f = env / "manual.json"
    f.write_bytes(cms_sample_bytes)
    assert runner.invoke(cli.app, ["crawl", "--all", "--toc-file", str(f)]).exit_code != 0
    r = runner.invoke(cli.app, ["crawl", "--payer", "nourl", "--toc-file", str(f)])
    assert r.exit_code == 0, r.output
    assert "1 ok" in r.output
