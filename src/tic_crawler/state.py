"""Resumable crawl state in SQLite."""

from __future__ import annotations

import json
import sqlite3
from datetime import UTC, datetime
from pathlib import Path

from .models import Payer, PayerResult

SCHEMA = """
CREATE TABLE IF NOT EXISTS payers (
    payer_id TEXT PRIMARY KEY,
    data TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
    run_id TEXT PRIMARY KEY,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL,
    args TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS run_payers (
    run_id TEXT NOT NULL,
    payer_id TEXT NOT NULL,
    status TEXT NOT NULL,
    result TEXT NOT NULL,
    PRIMARY KEY (run_id, payer_id)
);
CREATE TABLE IF NOT EXISTS payer_latest (
    payer_id TEXT PRIMARY KEY,
    toc_sha256 TEXT,
    toc_url TEXT,
    crawled_at TEXT,
    result TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS uploads (
    path TEXT PRIMARY KEY,
    sha256 TEXT NOT NULL,
    remote_id TEXT,
    uploaded_at TEXT NOT NULL
);
"""


def utcnow() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


class StateStore:
    def __init__(self, path: str | Path):
        path = Path(path)
        if str(path) != ":memory:":
            path.parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(str(path))
        self.db.row_factory = sqlite3.Row
        self.db.executescript(SCHEMA)

    def close(self) -> None:
        self.db.close()

    # payers
    def save_payers(self, payers: list[Payer]) -> None:
        now = utcnow()
        with self.db:
            self.db.executemany(
                "INSERT INTO payers(payer_id, data, updated_at) VALUES (?,?,?) "
                "ON CONFLICT(payer_id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at",
                [(p.payer_id, json.dumps(p.to_dict(), sort_keys=True), now) for p in payers],
            )

    def load_payers(self) -> list[Payer]:
        rows = self.db.execute("SELECT data FROM payers ORDER BY payer_id").fetchall()
        return [Payer.from_dict(json.loads(r["data"])) for r in rows]

    # runs
    def open_run(self, args: dict) -> tuple[str, bool]:
        """Return (run_id, resumed). An unfinished run with the same args is resumed."""
        key = json.dumps(args, sort_keys=True)
        row = self.db.execute(
            "SELECT run_id FROM runs WHERE status='running' AND args=? ORDER BY started_at DESC",
            (key,),
        ).fetchone()
        if row:
            return row["run_id"], True
        base = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
        run_id, n = base, 1
        while self.db.execute("SELECT 1 FROM runs WHERE run_id=?", (run_id,)).fetchone():
            n += 1
            run_id = f"{base}-{n}"
        with self.db:
            self.db.execute(
                "INSERT INTO runs(run_id, started_at, status, args) VALUES (?,?,?,?)",
                (run_id, utcnow(), "running", key),
            )
        return run_id, False

    def finish_run(self, run_id: str, status: str = "finished") -> None:
        with self.db:
            self.db.execute(
                "UPDATE runs SET status=?, finished_at=? WHERE run_id=?", (status, utcnow(), run_id)
            )

    def done_payers(self, run_id: str) -> set[str]:
        rows = self.db.execute(
            "SELECT payer_id FROM run_payers WHERE run_id=?", (run_id,)
        ).fetchall()
        return {r["payer_id"] for r in rows}

    def run_results(self, run_id: str) -> list[dict]:
        rows = self.db.execute(
            "SELECT result FROM run_payers WHERE run_id=? ORDER BY payer_id", (run_id,)
        ).fetchall()
        return [json.loads(r["result"]) for r in rows]

    def record_result(self, run_id: str, result: PayerResult) -> None:
        data = json.dumps(result.to_dict(), sort_keys=True, default=str)
        with self.db:
            self.db.execute(
                "INSERT OR REPLACE INTO run_payers(run_id, payer_id, status, result) VALUES (?,?,?,?)",
                (run_id, result.payer_id, result.status, data),
            )
            if result.status in ("ok", "unchanged", "failed", "skipped"):
                prev = self.latest(result.payer_id)
                sha = result.toc_sha256 or (prev or {}).get("toc_sha256")
                merged = result.to_dict()
                if result.status != "ok" and prev:
                    # Keep the counts from the run that produced the current TOC.
                    for k in ("mrf_count", "total_bytes", "schema_valid", "validation", "toc_bytes",
                              "toc_last_updated_on"):
                        merged[k] = prev.get(k)
                    merged["toc_url"] = result.toc_url or prev.get("toc_url", "")
                    merged["toc_sha256"] = sha or ""
                self.db.execute(
                    "INSERT OR REPLACE INTO payer_latest(payer_id, toc_sha256, toc_url, crawled_at, "
                    "result) VALUES (?,?,?,?,?)",
                    (result.payer_id, sha, merged["toc_url"], result.crawled_at,
                     json.dumps(merged, sort_keys=True, default=str)),
                )

    def latest(self, payer_id: str) -> dict | None:
        row = self.db.execute(
            "SELECT result FROM payer_latest WHERE payer_id=?", (payer_id,)
        ).fetchone()
        return json.loads(row["result"]) if row else None

    def latest_sha(self, payer_id: str) -> str | None:
        row = self.db.execute(
            "SELECT toc_sha256 FROM payer_latest WHERE payer_id=?", (payer_id,)
        ).fetchone()
        return row["toc_sha256"] if row else None

    def all_latest(self) -> dict[str, dict]:
        rows = self.db.execute("SELECT payer_id, result FROM payer_latest").fetchall()
        return {r["payer_id"]: json.loads(r["result"]) for r in rows}

    # uploads (Drive idempotency)
    def uploaded_sha(self, path: str) -> str | None:
        row = self.db.execute("SELECT sha256 FROM uploads WHERE path=?", (path,)).fetchone()
        return row["sha256"] if row else None

    def mark_uploaded(self, path: str, sha256: str, remote_id: str) -> None:
        with self.db:
            self.db.execute(
                "INSERT OR REPLACE INTO uploads(path, sha256, remote_id, uploaded_at) VALUES (?,?,?,?)",
                (path, sha256, remote_id, utcnow()),
            )
