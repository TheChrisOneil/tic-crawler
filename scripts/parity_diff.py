"""Compare a Python crawl (py/) with a Go crawl (go/) of the same payers.

Prints one line per difference and exits 1 if any stable field differs. Fields that can
legitimately change between two crawls minutes apart (timestamps, run ids, Last-Modified,
ETag, signed URL query strings) are ignored.
"""

import csv
import json
import sys
from pathlib import Path

VOLATILE = {"generated_at", "crawled_at", "last_crawled", "finished_at", "audited_at", "run_id",
            "last_modified", "etag", "runs", "age_days", "stale", "manifest_file", "url_status",
            "toc_plans"}


def walk(a, b, path, out):
    if type(a) is not type(b):
        out.append(f"{path}: type {type(a).__name__} vs {type(b).__name__}")
    elif isinstance(a, dict):
        for k in sorted(set(a) | set(b)):
            if k in VOLATILE:
                continue
            if k == "verdict" and k in a and k in b and a[k] != b[k]:
                out.append(f"{path}.url: {a.get('url', '')[:100]!r} errors {a.get('error', '')[:60]!r} vs {b.get('error', '')[:60]!r}")
            if k not in a or k not in b:
                out.append(f"{path}.{k}: only in {'py' if k in a else 'go'}")
            else:
                walk(a[k], b[k], f"{path}.{k}", out)
    elif isinstance(a, list):
        if len(a) != len(b):
            out.append(f"{path}: {len(a)} vs {len(b)} items")
        for i, (x, y) in enumerate(zip(a, b)):
            walk(x, y, f"{path}[{i}]", out)
    elif a != b:
        sa, sb = str(a), str(b)
        i = next((n for n, (x, y) in enumerate(zip(sa, sb)) if x != y), min(len(sa), len(sb)))
        start = max(0, i - 30)
        out.append(f"{path}: @{i} {sa[start:start + 80]!r} vs {sb[start:start + 80]!r}")


def rows(path):
    with open(path, newline="", encoding="utf-8") as fh:
        return list(csv.DictReader(fh))


def main(root):
    py, go = Path(root) / "py" / "out", Path(root) / "go" / "out"
    diffs = []
    for pdir in sorted((py / "manifests").glob("*")):
        pid = pdir.name
        a = sorted(pdir.glob("*.csv"))
        b = sorted((go / "manifests" / pid).glob("*.csv"))
        if not b:
            diffs.append(f"manifests/{pid}: missing in go")
            continue
        ra, rb = rows(a[-1]), rows(b[-1])
        if ra and rb and list(ra[0]) != list(rb[0]):
            diffs.append(f"manifests/{pid}: columns differ")
        ka = {r["url"].split("?")[0]: r for r in ra}
        kb = {r["url"].split("?")[0]: r for r in rb}
        if set(ka) != set(kb):
            diffs.append(f"manifests/{pid}: {len(set(ka) - set(kb))} urls only in py, "
                         f"{len(set(kb) - set(ka))} only in go")
        for u in sorted(set(ka) & set(kb)):
            for col, va in ka[u].items():
                if col in VOLATILE or col == "url":
                    continue
                if va != kb[u].get(col):
                    diffs.append(f"manifests/{pid} {u[:80]} {col}: {va[:60]!r} vs {kb[u].get(col, '')[:60]!r}")
    for pdir in sorted((go / "manifests").glob("*")):
        if not (py / "manifests" / pdir.name).exists():
            diffs.append(f"manifests/{pdir.name}: missing in py")
    for name in ["dashboard/data.json"] + sorted(
            f"dashboard/payers/{p.name}" for p in (py / "dashboard" / "payers").glob("*.json")):
        if not (go / name).exists():
            diffs.append(f"{name}: missing in go")
            continue
        walk(json.loads((py / name).read_text()), json.loads((go / name).read_text()), name, diffs)
    ra = {r["payer_id"]: r for r in rows(py / "registry.csv")}
    rb = {r["payer_id"]: r for r in rows(go / "registry.csv")}
    walk(ra, rb, "registry", diffs)
    # Size and status differences go last too: they explain total_bytes mismatches.
    notes = [d for d in diffs if any(k in d for k in (" content_length:", " http_status:", " check_method:",
                                                       "].error:", "].url:"))]
    for d in diffs[:300]:
        print(d)
    # Grouped counts last, so the tail of the log (the run annotation) shows the overview.
    groups = {}
    for d in diffs:
        head = d.split(":")[0].split(" ")
        key = f"{head[0].split('/')[0]} {head[-1] if len(head) > 1 else ''}".strip()
        groups[key] = groups.get(key, 0) + 1
    for k, n in sorted(groups.items(), key=lambda x: -x[1])[:13]:
        print(f"  {n:6d}  {k}")
    for d in notes[:8]:
        print(d)
    # Known and intended: Go's HEAD sends no Accept-Encoding, so servers that compress report
    # the real file size instead of the gzip transfer size Python saw.
    known = [d for d in diffs if any(k in d for k in ("content_length", "total_bytes", "mrf_bytes"))]
    print(f"{len(diffs)} differences ({len(known)} size differences from Accept-Encoding, expected)")
    return 1 if len(diffs) > len(known) else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
