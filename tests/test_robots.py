import pytest

from tic_crawler.robots import RobotsRules, product_token

CENTENE = """User-agent: *
Disallow: /page/
Disallow: /?
Disallow: /pay stubs
Noindex: /investor-day.html
Allow: /

Sitemap: https://www.centene.com/.sitemap.xml
"""
UA = "EverBetter-TiC-Crawler/1.0 (+x@y.com)"


def test_product_token():
    assert product_token(UA) == "everbetter-tic-crawler"


def test_centene_query_rule_does_not_block_everything():
    r = RobotsRules.parse(CENTENE)
    assert r.can_fetch(UA, "https://www.centene.com/price-transparency-files.html")
    assert not r.can_fetch(UA, "https://www.centene.com/?q=1")
    assert not r.can_fetch(UA, "https://www.centene.com/page/x")
    assert not r.can_fetch(UA, "https://www.centene.com/pay%20stubs/a")


@pytest.mark.parametrize("text,url,expected", [
    ("User-agent: *\nDisallow: /a\nAllow: /a/b\n", "https://h/a/b/c", True),     # longest wins
    ("User-agent: *\nAllow: /a\nDisallow: /a/b\n", "https://h/a/b/c", False),
    ("User-agent: *\nDisallow: /x\nAllow: /x\n", "https://h/x", True),           # tie -> allow
    ("User-agent: *\nDisallow: /*.json$\n", "https://h/f.json", False),
    ("User-agent: *\nDisallow: /*.json$\n", "https://h/f.json.gz", True),
    ("User-agent: *\nDisallow: /tmp*/x\n", "https://h/tmp123/x", False),
    ("User-agent: *\nDisallow:\n", "https://h/anything", True),
    ("User-agent: *\nDisallow: /\n", "https://h/robots.txt", True),
    ("User-agent: other\nDisallow: /\n", "https://h/a", True),                  # no group for us
    ("User-agent: everbetter-tic-crawler\nDisallow: /\nUser-agent: *\nAllow: /\n", "https://h/a", False),
    ("User-agent: a\nUser-agent: *\nDisallow: /p\n", "https://h/p", False),     # multi-agent group
    ("Disallow: /\n", "https://h/a", True),                                     # rule outside group
    ("User-agent: *\nDisallow: /%7Euser\n", "https://h/~user/x", False),
])
def test_matching(text, url, expected):
    assert RobotsRules.parse(text).can_fetch(UA, url) is expected


def test_allow_all():
    assert RobotsRules(allow_all=True).can_fetch(UA, "https://h/x")
