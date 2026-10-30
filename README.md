# tic-crawler

Deterministic Go tool (one static binary) that finds U.S. health plan and issuer Transparency in Coverage (TiC)
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
go build -o bin/tic-crawler ./cmd/tic-crawler    # Go 1.24+; no cgo (pure-Go SQLite)
cp .env.example .env                              # fill in values, never commit .env
set -a; . ./.env; set +a
```

The examples below call the binary `tic-crawler`; use `bin/tic-crawler` or put `bin/` on your `PATH`.

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

Price and quality pilot (see `.github/workflows/pilot-quality.yml`):

```bash
tic-crawler reference provider-data            # CMS Care Compare hospital files for [reference].states
tic-crawler reference nppes                    # NPPES bulk file filtered to those states
tic-crawler extract-rates --payer P --toc-url T --codes config/codes.yaml --max-files 1
tic-crawler reference npi-lookup --payer P     # NPI Registry API for facility NPIs NPPES lacks
tic-crawler join-quality --payer P             # NPI -> CCN crosswalk, prices + CMS measures
```

`extract-rates` streams an in-network file and keeps only the listed billing codes (and optional
NPI/TIN lists); a run never downloads more than `max_run_bytes` (10 GB).

`crawl` always writes locally to `out/` and records state in `state/tic.sqlite`. `sync-drive`
uploads only files whose SHA-256 changed since the last sync.

## Crawl behavior

- Concurrent HTTP (`net/http`), 1 request/second per host (configurable), robots.txt honored (RFC 9309: 4xx = allow,
  5xx or unreachable = disallow), User-Agent `EverBetter-TiC-Crawler/1.0 (+TIC_CONTACT_EMAIL)`,
  retries with exponential backoff and `Retry-After`.
- TOCs are streamed to disk (capped by `max_toc_bytes`, default 2 GB) and parsed with a streaming JSON
  decoder one `reporting_structure` item at a time. Header fields may come after the array; that is handled.
- Each item is validated against the embedded CMS schema (`internal/schemas/`), with error
  messages worded as Python jsonschema words them; the header is validated at the end.
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

Each source implements `discovery.Source` and is added with `discovery.Register`; adding a source
means adding one type (`internal/discovery`).

1. `SeedFile`: `seeds/payers.yaml`, maintained by hand.
2. `ExchangePUF`: CMS Exchange PUF (issuer names, HIOS IDs, states). URL in `config.toml`.
   Covers HealthCare.gov issuers only.
3. `RootTxt`: the payer root `.txt` file, active from 2027-09-06. The file name is
   configurable (`root_txt_filename`); confirm it against the final rule before relying on it.

Payers are merged by normalized legal name (and aliases) and by shared HIOS IDs; seed data wins.

## Dashboard

`dashboard/` is a static site (no build step) deployed on Vercel at https://tic-crawler.vercel.app
(project `tic-crawler`, root directory `dashboard`). It reads `dashboard/data/*.json` from its own
origin. Each workflow run copies `out/dashboard/` to `main:dashboard/data/`
(`scripts/data_branch.sh dashboard`), which triggers a Vercel deploy; this works with a private repo.

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
go test -cover ./...
```

HTTP uses `httptest` servers or a fake `RoundTripper`; Google APIs are faked. `testdata/toc/cms-sample.json`
is the CMS example TOC. CI fails if extraction, crosswalk, validation or the crawl pipeline drop
below 80% coverage.

The Go version replaced an earlier Python version in October 2026. Before the switch, both crawled
the same payers and their manifests, registry and dashboard JSON were compared. They matched
except for one intended difference: Go's HEAD requests do not ask for gzip, so servers that
compress report the real file size.

## Diagrams

`docs/component.puml` and `docs/sequence.puml` (PlantUML).
