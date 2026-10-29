"""Settings loaded from config.toml plus environment variables."""

from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass
from datetime import date
from pathlib import Path

USER_AGENT_BASE = "EverBetter-TiC-Crawler/1.0"


@dataclass(frozen=True)
class Settings:
    schema_version: str = "2.2.1"
    max_errors_per_toc: int = 50
    requests_per_second_per_host: float = 1.0
    concurrency: int = 8
    timeout_seconds: float = 60.0
    max_retries: int = 5
    backoff_base_seconds: float = 1.0
    backoff_max_seconds: float = 60.0
    max_toc_bytes: int = 2 * 1024**3
    max_run_bytes: int = 10 * 1024**3
    quarterly_from: date = date(2027, 3, 6)
    stale_days_monthly: int = 35
    stale_days_quarterly: int = 100
    root_txt_from: date = date(2027, 9, 6)
    min_recrawl_days: int = 6
    seed_file: str = "seeds/payers.yaml"
    exchange_puf_url: str = ""
    exchange_puf_max_bytes: int = 500 * 1024**2
    root_txt_filename: str = "price-transparency.txt"
    local_dir: str = "out"
    state_db: str = "state/tic.sqlite"
    sheet_name: str = "TiC Payer Registry"
    manifest_plan_name_cap: int = 50
    contact_email: str = ""
    drive_folder_id: str = ""
    raw_store: str = ""
    credentials_path: str = ""

    @property
    def user_agent(self) -> str:
        contact = self.contact_email or "unset"
        return f"{USER_AGENT_BASE} (+{contact})"

    def stale_days(self, today: date) -> int:
        return self.stale_days_quarterly if today >= self.quarterly_from else self.stale_days_monthly


def _as_date(v) -> date:
    return v if isinstance(v, date) else date.fromisoformat(str(v))


def load_settings(path: str | os.PathLike | None = None, env: dict | None = None) -> Settings:
    env = dict(os.environ if env is None else env)
    cfg_path = Path(path or env.get("TIC_CONFIG", "config.toml"))
    raw: dict = {}
    if cfg_path.exists():
        with cfg_path.open("rb") as fh:
            raw = tomllib.load(fh)
    flat: dict = {}
    s = raw.get("schema", {})
    if "version" in s:
        flat["schema_version"] = str(s["version"])
    if "max_errors_per_toc" in s:
        flat["max_errors_per_toc"] = int(s["max_errors_per_toc"])
    for section in ("http", "limits", "schedule", "discovery", "output"):
        flat.update(raw.get(section, {}))
    for k in ("quarterly_from", "root_txt_from"):
        if k in flat:
            flat[k] = _as_date(flat[k])
    flat["contact_email"] = env.get("TIC_CONTACT_EMAIL", "")
    flat["drive_folder_id"] = env.get("TIC_DRIVE_FOLDER_ID", "")
    flat["raw_store"] = env.get("TIC_RAW_STORE", "")
    flat["credentials_path"] = env.get("GOOGLE_APPLICATION_CREDENTIALS", "")
    if env.get("TIC_STATE_DB"):
        flat["state_db"] = env["TIC_STATE_DB"]
    if env.get("TIC_OUTPUT_DIR"):
        flat["local_dir"] = env["TIC_OUTPUT_DIR"]
    known = set(Settings.__dataclass_fields__)
    unknown = set(flat) - known
    if unknown:
        raise ValueError(f"unknown config keys: {sorted(unknown)}")
    return Settings(**flat)
