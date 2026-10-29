"""Export compact JSON for the dashboard (dashboard/ reads these from the data branch)."""

from __future__ import annotations

import csv
import json
from collections import Counter
from datetime import date
from pathlib import Path

from .config import Settings
from .models import Payer
from .parse import iter_toc, open_json
from .state import StateStore, utcnow

PLAN_CAP = 5000
ROW_FIELDS = (
    "file_type", "url", "description", "plan_count", "plan_names", "plans_truncated",
    "plan_id_types", "plan_market_types", "http_status", "check_method", "content_length",
    "last_modified", "etag", "check_error",
)


def _latest(path: Path, pattern: str) -> Path | None:
    files = sorted(path.glob(pattern))
    return files[-1] if files else None


def _int(v) -> int | None:
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


def manifest_rows(path: Path) -> list[dict]:
    rows = []
    with path.open(newline="", encoding="utf-8") as fh:
        for r in csv.DictReader(fh):
            if not r.get("url") and not r.get("check_error"):
                continue
            row = {k: r.get(k, "") for k in ROW_FIELDS}
            row["plan_count"] = _int(row["plan_count"]) or 0
            row["http_status"] = _int(row["http_status"])
            row["content_length"] = _int(row["content_length"])
            row["plans_truncated"] = row["plans_truncated"] == "True"
            rows.append(row)
    return rows


def toc_plans(path: Path, cap: int = PLAN_CAP) -> dict:
    """Distinct plans in a stored TOC with the number of files that reference each."""
    plans: dict[tuple, dict] = {}
    total_refs = 0
    with open_json(path) as fh:
        for ev in iter_toc(fh):
            if ev.kind != "item" or not isinstance(ev.value, dict):
                continue
            item = ev.value
            n_in = len(item.get("in_network_files") or []) if isinstance(
                item.get("in_network_files"), list) else 0
            has_aa = isinstance(item.get("allowed_amount_file"), dict)
            for p in item.get("reporting_plans") or []:
                if not isinstance(p, dict):
                    continue
                total_refs += 1
                key = (p.get("plan_id_type"), p.get("plan_id"), p.get("plan_name"))
                entry = plans.get(key)
                if entry is None:
                    if len(plans) >= cap:
                        continue
                    entry = plans[key] = {
                        "plan_name": p.get("plan_name"), "plan_id_type": p.get("plan_id_type"),
                        "plan_id": p.get("plan_id"), "plan_market_type": p.get("plan_market_type"),
                        "issuer_name": p.get("issuer_name"),
                        "plan_sponsor_name": p.get("plan_sponsor_name"),
                        "in_network_files": 0, "allowed_amount_files": 0,
                    }
                entry["in_network_files"] += n_in
                entry["allowed_amount_files"] += int(has_aa)
    return {"plans": list(plans.values()), "plan_refs": total_refs,
            "truncated": len(plans) >= cap, "cap": cap}


def build(settings: Settings, state: StateStore, payers: dict[str, Payer], today: date) -> Path:
    out = Path(settings.local_dir)
    dest = out / "dashboard"
    (dest / "payers").mkdir(parents=True, exist_ok=True)
    latest = state.all_latest()
    stale_days = settings.stale_days(today)
    rows = []
    errors = {"access": [], "records": []}
    for pid in sorted(payers):
        p = payers[pid]
        r = latest.get(pid) or {}
        v = r.get("validation") or {}
        lu = r.get("toc_last_updated_on") or ""
        age = None
        if lu:
            try:
                age = (today - date.fromisoformat(lu)).days
            except ValueError:
                age = None
        man = _latest(out / "manifests" / pid, "*.csv")
        mrows = manifest_rows(man) if man else []
        status_counts = Counter(str(x["http_status"] or x["check_error"] or "none") for x in mrows)
        detail = {
            "payer": p.to_dict(),
            "result": r,
            "manifest_file": man.relative_to(out).as_posix() if man else "",
            "rows": mrows,
        }
        toc = _latest(out / "toc" / pid, "*.json.gz")
        if toc:
            try:
                detail["toc_plans"] = toc_plans(toc)
            except Exception as e:  # a broken TOC is already reported by validation
                detail["toc_plans"] = {"error": f"{type(e).__name__}: {e}"[:200]}
        (dest / "payers" / f"{pid}.json").write_text(json.dumps(detail, default=str))
        status = r.get("status", "never")
        rows.append({
            "payer_id": pid, "legal_name": p.legal_name, "kind": p.kind, "states": p.states,
            "landing_url": p.landing_url, "toc_url": r.get("toc_url") or p.toc_url,
            "has_toc_source": bool(p.toc_url or p.toc_link_pattern),
            "status": status, "last_crawled": r.get("crawled_at", ""),
            "toc_last_updated_on": lu, "age_days": age,
            "stale": None if age is None else age > stale_days,
            "mrf_count": r.get("mrf_count") or 0, "total_bytes": r.get("total_bytes") or 0,
            "toc_bytes": r.get("toc_bytes") or 0, "schema_valid": r.get("schema_valid"),
            "validation_errors": v.get("error_count") or 0,
            "invalid_items": v.get("invalid_items") or 0, "items": v.get("item_count") or 0,
            "url_status": dict(status_counts), "error": r.get("error", ""),
            "file_version": v.get("file_version", ""),
        })
        if status in ("failed", "skipped"):
            errors["access"].append({"payer_id": pid, "level": "payer", "status": status,
                                     "url": r.get("toc_url") or p.toc_url or p.landing_url,
                                     "error": r.get("error", "")})
        for x in mrows:
            if x["check_error"]:
                errors["access"].append({"payer_id": pid, "level": "mrf", "status": x["http_status"],
                                         "url": x["url"], "error": x["check_error"],
                                         "file_type": x["file_type"]})
        for k in v.get("error_kinds") or []:
            errors["records"].append({"payer_id": pid, **k})
    audit_path = out / "access_audit.json"
    access = json.loads(audit_path.read_text()) if audit_path.exists() else None
    crawled = [r for r in rows if r["status"] != "never"]
    data = {
        "generated_at": utcnow(),
        "schema_version": settings.schema_version,
        "stale_threshold_days": stale_days,
        "totals": {
            "payers": len(rows),
            "with_toc_source": sum(1 for r in rows if r["has_toc_source"]),
            "crawled": len(crawled),
            "ok": sum(1 for r in rows if r["status"] in ("ok", "unchanged")),
            "failed": sum(1 for r in rows if r["status"] in ("failed", "skipped")),
            "stale": sum(1 for r in rows if r["stale"]),
            "mrf_urls": sum(r["mrf_count"] for r in rows),
            "mrf_bytes": sum(r["total_bytes"] for r in rows),
            "schema_invalid": sum(1 for r in rows if r["schema_valid"] is False),
        },
        "payers": rows,
        "errors": errors,
        "access_audit": access,
        "runs": sorted(p.name for p in (out / "runs").glob("*.json")) if (out / "runs").exists() else [],
    }
    (dest / "data.json").write_text(json.dumps(data, default=str))
    return dest
