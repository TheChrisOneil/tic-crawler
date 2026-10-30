# CLAUDE.md: decisions for tic-crawler

Read this before changing the code.

## Hard constraints
- No LLM or inference calls at runtime. The tool is deterministic.
- Never put raw in-network or allowed-amount files in Google Drive (750 GB/day upload limit; files
  reach TB). `DriveAdapter.put_file` rejects `raw/` paths and `make_raw_store` rejects Drive targets.
- Never commit credentials. Keys come from `GOOGLE_APPLICATION_CREDENTIALS`; `.env` and key files
  are gitignored.
- No member data. Only public MRF metadata.
- Stop and ask the owner before any change that needs new cloud resources, IAM changes, or more
  than 10 GB of data in a run (`max_run_bytes`).

## Spec
- CMS price-transparency-guide v2.2.1 (commit 66898bb, 2026-04). The TOC schema is structurally
  unchanged since v2.0.0. Schemas are embedded from `internal/schemas/v2.2.1/`; to move to a
  new version, add the new folder there and change `[schema].version` in `config.toml`.
- CMS-9882-F (finalized 2026-10): quarterly updates from 2027-03-06; footer link and root `.txt`
  file from 2027-09-06. The TOC stays optional, so some payers have none.

## Design
- Language: Go (one static binary, `cmd/tic-crawler`, cobra). It replaced the Python version in
  October 2026 after a side-by-side crawl matched; keep outputs (manifests, state JSON, dashboard
  JSON, validation messages) byte-compatible with what the dashboard and data branch already hold.
- Layout: `internal/` packages `discovery, fetch, robots, toc, validate, storage, state, pipeline,
  model, config, audit, export`, plus `extract, reference, quality` for the price and quality pilot.
- Patterns: Strategy for discovery sources (`discovery.Source`, added with `discovery.Register`)
  and storage backends (`storage.Backend`, `RawStore`); Adapter for Drive, Sheets and GCS;
  Template Method for the crawl (`pipeline.Run` fixes the order; `TocCrawl`, `RawDownload` and
  `Probe` fill in the `Steps`).
- Validation messages follow Python jsonschema 4.x wording and order (`internal/validate/pymsg.go`),
  and the `uri` format is not checked, so error kinds stay comparable across runs.
- Streaming validation: the TOC schema is split into an item schema and a header schema so
  multi-GB TOCs never load into memory. `uniqueItems` is checked with item hashes.
- Manifest grain: one row per unique MRF URL, with plan_count and up to 50 plan names.
- TOC hash: SHA-256 of the decompressed JSON, so a gzip re-wrap does not look like a change.
- HEAD fallback: 403/405/501 on HEAD triggers one `GET Range: bytes=0-0`.
- robots.txt per RFC 9309. A disallowed TOC fails the payer with `robots_disallowed`.
- `crawl` writes locally; `sync-drive` uploads. Drive must be a Shared Drive (service accounts have
  no My Drive quota). The Sheet is found or created by name in the root folder and updated in
  place by `payer_id`.
- Weekly schedule with a 6-day recrawl window; staleness threshold is 35 days before 2027-03-06
  and 100 days after.

## Dashboard and data branch
- `dashboard/` is plain HTML/JS on Vercel (team TechNexusClarity, project `tic-crawler`, root
  `dashboard`). It reads JSON from the `data` branch at runtime; keep it build-free.
- `export-dashboard` (`internal/export`) is the only producer of `out/dashboard/`. Change the JSON
  shape there and in `dashboard/app.js` together. Workflows copy it to `main:dashboard/data/`,
  which Vercel serves (the repo can be private).
- robots.txt: retried with backoff; only a persistent 5xx/429/network failure counts as disallow
  (RFC 9309), and the reason is recorded so the dashboard can tell "unreachable" from "disallowed".
- Access outcomes are typed (`PayerResult.access_status`): ok, manual, robots_disallow,
  robots_unreachable, waf_blocked, http_error, network_error, no_toc_url, too_large. Unreachable
  robots.txt, network errors, 408/429 and 5xx set status `deferred` (retried next run, previous
  counts kept); explicit disallow and WAF blocks fail. Never try to get past a WAF or challenge;
  use a hand-saved TOC (`toc_file`, `verified_by: manual`) instead.
- Payer requests are GitHub issues (template `payer-request.yml`); the probe workflow only reads
  public URLs and comments. It never edits seeds.

## Seeds
- `seeds/payers.yaml` entries carry `verified_by` (`page_fetch`, `http`, `none`). Do not invent
  URLs; check them with `tic-crawler verify-seeds` and record the result.
- One entry per TOC when a payer publishes several.

## Price and quality pilot
- `extract-rates` keeps only `config/codes.yaml` codes; a run stops at `max_run_bytes`. Prices are
  summarized per code and billing class, dollar types only (percentage and per diem are counted, not
  averaged).
- Crosswalk NPI -> CCN records `method` (address, name_zip, affiliation) on every link. The NPPES
  bulk page was in maintenance in October 2026; the NPI Registry API is the fallback (3,000 lookups
  per run).
- The rating method waits for owner approval before any rating code is written.

## Known gaps
- Exchange PUF URL and column names in `config.toml`/`ExchangePufSource` need checking against
  the live CMS file (the build sandbox could not reach cms.gov).
- Root `.txt` file name and format are placeholders until checked against the final rule text.
- Several national carriers (UHC, Aetna, Humana, Anthem, Cigna) render TOC links with JavaScript
  or block bots; their seeds need a TOC URL found by hand.
