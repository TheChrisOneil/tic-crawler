from __future__ import annotations

import json
from pathlib import Path

import pytest

from tic_crawler.config import Settings

FIXTURES = Path(__file__).parent / "fixtures"


@pytest.fixture
def cms_sample_bytes() -> bytes:
    return (FIXTURES / "toc" / "cms-sample.json").read_bytes()


@pytest.fixture
def cms_sample() -> dict:
    return json.loads((FIXTURES / "toc" / "cms-sample.json").read_text())


@pytest.fixture
def settings(tmp_path: Path) -> Settings:
    return Settings(
        requests_per_second_per_host=0,
        backoff_base_seconds=0,
        backoff_max_seconds=0,
        max_retries=2,
        contact_email="test@example.com",
        local_dir=str(tmp_path / "out"),
        state_db=str(tmp_path / "state.sqlite"),
        seed_file=str(tmp_path / "payers.yaml"),
        exchange_puf_url="",
    )


async def no_sleep(_s: float) -> None:
    return None
