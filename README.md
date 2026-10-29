# tic-crawler

Deterministic Python 3.12 tool that finds U.S. health plan and issuer Transparency in Coverage (TiC)
machine-readable files (MRFs) and catalogs them in Google Drive. It makes no LLM or inference calls.

- Regulation: 45 CFR 147.212. MRFs are public and need no account.
- Spec: [CMSgov/price-transparency-guide](https://github.com/CMSgov/price-transparency-guide), pinned to **v2.2.1** (`config.toml`).
- The data is public pricing data. It contains no PHI, and the tool never ingests member data.

## What it stores

Google Drive (folder `TIC_DRIVE_FOLDER_ID`, which must be in a **Shared Drive**):

| Path | Content |
| --- | --- |
| `TiC Payer Registry` (Sheet) | One row per payer: payer_id, legal_name, hios_ids, landing_url, toc_url, last_crawled, toc_sha256, mrf_count, total_bytes, schema_valid, last_error |
| `toc/<payer_id>/<yyyy-mm-dd>.json.gz` | Raw table-of-contents file |
| `manifests/<payer_id>/<yyyy-mm-dd>.csv` | One row per unique MRF URL (see below) |
| `runs/<timestamp>.json` | Run log |

Raw in-network and allowed-amount files are **never** sent to Drive (Drive allows 750 GB of uploads
per user per day, and single MRFs can be terabytes). `--download-raw` streams them to `TIC_RAW_STORE`
(`gcs://bucket/prefix` or a local path) and refuses any Drive target. It is off by default.

### Manifest columns

`payer_id, toc_url, toc_sha256, toc_last_updated_on, toc_file_version, pinned_schema_version,
toc_schema_valid, toc_validation_error_count, toc_validation_errors, file_type, url, description,
plan_count, plan_names, plans_truncated, plan_id_types, plan_market_types, http_status, check_method,
content_length, last_modified, etag, check_error`

A URL can serve thousands of plans, so each row carries `plan_count` plus up to 50 distinct plan
names (`plans_truncated` marks the cap). The full plan list stays in the stored TOC.

## Setup

```bash
python3.12 -m venv .venv && . .venv/bin/activate
pip install -e ".[dev]"          # add ,gcs for the GCS raw store
cp .env.example .env              # fill in values, never commit .env
set -a; . ./.env; set +a
```

Google access uses a service account (`GOOGLE_APPLICATION_CREDENTIALS`). Add the service account
as **Content manager** on the Shared Drive that holds `TIC_DRIVE_FOLDER_ID`. Service accounts have
no My Drive quota, so a My Drive folder will not work.

## Commands

```bash
tic-crawler discover                 # seed file + CMS Exchange PUF (+ root .txt from 2027-09-06)
tic-crawler verify-seeds             # HTTP-check seed URLs -> out/seed_verification.csv
tic-crawler crawl --payer bcbs-kansas
tic-crawler crawl --all              # skips payers crawled in the last 6 days
tic-crawler crawl --all --force      # ignore TOC hash and recrawl window
tic-crawler crawl --payer X --download-raw
tic-crawler sync-drive               # upload new/changed files, update the Sheet in place
tic-crawler report [--json]
tic-crawler audit-access             # robots.txt rules, bot walls, per-URL verdicts -> out/access_audit.json
tic-crawler export-dashboard         # dashboard JSON -> out/dashboard/
tic-crawler probe --name X --landing-url U [--toc-url T]   # check a requested payer
```

`crawl` always writes locally to `out/` and records state in `state/tic.sqlite`. `sync-drive`
uploads only files whose SHA-256 changed since the last sync.

## Crawl behavior

- Async httpx, 1 request/second per host (configurable), robots.txt honored (RFC 9309: 4xx = allow,
  5xx or unreachable = disallow), User-Agent `EverBetter-TiC-Crawler/1.0 (+TIC_CONTACT_EMAIL)`,
  retries with exponential backoff and `Retry-After`.
- TOCs are streamed to disk (capped by `max_toc_bytes`, default 2 GB) and parsed with ijson one
  `reporting_structure` item at a time. Header fields may come after the array; that is handled.
- Each item is validated against the vendored CMS schema; the header is validated at the end.
  Validation errors go to the manifest and run log; an invalid TOC is still cataloged.
- MRF URLs get HEAD only. If HEAD returns 403/405/501 (common for signed S3 links), one
  `GET Range: bytes=0-0` reads the size from `Content-Range`.
- Idempotent: if the TOC SHA-256 (of the decompressed JSON) matches the last run, nothing is
  re-checked or re-uploaded.
- Resumable: an interrupted `crawl` with the same arguments resumes the open run and skips payers
  already done.
- Payers whose TOC file name changes monthly use `toc_url` with `{date:%Y-%m}` placeholders, or
  `toc_link_pattern` (a regex applied to `landing_url`).

## Discovery

Each source is a `DiscoverySource` subclass and registers itself; adding a source means adding
one class.

1. `SeedFileSource`: `seeds/payers.yaml`, maintained by hand.
2. `ExchangePufSource`: CMS Exchange PUF (issuer names, HIOS IDs, states). URL in `config.toml`.
   Covers HealthCare.gov issuers only.
3. `RootTxtSource`: the payer root `.txt` file, active from 2027-09-06. The file name is
   configurable (`root_txt_filename`); confirm it against the final rule before relying on it.

Payers are merged by normalized legal name (and aliases) and by shared HIOS IDs; seed data wins.

## Dashboard

`dashboard/` is a static site (no build step) deployed on Vercel at https://tic-crawler.vercel.app
(project `tic-crawler`, root directory `dashboard`). It reads `out/dashboard/*.json` from the `data`
branch through raw.githubusercontent.com, so new crawl data shows up without a redeploy.

- Overview: payers, freshness (TOC `last_updated_on`), failures, MRF counts and bytes.
- Payer page: every MRF file with HTTP result and size, the plans in the TOC, and schema errors
  grouped by kind with example record positions.
- Errors: access errors (payer and file level) and record errors across payers.
- Access: robots.txt per host and what a polite crawler sees on each URL (allowed, robots-blocked,
  bot wall, HTTP error).
- Request a payer: opens a GitHub issue form; `.github/workflows/payer-request.yml` probes the URLs
  and replies on the issue with the result and a seed entry.
- User designs: `dashboard/designs/`, listed from `designs/manifest.json`.

## Data branch

Workflows restore `state/tic.sqlite` and `out/` from the `data` branch, run, then publish back
(`scripts/data_branch.sh`). Raw MRFs are never published; stored TOCs over 50 MB are dropped from
the branch.

## Scheduling

`.github/workflows/crawl.yml` runs weekly (Monday 06:17 UTC) and on demand; Drive sync runs only
when `TIC_DRIVE_FOLDER_ID` is set. Secrets: `GOOGLE_SERVICE_ACCOUNT_JSON`, `TIC_DRIVE_FOLDER_ID`,
`TIC_CONTACT_EMAIL`, optional `TIC_RAW_STORE`. `.github/workflows/dry-run.yml` crawls a fixed set of
payers on demand and monthly. State persists on the `data` branch.

## Tests

```bash
pytest --cov=tic_crawler
```

HTTP is recorded with respx; Google APIs are faked. `tests/fixtures/toc/cms-sample.json` is the
CMS example TOC.

## Diagrams

`docs/component.puml` and `docs/sequence.puml` (PlantUML).
