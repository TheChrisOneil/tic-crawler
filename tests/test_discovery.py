import io
import zipfile
from datetime import date

import httpx
import pytest
import respx

from tests.conftest import no_sleep
from tic_crawler.discovery import (
    DiscoveryRegistry,
    DiscoverySource,
    ExchangePufSource,
    RootTxtSource,
    SeedFileSource,
    dedupe_payers,
    normalize_name,
    parse_root_txt,
    slugify,
)
from tic_crawler.fetch import HttpClient
from tic_crawler.models import Payer

PUF_CSV = (
    "State,Issuer ID,Issuer Name,URL Submitted\n"
    "TX,12345,Blue Cross and Blue Shield of Texas,https://a\n"
    "TX,12345,Blue Cross and Blue Shield of Texas,https://a\n"
    "OK,67890,\"Aetna Life Insurance Company\",https://b\n"
    "FL,67891,Aetna Life Insurance Co.,https://b\n"
    ",,,\n"
)

SEED = """
payers:
  - payer_id: aetna
    legal_name: Aetna Inc.
    aliases: [Aetna Life Insurance Company]
    landing_url: https://www.aetna.com/tic
    toc_url: https://mrf.aetna.com/{date:%Y-%m}-01_toc.json
  - legal_name: The Example Health Plan, LLC
    hios_ids: [12345]
    kind: tpa
"""


@pytest.mark.parametrize("raw,norm", [
    ("Aetna Life Insurance Company", "aetna life insurance"),
    ("Aetna Life Insurance Co.", "aetna life insurance"),
    ("The Cigna Group, Inc.", "cigna group"),
    ("Blue Cross & Blue Shield of Texas", "blue cross and blue shield of texas"),
    ("UnitedHealthcare Insurance Company, LLC", "unitedhealthcare insurance"),
])
def test_normalize_name(raw, norm):
    assert normalize_name(raw) == norm


def test_slugify():
    assert slugify("Blue Cross & Blue Shield of Texas") == "blue-cross-and-blue-shield-of-texas"
    assert slugify("!!!") == "payer"


async def test_seed_source(settings, tmp_path):
    p = tmp_path / "seed.yaml"
    p.write_text(SEED)
    payers = await SeedFileSource(settings, p).discover(None, [])
    assert [x.payer_id for x in payers] == ["aetna", "example-health-plan"]
    assert payers[1].hios_ids == ["12345"] and payers[1].kind == "tpa"


@respx.mock
async def test_exchange_puf_csv_and_zip(settings):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("readme.txt", "x")
        zf.writestr("puf.csv", PUF_CSV)
    respx.get("https://puf/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://puf/p.zip").mock(return_value=httpx.Response(200, content=buf.getvalue()))
    respx.get("https://puf/p.csv").mock(return_value=httpx.Response(200, text=PUF_CSV))
    async with HttpClient(settings, sleep=no_sleep) as http:
        for url in ("https://puf/p.zip", "https://puf/p.csv"):
            payers = await ExchangePufSource(settings, url).discover(http, [])
            by_id = {p.payer_id: p for p in payers}
            assert set(by_id) == {"hios-12345", "hios-67890", "hios-67891"}
            assert by_id["hios-12345"].states == ["TX"]
            # The two Aetna HIOS IDs merge on normalized name.
            merged = dedupe_payers(payers)
            assert len(merged) == 2
            aetna = [m for m in merged if m.hios_ids == ["67890", "67891"]][0]
            assert aetna.states == ["FL", "OK"]


async def test_exchange_puf_disabled(settings):
    assert await ExchangePufSource(settings, "").discover(None, []) == []


def test_dedupe_seed_wins_and_merges_by_alias_and_hios():
    seed = Payer("aetna", "Aetna Inc.", aliases=["Aetna Life Insurance Company"],
                 toc_url="https://t", sources=["seed"])
    seed2 = Payer("ex", "Example Plan", hios_ids=["12345"], sources=["seed"])
    puf = [
        Payer("hios-67890", "Aetna Life Insurance Co", hios_ids=["67890"], states=["OK"],
              landing_url="https://l", sources=["exchange_puf"]),
        Payer("hios-12345", "Different Legal Name Inc", hios_ids=["12345"], states=["TX"],
              sources=["exchange_puf"]),
    ]
    out = dedupe_payers([seed, seed2, *puf])
    assert [p.payer_id for p in out] == ["aetna", "ex"]
    assert out[0].hios_ids == ["67890"] and out[0].toc_url == "https://t"
    assert out[0].landing_url == "https://l"
    assert out[0].sources == ["seed", "exchange_puf"]
    assert "Different Legal Name Inc" in out[1].aliases


def test_root_txt_parse():
    tocs, contact = parse_root_txt("toc: https://a/b.json\ncontact: mrf@payer.com\n")
    assert tocs == ["https://a/b.json"] and contact == "mrf@payer.com"


@respx.mock
async def test_root_txt_inactive_until_2027(settings):
    src = RootTxtSource(settings)
    assert not src.is_active(date(2027, 9, 5))
    assert src.is_active(date(2027, 9, 6))
    respx.get("https://p.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://p.com/price-transparency.txt").mock(
        return_value=httpx.Response(200, text="https://p.com/toc.json\nmrf@p.com"))
    respx.get("https://q.com/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://q.com/price-transparency.txt").mock(return_value=httpx.Response(404))
    known = [Payer("p", "P", landing_url="https://p.com/tic"),
             Payer("q", "Q", landing_url="https://q.com/x"), Payer("r", "R")]
    async with HttpClient(settings, sleep=no_sleep) as http:
        found = await src.discover(http, known)
    assert [(f.payer_id, f.toc_url) for f in found] == [("p", "https://p.com/toc.json")]


async def test_registry_auto_registers_and_isolates_failures(settings, tmp_path):
    (tmp_path / "payers.yaml").write_text(SEED)

    class Broken(DiscoverySource):
        name = "broken_test_source"
        order = 50

        async def discover(self, http, known):
            raise RuntimeError("down")

    try:
        assert "broken_test_source" in DiscoverySource.registry
        reg = DiscoveryRegistry(settings)
        payers, status = await reg.run(None, date(2026, 10, 29))
        assert status["seed"] == "ok:2"
        assert status["broken_test_source"].startswith("error:RuntimeError")
        assert status["root_txt"] == "inactive"
        assert status["exchange_puf"] == "ok:0"
        assert len(payers) == 2
        payers, status = await reg.run(None, date(2026, 10, 29), only=["seed"])
        assert list(status) == ["seed"]
    finally:
        DiscoverySource.registry.pop("broken_test_source", None)
