import copy

import pytest

from tic_crawler.parse import iter_toc
from tic_crawler.validate import TocValidator, load_schema


def run(doc_items, header, max_errors=50):
    v = TocValidator("2.2.1", max_errors=max_errors)
    for i, item in enumerate(doc_items):
        v.validate_item(i, item)
    v.validate_header(header)
    return v.finish()


def validate_bytes(data: bytes):
    v = TocValidator("2.2.1")
    for ev in iter_toc(data):
        if ev.kind == "item":
            v.validate_item(ev.index, ev.value)
        else:
            v.validate_header(ev.value)
    return v.finish()


HEADER = {
    "reporting_entity_name": "Acme", "reporting_entity_type": "health insurance issuer",
    "last_updated_on": "2026-10-01", "version": "2.0.0",
    "_has_reporting_structure": True, "_top_level_type": "start_map",
}
PLAN = {"plan_name": "A", "issuer_name": "Acme", "plan_id_type": "hios",
        "plan_id": "1234567890", "plan_market_type": "group"}
ITEM = {"reporting_plans": [PLAN],
        "in_network_files": [{"description": "in", "location": "https://x/a.json"}]}


def test_cms_sample_is_valid(cms_sample_bytes):
    r = validate_bytes(cms_sample_bytes)
    assert r.valid, r.errors
    assert r.file_version == "2.0.0"
    assert r.pinned_version == "2.2.1"
    assert r.item_count == 2


def test_valid_minimal():
    assert run([ITEM], HEADER).valid


@pytest.mark.parametrize("mutate,expect", [
    (lambda h: h.pop("version"), "'version' is a required property"),
    (lambda h: h.update(last_updated_on="10/01/2026"), "last_updated_on"),
    (lambda h: h.update(reporting_entity_name=""), "reporting_entity_name"),
])
def test_header_errors(mutate, expect):
    h = copy.deepcopy(HEADER)
    mutate(h)
    r = run([ITEM], h)
    assert not r.valid
    assert any(expect in e for e in r.errors), r.errors


@pytest.mark.parametrize("mutate,expect", [
    (lambda i: i["reporting_plans"][0].update(plan_id_type="npi"), "plan_id_type"),
    (lambda i: i["reporting_plans"][0].update(plan_market_type="medicare"), "plan_market_type"),
    (lambda i: i["reporting_plans"][0].update(plan_id_type="ein"), "plan_sponsor_name"),
    (lambda i: i["in_network_files"][0].update(location="http://x/a.json"), "https"),
    (lambda i: i.pop("in_network_files"), "is not valid under any of the given schemas"),
    (lambda i: i.update(reporting_plans=[]), "reporting_plans"),
    (lambda i: i["reporting_plans"][0].pop("issuer_name"), "issuer_name"),
])
def test_item_errors(mutate, expect):
    item = copy.deepcopy(ITEM)
    mutate(item)
    r = run([item], HEADER)
    assert not r.valid
    assert any(expect in e for e in r.errors), r.errors
    assert all(e.startswith("reporting_structure[0]") for e in r.errors)


def test_ein_with_sponsor_is_valid():
    item = copy.deepcopy(ITEM)
    item["reporting_plans"][0].update(plan_id_type="ein", plan_sponsor_name="Employer Co")
    assert run([item], HEADER).valid


def test_allowed_amount_only_is_valid():
    item = {"reporting_plans": [PLAN],
            "allowed_amount_file": {"description": "aa", "location": "https://x/aa.json"}}
    assert run([item], HEADER).valid


def test_duplicate_items_flagged():
    r = run([ITEM, copy.deepcopy(ITEM)], HEADER)
    assert any("duplicate" in e for e in r.errors)


def test_missing_and_empty_reporting_structure():
    h = {k: v for k, v in HEADER.items() if k != "_has_reporting_structure"}
    r = run([], h)
    assert any("'reporting_structure' is a required property" in e for e in r.errors)
    r = run([], HEADER)
    assert any("non-empty" in e for e in r.errors)


def test_reporting_structure_wrong_type():
    r = validate_bytes(b'{"reporting_entity_name":"a","reporting_entity_type":"b",'
                       b'"last_updated_on":"2026-01-01","version":"1","reporting_structure":"x"}')
    assert any("is not of type 'array'" in e for e in r.errors)
    assert not any("required property" in e for e in r.errors)


def test_top_level_not_object():
    r = validate_bytes(b"[1]")
    assert not r.valid
    assert r.errors == ["$: top level is not a JSON object"]


def test_error_cap():
    bad = {"reporting_plans": [{}], "in_network_files": [{}]}
    r = run([bad] * 1 + [dict(bad, x=i) for i in range(20)], HEADER, max_errors=5)
    assert len(r.errors) == 5
    assert r.error_count > 5


def test_numeric_version_recorded_as_string():
    h = dict(HEADER, version=2.0)
    r = run([ITEM], h)
    assert r.file_version == "2.0"
    assert any("version" in e for e in r.errors)


def test_unknown_schema_version():
    with pytest.raises(FileNotFoundError):
        load_schema("9.9.9")


def test_error_kinds_group_by_path_without_indices():
    items = []
    for i in range(3):
        it = copy.deepcopy(ITEM)
        it["reporting_plans"][0]["plan_id_type"] = "HIOS"
        items.append(dict(it, n=i))
    items.append(ITEM)
    r = run(items, HEADER)
    assert r.invalid_items == 3
    top = r.error_kinds[0]
    assert top["count"] == 3
    assert top["kind"].startswith("reporting_structure[*].reporting_plans[*].plan_id_type: 'HIOS'")
    assert top["examples"][0] == "reporting_structure[0].reporting_plans[0].plan_id_type"


def test_error_kinds_cap():
    v = TocValidator("2.2.1")
    v.validate_header(dict(HEADER))
    for i in range(250):
        v._add(f"x{i}: boom")
    kinds = v.finish().error_kinds
    assert len(kinds) == 201
    assert any(k["kind"] == "(other errors)" and k["count"] == 51 for k in kinds)
