import json
from datetime import date

import httpx
import respx
from typer.testing import CliRunner

from tests.conftest import FIXTURES, no_sleep
from tests.test_cli import env, mock_site  # noqa: F401
from tic_crawler import cli
from tic_crawler.audit import audit
from tic_crawler.export import toc_plans
from tic_crawler.fetch import HttpClient

runner = CliRunner()


@respx.mock
async def test_audit_verdicts(settings):
    respx.get("https://ok.com/robots.txt").mock(return_value=httpx.Response(
        200, text="User-agent: *\nDisallow: /private\nUser-agent: GPTBot\nDisallow: /\n"))
    respx.get("https://ok.com/toc.json").mock(return_value=httpx.Response(206, content=b"{"))
    respx.get("https://cf.com/robots.txt").mock(return_value=httpx.Response(200, text=""))
    respx.get("https://cf.com/toc").mock(return_value=httpx.Response(
        403, headers={"server": "cloudflare"}, text="<title>Attention Required! | Cloudflare</title>"))
    respx.get("https://down.com/robots.txt").mock(return_value=httpx.Response(503))
    respx.get("https://nf.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://nf.com/x").mock(return_value=httpx.Response(404))
    respx.get("https://err.com/robots.txt").mock(side_effect=httpx.ConnectError("x"))
    targets = [("a", "toc", "https://ok.com/toc.json"), ("a", "landing", "https://ok.com/private/p"),
               ("b", "toc", "https://cf.com/toc"), ("c", "toc", "https://down.com/t"),
               ("d", "toc", "https://nf.com/x"), ("e", "toc", "https://err.com/x"),
               ("f", "toc", "Missing file")]
    async with HttpClient(settings, sleep=no_sleep) as http:
        robots, probes = await audit(http, targets)
    v = {(p.payer_id, p.role): p for p in probes}
    assert v[("a", "toc")].verdict == "accessible"
    assert v[("a", "toc")].robots_allows == {"*": True, "Googlebot": True, "GPTBot": False, "CCBot": True}
    assert v[("a", "landing")].verdict == "robots_blocked" and not v[("a", "landing")].fetched
    assert v[("b", "toc")].verdict == "bot_wall" and v[("b", "toc")].wall == "cloudflare_challenge"
    assert v[("c", "toc")].verdict == "robots_blocked"
    assert v[("d", "toc")].verdict == "http_error"
    assert v[("e", "toc")].verdict == "robots_blocked"
    assert len(probes) == 6
    pol = {r.origin: r.policy for r in robots}
    assert pol == {"https://ok.com": "rules", "https://cf.com": "rules",
                   "https://down.com": "disallow_all", "https://nf.com": "allow_all",
                   "https://err.com": "disallow_all"}
    assert "GPTBot" in [r for r in robots if r.origin == "https://ok.com"][0].text


def test_toc_plans_from_sample():
    res = toc_plans(FIXTURES / "toc" / "cms-sample.json")
    names = {p["plan_name"]: p for p in res["plans"]}
    assert set(names) == {"medicaid", "medicare", "chip"}
    assert names["medicare"]["in_network_files"] == 2
    assert names["chip"]["allowed_amount_files"] == 1
    assert res["plan_refs"] == 3 and not res["truncated"]
    capped = toc_plans(FIXTURES / "toc" / "cms-sample.json", cap=1)
    assert len(capped["plans"]) == 1 and capped["truncated"]


@respx.mock
def test_export_and_audit_cli(env, cms_sample_bytes):  # noqa: F811
    mock_site(cms_sample_bytes)
    respx.get("https://mrf.example.com/").mock(return_value=httpx.Response(200, text="<html>"))
    respx.get("https://www.some_site.com/files/in-network-file-123456.json").mock(
        return_value=httpx.Response(206, content=b"{"))
    respx.get(url__startswith="https://www.some_site.com/files/").mock(
        return_value=httpx.Response(206, content=b"{"))
    assert runner.invoke(cli.app, ["crawl", "--all"]).exit_code == 0
    r = runner.invoke(cli.app, ["audit-access", "--mrf-samples", "1"])
    assert r.exit_code == 0, r.output
    assert "'accessible': 3" in r.output
    r = runner.invoke(cli.app, ["export-dashboard"])
    assert r.exit_code == 0, r.output
    data = json.loads((env / "out/dashboard/data.json").read_text())
    assert data["totals"]["payers"] == 2 and data["totals"]["ok"] == 1
    assert data["totals"]["mrf_bytes"] == 50
    acme = [p for p in data["payers"] if p["payer_id"] == "acme"][0]
    assert acme["url_status"] == {"200": 5}
    assert acme["age_days"] == (date.today() - date(2023, 4, 19)).days and acme["stale"] is True
    assert data["access_audit"]["probes"][0]["verdict"] == "accessible"
    detail = json.loads((env / "out/dashboard/payers/acme.json").read_text())
    assert len(detail["rows"]) == 5
    assert len(detail["toc_plans"]["plans"]) == 3
    nourl = json.loads((env / "out/dashboard/payers/nourl.json").read_text())
    assert nourl["rows"] == []


@respx.mock
def test_probe_cli(env, cms_sample_bytes, tmp_path):  # noqa: F811
    mock_site(cms_sample_bytes)
    respx.get("https://mrf.example.com/").mock(return_value=httpx.Response(200, text="<html>"))
    md = tmp_path / "probe.md"
    r = runner.invoke(cli.app, ["probe", "--toc-url", "https://mrf.example.com/index.json",
                                "--landing-url", "https://mrf.example.com/", "--name", "New Plan Inc",
                                "--markdown", str(md)])
    assert r.exit_code == 0, r.output
    text = md.read_text()
    assert "**TOC:** ok" in text and "5 unique MRF URLs" in text
    assert "payer_id: new-plan" in text
    assert "| toc | https://mrf.example.com/index.json | True | 200 | accessible |" in text
