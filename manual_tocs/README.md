# Manually saved TOCs

For payers whose site blocks automated access (for example a Cloudflare challenge), save the
table-of-contents file from a browser here as `<payer_id>.json` or `<payer_id>.json.gz`, then set
`toc_file: manual_tocs/<payer_id>.json.gz` and `verified_by: manual` on the seed entry.
The crawler parses and validates it like any other TOC and records `access_status: manual`.
MRF URLs inside it are still HEAD-checked politely. Do not save raw in-network files here.
