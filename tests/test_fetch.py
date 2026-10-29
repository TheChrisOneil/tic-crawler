import httpx
import pytest
import respx

from tests.conftest import no_sleep
from tic_crawler.fetch import FetchError, HostRateLimiter, HttpClient, RobotsDisallowed


@pytest.fixture
def client(settings):
    return HttpClient(settings, sleep=no_sleep)


async def test_rate_limiter_spaces_same_host():
    now = [0.0]
    slept = []

    async def fake_sleep(s):
        slept.append(s)
        now[0] += s

    lim = HostRateLimiter(1.0, clock=lambda: now[0], sleep=fake_sleep)
    await lim.wait("a")
    await lim.wait("a")
    await lim.wait("b")
    await lim.wait("a")
    assert slept == [1.0, 1.0]


@respx.mock
async def test_user_agent_and_robots_allow(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    route = respx.get("https://h/x").mock(return_value=httpx.Response(200, text="ok"))
    assert await client.get_text("https://h/x") == "ok"
    ua = route.calls[0].request.headers["user-agent"]
    assert ua == "EverBetter-TiC-Crawler/1.0 (+test@example.com)"
    await client.aclose()


@respx.mock
async def test_robots_disallow(client):
    respx.get("https://h/robots.txt").mock(
        return_value=httpx.Response(200, text="User-agent: *\nDisallow: /private\n"))
    with pytest.raises(RobotsDisallowed):
        await client.get_text("https://h/private/toc.json")
    check = await client.head_or_range("https://h/private/f.json")
    assert check.error == "robots_disallowed"


@respx.mock
async def test_robots_5xx_disallows_all(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(503))
    assert await client.allowed("https://h/a") is False


@respx.mock
async def test_robots_unreachable_disallows(client):
    respx.get("https://h/robots.txt").mock(side_effect=httpx.ConnectError("boom"))
    assert await client.allowed("https://h/a") is False


@respx.mock
async def test_retry_on_429_then_success(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    route = respx.get("https://h/x").mock(side_effect=[
        httpx.Response(429, headers={"Retry-After": "0"}),
        httpx.Response(503),
        httpx.Response(200, text="done"),
    ])
    assert await client.get_text("https://h/x") == "done"
    assert route.call_count == 3


@respx.mock
async def test_retries_exhausted_returns_error_status(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://h/x").mock(return_value=httpx.Response(500))
    with pytest.raises(FetchError) as ei:
        await client.get_text("https://h/x")
    assert ei.value.status == 500


@respx.mock
async def test_transport_error_retried_then_raised(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    route = respx.get("https://h/x").mock(side_effect=httpx.ConnectTimeout("slow"))
    with pytest.raises(FetchError):
        await client.get_text("https://h/x")
    assert route.call_count == 3


@respx.mock
async def test_head_records_metadata(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    respx.head("https://h/f.json.gz").mock(return_value=httpx.Response(200, headers={
        "Content-Length": "12345", "Last-Modified": "Wed, 01 Oct 2026 00:00:00 GMT",
        "ETag": '"abc"'}))
    c = await client.head_or_range("https://h/f.json.gz")
    assert (c.status, c.method, c.content_length, c.etag) == (200, "HEAD", 12345, '"abc"')
    assert c.last_modified.startswith("Wed")
    assert c.error == ""


@respx.mock
async def test_head_forbidden_falls_back_to_range_get(client):
    respx.get("https://s3/robots.txt").mock(return_value=httpx.Response(404))
    respx.head("https://s3/f.json").mock(return_value=httpx.Response(403))
    get = respx.get("https://s3/f.json").mock(return_value=httpx.Response(
        206, headers={"Content-Range": "bytes 0-0/987654321", "ETag": "e"}, content=b"{"))
    c = await client.head_or_range("https://s3/f.json")
    assert get.calls[0].request.headers["range"] == "bytes=0-0"
    assert (c.status, c.method, c.content_length) == (206, "RANGE_GET", 987654321)


@respx.mock
async def test_head_404_recorded(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    respx.head("https://h/gone").mock(return_value=httpx.Response(404))
    c = await client.head_or_range("https://h/gone")
    assert c.status == 404 and c.error == "http_404"


@respx.mock
async def test_head_transport_failure_recorded(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    respx.head("https://h/x").mock(side_effect=httpx.ConnectError("nope"))
    c = await client.head_or_range("https://h/x")
    assert c.status is None and "ConnectError" in c.error


@respx.mock
async def test_stream_get_raises_on_404(client):
    respx.get("https://h/robots.txt").mock(return_value=httpx.Response(404))
    respx.get("https://h/x").mock(return_value=httpx.Response(404))
    with pytest.raises(FetchError):
        async with client.stream_get("https://h/x"):
            pass
