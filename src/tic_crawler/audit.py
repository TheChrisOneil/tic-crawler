"""Access audit: what a polite automated client sees on each payer host.

For every origin it records the robots.txt response and rules, then probes each URL of
interest once (only when robots.txt allows it) and looks for bot walls (Cloudflare, Akamai,
CAPTCHA pages). Results feed the dashboard and the robots/accessibility assessment.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import asdict, dataclass, field
from urllib.parse import urlsplit
from urllib.robotparser import RobotFileParser

import httpx

from .fetch import HttpClient, detect_wall

ROBOTS_TEXT_CAP = 8000
BODY_SNIFF_BYTES = 4096
OTHER_AGENTS = ("*", "Googlebot", "GPTBot", "CCBot")


@dataclass
class RobotsResult:
    origin: str
    status: int | None = None
    error: str = ""
    server: str = ""
    policy: str = ""  # allow_all (4xx) | disallow_all (5xx/unreachable) | rules
    text: str = ""
    wall: str = ""


@dataclass
class UrlProbe:
    payer_id: str
    role: str  # landing | toc | mrf_sample
    url: str
    robots_allows_us: bool | None = None
    robots_allows: dict[str, bool] = field(default_factory=dict)
    fetched: bool = False
    status: int | None = None
    server: str = ""
    content_type: str = ""
    wall: str = ""
    error: str = ""
    verdict: str = ""  # accessible | robots_blocked | bot_wall | http_error | unreachable


async def _get_sniff(client: httpx.AsyncClient, url: str, headers: dict | None = None
                     ) -> tuple[httpx.Response, bytes]:
    async with client.stream("GET", url, headers=headers) as resp:
        body = b""
        async for chunk in resp.aiter_bytes():
            body += chunk
            if len(body) >= BODY_SNIFF_BYTES:
                break
        return resp, body


async def audit(http: HttpClient, targets: Iterable[tuple[str, str, str]]
                ) -> tuple[list[RobotsResult], list[UrlProbe]]:
    """targets: (payer_id, role, url). Each origin's robots.txt is fetched once."""
    targets = [t for t in targets if t[2].lower().startswith(("https://", "http://"))]
    robots: dict[str, RobotsResult] = {}
    parsers: dict[str, RobotFileParser | None] = {}
    client = http._client
    for _, _, url in targets:
        p = urlsplit(url)
        origin = f"{p.scheme}://{p.netloc}"
        if origin in robots:
            continue
        rr = RobotsResult(origin=origin)
        await http.limiter.wait(p.netloc.lower())
        try:
            resp, body = await _get_sniff(client, f"{origin}/robots.txt")
            rr.status = resp.status_code
            rr.server = resp.headers.get("server", "")
            rr.wall = detect_wall(resp, body)
            if resp.status_code < 400 and not rr.wall:
                full = await client.get(f"{origin}/robots.txt")
                rr.text = full.text[:ROBOTS_TEXT_CAP]
                rp = RobotFileParser()
                rp.parse(full.text.splitlines())
                parsers[origin] = rp
                rr.policy = "rules"
            elif resp.status_code >= 500:
                parsers[origin] = None
                rr.policy = "disallow_all"
            else:
                rp = RobotFileParser()
                rp.parse([])
                parsers[origin] = rp
                rr.policy = "allow_all"
        except httpx.HTTPError as e:
            rr.error = f"{type(e).__name__}: {e}"[:200]
            rr.policy = "disallow_all"
            parsers[origin] = None
        robots[origin] = rr

    probes: list[UrlProbe] = []
    for payer_id, role, url in targets:
        p = urlsplit(url)
        origin = f"{p.scheme}://{p.netloc}"
        rp = parsers.get(origin)
        probe = UrlProbe(payer_id=payer_id, role=role, url=url)
        if rp is None:
            probe.robots_allows_us = False
        else:
            probe.robots_allows_us = rp.can_fetch(http.settings.user_agent, url)
            probe.robots_allows = {a: rp.can_fetch(a, url) for a in OTHER_AGENTS}
        if not probe.robots_allows_us:
            probe.verdict = "robots_blocked"
            probes.append(probe)
            continue
        await http.limiter.wait(p.netloc.lower())
        headers = {"Range": "bytes=0-4095"} if role != "landing" else None
        try:
            resp, body = await _get_sniff(client, url, headers)
            probe.fetched = True
            probe.status = resp.status_code
            probe.server = resp.headers.get("server", "")
            probe.content_type = resp.headers.get("content-type", "")[:80]
            probe.wall = detect_wall(resp, body)
            if probe.wall:
                probe.verdict = "bot_wall"
            elif resp.status_code < 400:
                probe.verdict = "accessible"
            else:
                probe.verdict = "http_error"
        except httpx.HTTPError as e:
            probe.error = f"{type(e).__name__}: {e}"[:200]
            probe.verdict = "unreachable"
        probes.append(probe)
    return list(robots.values()), probes


def to_dict(robots: list[RobotsResult], probes: list[UrlProbe]) -> dict:
    return {"robots": [asdict(r) for r in robots], "probes": [asdict(p) for p in probes]}
