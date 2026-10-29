"""Polite async HTTP: per-host rate limit, robots.txt, retries with backoff."""

from __future__ import annotations

import asyncio
import email.utils
import random
import re
import time
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from urllib.parse import urlsplit
from urllib.robotparser import RobotFileParser

import httpx

from .config import Settings
from .models import UrlCheck

RETRY_STATUSES = {408, 425, 429, 500, 502, 503, 504}


class RobotsDisallowed(Exception):
    def __init__(self, message: str, unreachable: bool = False):
        super().__init__(message)
        # True when robots.txt could not be read (5xx, 429, network), as opposed to a real Disallow.
        self.unreachable = unreachable


class FetchError(Exception):
    def __init__(self, url: str, status: int | None, message: str = "", wall: str = ""):
        super().__init__(f"{status or 'error'} {url}" + (f" ({message})" if message else ""))
        self.url = url
        self.status = status
        self.wall = wall


WALL_MARKERS = (
    ("cloudflare_challenge",
     re.compile(r"attention required!? \| cloudflare|just a moment\.\.\.|cf-chl", re.I)),
    ("akamai_block", re.compile(r"access denied.*reference #|errors\.edgesuite\.net", re.I | re.S)),
    ("captcha", re.compile(r"captcha|are you a robot|verify you are human", re.I)),
    ("incapsula", re.compile(r"incapsula|_incap_", re.I)),
)


def detect_wall(resp: httpx.Response, body: bytes) -> str:
    """Name the bot wall (WAF challenge or block page) a response shows, or ''."""
    if resp.headers.get("cf-mitigated", "").lower() == "challenge":
        return "cloudflare_challenge"
    text = body[:4096].decode("utf-8", "replace")
    for name, rx in WALL_MARKERS:
        if rx.search(text):
            return name
    if resp.status_code in (403, 429) and "cloudflare" in resp.headers.get("server", "").lower():
        return "cloudflare_block"
    return ""


class HostRateLimiter:
    """Spaces requests to the same host by at least 1/rate seconds."""

    def __init__(self, rate: float, clock: Callable[[], float] = time.monotonic,
                 sleep: Callable[[float], Awaitable[None]] = asyncio.sleep):
        self.interval = 0.0 if rate <= 0 else 1.0 / rate
        self._next: dict[str, float] = {}
        self._locks: dict[str, asyncio.Lock] = {}
        self._clock = clock
        self._sleep = sleep

    async def wait(self, host: str) -> None:
        lock = self._locks.setdefault(host, asyncio.Lock())
        async with lock:
            now = self._clock()
            due = self._next.get(host, now)
            if due > now:
                await self._sleep(due - now)
                now = self._clock()
            self._next[host] = max(now, due) + self.interval


def _host(url: str) -> str:
    return urlsplit(url).netloc.lower()


def _retry_after(resp: httpx.Response) -> float | None:
    value = resp.headers.get("retry-after")
    if not value:
        return None
    if value.isdigit():
        return float(value)
    parsed = email.utils.parsedate_to_datetime(value)
    return max(0.0, parsed.timestamp() - time.time()) if parsed else None


class HttpClient:
    def __init__(self, settings: Settings, client: httpx.AsyncClient | None = None,
                 sleep: Callable[[float], Awaitable[None]] = asyncio.sleep):
        self.settings = settings
        self._sleep = sleep
        self._client = client or httpx.AsyncClient(
            follow_redirects=True,
            timeout=httpx.Timeout(settings.timeout_seconds),
        )
        self._client.headers["User-Agent"] = settings.user_agent
        self.limiter = HostRateLimiter(settings.requests_per_second_per_host, sleep=sleep)
        self._robots: dict[str, RobotFileParser | None] = {}
        self.robots_status: dict[str, str] = {}
        self._robots_locks: dict[str, asyncio.Lock] = {}
        self._sem = asyncio.Semaphore(max(1, settings.concurrency))

    async def aclose(self) -> None:
        await self._client.aclose()

    async def __aenter__(self) -> HttpClient:
        return self

    async def __aexit__(self, *exc) -> None:
        await self.aclose()

    # robots.txt (RFC 9309: 4xx means allow all, 5xx or unreachable means disallow all)
    async def allowed(self, url: str) -> bool:
        parts = urlsplit(url)
        origin = f"{parts.scheme}://{parts.netloc}"
        async with self._robots_locks.setdefault(origin, asyncio.Lock()):
            if origin not in self._robots:
                self._robots[origin] = await self._load_robots(origin)
        rp = self._robots[origin]
        if rp is None:
            return False
        return rp.can_fetch(self.settings.user_agent, url)

    def robots_reason(self, url: str) -> str:
        parts = urlsplit(url)
        return self.robots_status.get(f"{parts.scheme}://{parts.netloc}", "")

    async def _load_robots(self, origin: str) -> RobotFileParser | None:
        """Fetch robots.txt with retries. Only a persistent 5xx/429/network failure disallows."""
        rp = RobotFileParser()
        resp = None
        problem = ""
        for attempt in range(self.settings.max_retries + 1):
            try:
                await self.limiter.wait(_host(origin))
                resp = await self._client.get(f"{origin}/robots.txt")
            except httpx.HTTPError as e:
                resp, problem = None, f"robots.txt unreachable ({type(e).__name__})"
            else:
                if resp.status_code < 500 and resp.status_code != 429:
                    break
                problem = f"robots.txt unavailable (HTTP {resp.status_code})"
            if attempt < self.settings.max_retries:
                await self._sleep(self._backoff(attempt))
        else:
            self.robots_status[origin] = problem
            return None
        self.robots_status[origin] = f"robots.txt {resp.status_code}"
        if resp.status_code >= 400:
            rp.parse([])
        else:
            rp.parse(resp.text.splitlines())
        return rp

    def _backoff(self, attempt: int) -> float:
        base = self.settings.backoff_base_seconds * (2**attempt)
        return min(self.settings.backoff_max_seconds, base + random.uniform(0, base / 4))

    async def _send(self, method: str, url: str, headers: dict | None = None,
                    stream: bool = False) -> httpx.Response:
        """Send with robots check, rate limit and retries. Caller closes streamed responses."""
        if not await self.allowed(url):
            parts = urlsplit(url)
            unreachable = self._robots.get(f"{parts.scheme}://{parts.netloc}") is None
            raise RobotsDisallowed(f"{url} [{self.robots_reason(url)}]", unreachable=unreachable)
        last_exc: Exception | None = None
        for attempt in range(self.settings.max_retries + 1):
            await self.limiter.wait(_host(url))
            try:
                req = self._client.build_request(method, url, headers=headers)
                resp = await self._client.send(req, stream=stream)
            except httpx.TransportError as e:
                last_exc = e
                if attempt == self.settings.max_retries:
                    raise FetchError(url, None, f"{type(e).__name__}: {e}") from e
                await self._sleep(self._backoff(attempt))
                continue
            if resp.status_code in RETRY_STATUSES and attempt < self.settings.max_retries:
                wait = _retry_after(resp)
                await resp.aclose()
                await self._sleep(wait if wait is not None else self._backoff(attempt))
                continue
            return resp
        raise FetchError(url, None, str(last_exc))

    @asynccontextmanager
    async def stream_get(self, url: str) -> AsyncIterator[httpx.Response]:
        async with self._sem:
            resp = await self._send("GET", url, stream=True)
            try:
                if resp.status_code >= 400:
                    body = b""
                    async for chunk in resp.aiter_bytes():
                        body += chunk
                        if len(body) >= 4096:
                            break
                    raise FetchError(url, resp.status_code, wall=detect_wall(resp, body))
                yield resp
            finally:
                await resp.aclose()

    async def get_text(self, url: str) -> str:
        async with self._sem:
            resp = await self._send("GET", url)
        if resp.status_code >= 400:
            raise FetchError(url, resp.status_code, wall=detect_wall(resp, resp.content))
        return resp.text

    async def head_or_range(self, url: str) -> UrlCheck:
        """Record size and freshness without downloading the body.

        Pre-signed S3/CloudFront links are often signed for GET only, so HEAD returns 403.
        In that case one GET for the first byte gives the size from Content-Range.
        """
        async with self._sem:
            try:
                resp = await self._send("HEAD", url)
                await resp.aclose()
                if resp.status_code in (403, 405, 501):
                    rng = await self._send("GET", url, headers={"Range": "bytes=0-0"}, stream=True)
                    await rng.aclose()
                    return _check_from(rng, "RANGE_GET")
                return _check_from(resp, "HEAD")
            except RobotsDisallowed:
                return UrlCheck(error="robots_disallowed")
            except FetchError as e:
                return UrlCheck(status=e.status, error=str(e))


def _check_from(resp: httpx.Response, method: str) -> UrlCheck:
    length: int | None = None
    content_range = resp.headers.get("content-range", "")
    if resp.status_code == 206 and "/" in content_range:
        total = content_range.rsplit("/", 1)[1]
        length = int(total) if total.isdigit() else None
    elif resp.headers.get("content-length", "").isdigit():
        length = int(resp.headers["content-length"])
    return UrlCheck(
        status=resp.status_code,
        method=method,
        content_length=length,
        last_modified=resp.headers.get("last-modified", ""),
        etag=resp.headers.get("etag", ""),
        error="" if resp.status_code < 400 else f"http_{resp.status_code}",
    )
