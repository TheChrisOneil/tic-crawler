"""Turn a GitHub issue-form body (### Heading / value blocks) into step outputs.

Reads BODY from the environment and prints key=value lines. Only https URLs are passed on.
"""

import os
import re

LABELS = {
    "payer legal name": "payer_name",
    "transparency in coverage landing page url": "landing_url",
    "table of contents (index) json url, if known": "toc_url",
}

body = os.environ.get("BODY", "")
values = {}
for block in re.split(r"^### ", body, flags=re.M)[1:]:
    head, _, rest = block.partition("\n")
    key = LABELS.get(head.strip().lower())
    if not key:
        continue
    value = rest.strip().splitlines()[0].strip() if rest.strip() else ""
    if value == "_No response_":
        value = ""
    if key.endswith("_url") and value and not value.lower().startswith("https://"):
        value = ""
    value = re.sub(r"[\r\n\"`$]", "", value)[:500]
    values[key] = value
for key in LABELS.values():
    print(f"{key}={values.get(key, '')}")
