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
  unchanged since v2.0.0. Schemas are vendored in `src/tic_crawler/schemas/v2.2.1/`; to move to a
  new version, vendor the new folder and change `[schema].version` in `config.toml`.
- CMS-9882-F (finalized 2026-10): quarterly updates from 2027-03-06; footer link and root `.txt`
  file from 2027-09-06. The TOC stays optional, so some payers have none.

## Design
- Layout: `discovery, fetch, parse, validate, storage, cli` as specified, plus `models, config,
  state, pipeline` to keep modules focused.
- Patterns: Strategy for discovery sources (`DiscoverySource`, auto-registered on subclassing)
  and storage backends (`StorageBackend`, `RawStore`); Adapter for Drive, Sheets and GCS;
  Template Method for the crawl (`CrawlPipeline.run` fixes the order; `TocCrawlPipeline`,
  `RawDownloadPipeline` fill in steps).
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

## Seeds
- `seeds/payers.yaml` entries carry `verified_by` (`page_fetch`, `http`, `none`). Do not invent
  URLs; check them with `tic-crawler verify-seeds` and record the result.
- One entry per TOC when a payer publishes several.

## Known gaps
- Exchange PUF URL and column names in `config.toml`/`ExchangePufSource` need checking against
  the live CMS file (the build sandbox could not reach cms.gov).
- Root `.txt` file name and format are placeholders until checked against the final rule text.
- Several national carriers (UHC, Aetna, Humana, Anthem, Cigna) render TOC links with JavaScript
  or block bots; their seeds need a TOC URL found by hand.
