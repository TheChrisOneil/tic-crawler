"""Command line interface (Typer): discover, crawl, sync-drive, report."""

from __future__ import annotations

import asyncio
import csv
import hashlib
import json
from datetime import UTC, date, datetime
from pathlib import Path
from typing import Annotated

import typer

from .config import Settings, load_settings
from .discovery import DiscoveryRegistry, load_seed_file
from .fetch import HttpClient
from .models import Payer, PayerResult, RegistryRow
from .pipeline import RawDownloadPipeline, TocCrawlPipeline
from .state import StateStore, utcnow
from .storage import (
    DriveAdapter,
    SheetRegistry,
    build_google_services,
    make_raw_store,
)

app = typer.Typer(add_completion=False, help="Catalog Transparency in Coverage MRFs.")

ConfigOpt = Annotated[Path | None, typer.Option("--config", help="Path to config.toml")]


def _settings(config: Path | None) -> Settings:
    return load_settings(config)


def _today() -> date:
    return datetime.now(UTC).date()


def _payers(state: StateStore, settings: Settings) -> list[Payer]:
    payers = state.load_payers()
    if not payers and Path(settings.seed_file).exists():
        payers = load_seed_file(Path(settings.seed_file))
    return payers


@app.command()
def discover(
    source: Annotated[list[str] | None, typer.Option(help="Only run these sources")] = None,
    config: ConfigOpt = None,
) -> None:
    """Find payers from all discovery sources, de-duplicate, and save them."""
    settings = _settings(config)
    state = StateStore(settings.state_db)

    async def go():
        async with HttpClient(settings) as http:
            return await DiscoveryRegistry(settings).run(http, _today(), only=source)

    payers, status = asyncio.run(go())
    state.save_payers(payers)
    out = Path(settings.local_dir)
    out.mkdir(parents=True, exist_ok=True)
    (out / "payers.json").write_text(json.dumps([p.to_dict() for p in payers], indent=2))
    for name, s in status.items():
        typer.echo(f"{name}: {s}")
    with_toc = sum(1 for p in payers if p.toc_url)
    typer.echo(f"payers: {len(payers)} (with TOC URL: {with_toc})")


@app.command()
def crawl(
    payer: Annotated[list[str] | None, typer.Option("--payer", help="Payer id (repeatable)")] = None,
    all_: Annotated[bool, typer.Option("--all", help="Crawl every payer with a TOC URL")] = False,
    download_raw: Annotated[bool, typer.Option("--download-raw")] = False,
    force: Annotated[bool, typer.Option(help="Ignore TOC hash and recrawl window")] = False,
    config: ConfigOpt = None,
) -> None:
    """Fetch, validate and catalog TOCs. Raw MRFs are only HEAD-checked unless --download-raw."""
    if not payer and not all_:
        raise typer.BadParameter("give --payer <id> or --all")
    settings = _settings(config)
    state = StateStore(settings.state_db)
    known = {p.payer_id: p for p in _payers(state, settings)}
    if payer:
        missing = [p for p in payer if p not in known]
        if missing:
            raise typer.BadParameter(f"unknown payer ids: {', '.join(missing)}")
        targets = [known[p] for p in payer]
    else:
        targets = [p for p in known.values() if p.toc_url]
    run_args = {"payers": sorted(p.payer_id for p in targets), "raw": download_raw, "force": force}
    run_id, resumed = state.open_run(run_args)
    done = state.done_payers(run_id)
    if resumed:
        typer.echo(f"resuming run {run_id}: {len(done)} payers already done")
    results = asyncio.run(_crawl(settings, state, targets, done, run_id, download_raw, force, all_))
    state.finish_run(run_id)
    log = write_run_log(settings, state, run_id, run_args)
    write_local_registry(settings, state, known)
    ok = sum(1 for r in results if r.status == "ok")
    typer.echo(f"run {run_id}: {len(results)} crawled now, {ok} ok; log {log}")


async def _crawl(settings, state, targets, done, run_id, download_raw, force, all_):
    out = Path(settings.local_dir)
    results: list[PayerResult] = []
    async with HttpClient(settings) as http:
        if download_raw:
            pipe = RawDownloadPipeline(settings, http, state, out, force=force,
                                       raw_store=make_raw_store(settings.raw_store),
                                       max_run_bytes=settings.max_run_bytes)
        else:
            pipe = TocCrawlPipeline(settings, http, state, out, force=force)
        for p in targets:
            if p.payer_id in done:
                continue
            if all_ and not force and _recent(state, p.payer_id, settings.min_recrawl_days):
                continue
            typer.echo(f"crawling {p.payer_id} ...")
            r = await pipe.run(p)
            state.record_result(run_id, r)
            results.append(r)
            typer.echo(f"  {r.status} mrfs={r.mrf_count} bytes={r.total_bytes} {r.error}")
    return results


def _recent(state: StateStore, payer_id: str, days: int) -> bool:
    latest = state.latest(payer_id)
    if not latest or latest.get("status") == "failed" or not latest.get("crawled_at"):
        return False
    crawled = datetime.strptime(latest["crawled_at"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=UTC)
    return (datetime.now(UTC) - crawled).days < days


def write_run_log(settings: Settings, state: StateStore, run_id: str, args: dict) -> Path:
    results = state.run_results(run_id)
    summary = {
        "payers": len(results),
        "by_status": {},
        "mrf_urls": sum(r.get("mrf_count") or 0 for r in results if r["status"] == "ok"),
        "mrf_bytes": sum(r.get("total_bytes") or 0 for r in results if r["status"] == "ok"),
        "toc_bytes": sum(r.get("toc_bytes") or 0 for r in results),
    }
    for r in results:
        summary["by_status"][r["status"]] = summary["by_status"].get(r["status"], 0) + 1
    log = {"run_id": run_id, "finished_at": utcnow(), "args": args,
           "schema_version": settings.schema_version, "summary": summary, "results": results}
    path = Path(settings.local_dir) / "runs" / f"{run_id}.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(log, indent=2, sort_keys=True, default=str))
    return path


def registry_rows(state: StateStore, payers: dict[str, Payer]) -> list[RegistryRow]:
    latest = state.all_latest()
    rows = []
    for pid in sorted(payers):
        p = payers[pid]
        r = latest.get(pid, {})
        valid = r.get("schema_valid")
        rows.append(RegistryRow(
            payer_id=pid,
            legal_name=p.legal_name,
            hios_ids=",".join(p.hios_ids),
            landing_url=p.landing_url,
            toc_url=r.get("toc_url") or p.toc_url,
            last_crawled=r.get("crawled_at", ""),
            toc_sha256=r.get("toc_sha256", ""),
            mrf_count=r.get("mrf_count") or 0,
            total_bytes=r.get("total_bytes") or 0,
            schema_valid="" if valid is None else str(bool(valid)).upper(),
            last_error=r.get("error", "") if r.get("status") in ("failed", "skipped") else "",
        ))
    return rows


def write_local_registry(settings: Settings, state: StateStore, payers: dict[str, Payer]) -> Path:
    path = Path(settings.local_dir) / "registry.csv"
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as fh:
        w = csv.writer(fh)
        w.writerow(RegistryRow.COLUMNS)
        for row in registry_rows(state, payers):
            w.writerow(row.values())
    return path


def _sha_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        while chunk := fh.read(1 << 20):
            h.update(chunk)
    return h.hexdigest()


def sync_outputs(settings: Settings, state: StateStore, drive: DriveAdapter) -> dict[str, int]:
    """Upload catalog files that are new or changed since the last sync."""
    root = Path(settings.local_dir)
    counts = {"uploaded": 0, "skipped": 0}
    for sub in ("toc", "manifests", "runs"):
        for path in sorted((root / sub).rglob("*")):
            if not path.is_file():
                continue
            rel = path.relative_to(root).as_posix()
            sha = _sha_file(path)
            if state.uploaded_sha(rel) == sha:
                counts["skipped"] += 1
                continue
            remote = drive.put_file(rel, path)
            state.mark_uploaded(rel, sha, remote)
            counts["uploaded"] += 1
    return counts


@app.command("sync-drive")
def sync_drive(config: ConfigOpt = None) -> None:
    """Upload TOCs, manifests and run logs to Drive and update the registry Sheet."""
    settings = _settings(config)
    if not settings.credentials_path:
        raise typer.BadParameter("GOOGLE_APPLICATION_CREDENTIALS is not set")
    state = StateStore(settings.state_db)
    drive_svc, sheets_svc = build_google_services(settings.credentials_path)
    drive = DriveAdapter(drive_svc, settings.drive_folder_id)
    counts = sync_outputs(settings, state, drive)
    payers = {p.payer_id: p for p in _payers(state, settings)}
    sheet_id = drive.find_or_create_sheet(settings.sheet_name)
    sheet = SheetRegistry(sheets_svc, sheet_id).upsert(registry_rows(state, payers))
    typer.echo(f"drive: {counts}; sheet {sheet_id}: {sheet}")


@app.command()
def report(
    as_json: Annotated[bool, typer.Option("--json")] = False,
    config: ConfigOpt = None,
) -> None:
    """Summarize the latest state of every payer, including staleness."""
    settings = _settings(config)
    state = StateStore(settings.state_db)
    payers = {p.payer_id: p for p in _payers(state, settings)}
    latest = state.all_latest()
    today = _today()
    stale_days = settings.stale_days(today)
    rows = []
    for pid in sorted(payers):
        r = latest.get(pid) or {}
        rows.append({
            "payer_id": pid,
            "status": r.get("status", "never"),
            "mrf_count": r.get("mrf_count") or 0,
            "total_bytes": r.get("total_bytes") or 0,
            "schema_valid": r.get("schema_valid"),
            "last_crawled": r.get("crawled_at", ""),
            "error": r.get("error", ""),
            "toc_last_updated_on": r.get("toc_last_updated_on", ""),
            "stale": _is_stale(r, today, stale_days),
        })
    if as_json:
        typer.echo(json.dumps(rows, indent=2))
        return
    total = len(rows)
    crawled = sum(1 for r in rows if r["status"] != "never")
    failed = sum(1 for r in rows if r["status"] == "failed")
    typer.echo(f"payers {total}, crawled {crawled}, failed {failed}, stale threshold {stale_days}d")
    for r in rows:
        if r["status"] == "never":
            continue
        typer.echo(f"{r['payer_id']:<40} {r['status']:<9} mrfs={r['mrf_count']:<7} "
                   f"bytes={r['total_bytes']:<15} valid={r['schema_valid']} "
                   f"stale={r['stale']} {r['error'][:60]}")


def _is_stale(result: dict, today: date, stale_days: int) -> bool | None:
    lu = result.get("toc_last_updated_on")
    if not lu:
        return None
    try:
        return (today - date.fromisoformat(lu)).days > stale_days
    except ValueError:
        return None


if __name__ == "__main__":
    app()
