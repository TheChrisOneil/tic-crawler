"""Print a short summary of a price report card (used in CI annotations). Usage: report_card_summary.py <card.json>"""
import json
import sys

c = json.load(open(sys.argv[1]))
print(c["payer_name"], "| hospitals", len(c["hospitals"]), "| excluded", c["excluded_rows"], "|", "; ".join(c["medicare_releases"]))
for k in c["codes"]:
    print(f'{k["code"]} vs {k["medicare_code"]} {k["medicare_source"]} ${k["medicare_amount"]:,.2f}: median {k["median_multiple"]}x, '
          f'{k["min_multiple"]}-{k["max_multiple"]}x, {k["hospitals"]} hospitals')
flat = [h["name"] for h in c["hospitals"] if h.get("flat_codes")]
print("flat-rate hospitals:", flat)
