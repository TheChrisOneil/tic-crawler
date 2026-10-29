import gzip
import json

import pytest

from tic_crawler.parse import (
    MrfCollector,
    is_gzip,
    iter_toc,
    open_json,
    sha256_of_json,
    write_gzip_copy,
)


def events(data: bytes):
    return list(iter_toc(data))


def test_cms_sample_items_and_header(cms_sample_bytes, cms_sample):
    evs = events(cms_sample_bytes)
    items = [e for e in evs if e.kind == "item"]
    header = evs[-1]
    assert header.kind == "header"
    assert len(items) == len(cms_sample["reporting_structure"])
    assert items[0].value == cms_sample["reporting_structure"][0]
    assert header.value["reporting_entity_name"] == "medicare"
    assert header.value["version"] == "2.0.0"
    assert header.value["_has_reporting_structure"] is True
    assert header.value["_top_level_type"] == "start_map"


def test_header_fields_after_array_are_collected():
    doc = {"reporting_structure": [{"reporting_plans": []}], "version": "2.2.1",
           "last_updated_on": "2026-10-01"}
    header = events(json.dumps(doc).encode())[-1].value
    assert header["version"] == "2.2.1"
    assert header["last_updated_on"] == "2026-10-01"


def test_scalar_and_array_items_are_yielded():
    evs = events(b'{"reporting_structure": [1, [2, 3], {"a": {"b": []}}]}')
    vals = [e.value for e in evs if e.kind == "item"]
    assert vals == [1, [2, 3], {"a": {"b": []}}]
    assert [e.index for e in evs if e.kind == "item"] == [0, 1, 2]


def test_reporting_structure_wrong_type_is_noted():
    h = events(b'{"reporting_structure": "nope"}')[-1].value
    assert h["reporting_structure"] == "nope"
    assert h["_has_reporting_structure"] is False
    h = events(b'{"reporting_structure": {"x": 1}}')[-1].value
    assert h["reporting_structure"] == {}


def test_nested_objects_at_top_level_are_ignored():
    h = events(b'{"meta": {"version": "x"}, "version": "1"}')[-1].value
    assert h["version"] == "1"
    assert "meta" not in h


def test_top_level_array():
    h = events(b"[1,2]")[-1].value
    assert h["_top_level_type"] == "start_array"


def test_gzip_roundtrip_and_hash_stable(tmp_path, cms_sample_bytes):
    plain = tmp_path / "toc.json"
    plain.write_bytes(cms_sample_bytes)
    gz = tmp_path / "toc.json.gz"
    gz.write_bytes(gzip.compress(cms_sample_bytes))
    assert not is_gzip(plain) and is_gzip(gz)
    assert sha256_of_json(plain) == sha256_of_json(gz)
    with open_json(gz) as fh:
        assert fh.read() == cms_sample_bytes


def test_write_gzip_copy_is_deterministic(tmp_path, cms_sample_bytes):
    src = tmp_path / "a.json"
    src.write_bytes(cms_sample_bytes)
    a, b = tmp_path / "x" / "a.gz", tmp_path / "y" / "b.gz"
    write_gzip_copy(src, a)
    write_gzip_copy(src, b)
    assert a.read_bytes() == b.read_bytes()
    assert gzip.decompress(a.read_bytes()) == cms_sample_bytes
    # already gzipped input is copied as is
    c = tmp_path / "c.gz"
    write_gzip_copy(a, c)
    assert c.read_bytes() == a.read_bytes()


def test_collector_dedupes_urls_and_plans(cms_sample):
    col = MrfCollector()
    for item in cms_sample["reporting_structure"]:
        col.add_item(item)
    types = {e.url: e.file_type for e in col.entries.values()}
    assert types["https://www.some_site.com/files/in-network-file-123456.json"] == "in-network"
    assert types["https://www.some_site.com/files/allowed-amount-file-987665.json"] == "allowed-amount"
    e = col.entries["https://www.some_site.com/files/in-network-file-123456.json"]
    assert e.plan_count == 2
    assert e.plan_names == ["medicaid", "medicare"]
    assert e.plan_id_types == ["hios"]
    assert len(col) == len(types)


def test_collector_shared_url_across_items_and_cap():
    plans = [{"plan_name": f"P{i}", "plan_id_type": "ein", "plan_id": str(i),
              "plan_market_type": "group"} for i in range(5)]
    item = {"reporting_plans": plans,
            "in_network_files": [{"description": "d", "location": "https://h/f.json"}]}
    col = MrfCollector(name_cap=3)
    col.add_item(item)
    col.add_item(item)  # same plans again: not double counted
    col.add_item({"reporting_plans": [{"plan_name": "Z", "plan_id": "z"}],
                  "in_network_files": [{"description": "d", "location": "https://h/f.json"}]})
    e = col.entries["https://h/f.json"]
    assert e.plan_count == 6
    assert e.plan_names == ["P0", "P1", "P2"]
    assert e.plans_truncated is True
    assert e.descriptions == ["d"]


@pytest.mark.parametrize("item", [
    "str",
    {"reporting_plans": "x", "in_network_files": [{"location": ""}, "bad", {"location": 5}]},
    {"allowed_amount_file": ["not", "object"]},
])
def test_collector_ignores_malformed(item):
    col = MrfCollector()
    col.add_item(item)
    assert len(col) == 0
