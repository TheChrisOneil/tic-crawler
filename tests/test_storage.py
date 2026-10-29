from unittest.mock import MagicMock

import httplib2
import pytest
from googleapiclient.errors import HttpError

from tic_crawler.models import RegistryRow
from tic_crawler.storage import (
    DriveAdapter,
    GcsRawStore,
    LocalBackend,
    LocalRawStore,
    SheetRegistry,
    execute_with_backoff,
    make_raw_store,
)


def http_error(status, reason=""):
    resp = httplib2.Response({"status": status})
    return HttpError(resp, f'{{"error": {{"errors": [{{"reason": "{reason}"}}]}}}}'.encode())


class Req:
    def __init__(self, outcomes):
        self.outcomes = list(outcomes)
        self.calls = 0

    def execute(self):
        self.calls += 1
        o = self.outcomes.pop(0)
        if isinstance(o, Exception):
            raise o
        return o


def test_backoff_retries_429_and_rate_limit_403():
    sleeps = []
    req = Req([http_error(429), http_error(403, "userRateLimitExceeded"), {"ok": 1}])
    assert execute_with_backoff(req, sleep=sleeps.append) == {"ok": 1}
    assert req.calls == 3 and len(sleeps) == 2


def test_backoff_does_not_retry_permission_403():
    req = Req([http_error(403, "insufficientFilePermissions")])
    with pytest.raises(HttpError):
        execute_with_backoff(req, sleep=lambda s: None)
    assert req.calls == 1


def test_backoff_gives_up():
    req = Req([http_error(503)] * 3)
    with pytest.raises(HttpError):
        execute_with_backoff(req, max_retries=2, sleep=lambda s: None)


class FakeFiles:
    """Minimal in-memory stand-in for drive.files()."""

    def __init__(self):
        self.items = {}  # id -> {name, parents, mimeType}
        self.n = 0
        self.updates = []

    def list(self, q, **kw):
        def match(item):
            parent = q.split("'")[1]
            name = q.split("name = '")[1].split("'")[0]
            ok = parent in item["parents"] and item["name"] == name
            if "mimeType = '" in q:
                ok = ok and item["mimeType"] == q.split("mimeType = '")[1].split("'")[0]
            return ok
        assert kw["supportsAllDrives"] and kw["includeItemsFromAllDrives"]
        return Req([{"files": [{"id": i, "name": v["name"]} for i, v in self.items.items() if match(v)]}])

    def create(self, body, fields, supportsAllDrives, media_body=None):
        assert supportsAllDrives
        self.n += 1
        fid = f"id{self.n}"
        self.items[fid] = {"name": body["name"], "parents": body["parents"],
                           "mimeType": body.get("mimeType", "file")}
        return Req([{"id": fid}])

    def update(self, fileId, media_body, fields, supportsAllDrives):
        self.updates.append(fileId)
        return Req([{"id": fileId}])


@pytest.fixture
def drive():
    svc = MagicMock()
    files = FakeFiles()
    svc.files.return_value = files
    return DriveAdapter(svc, "ROOT", sleep=lambda s: None), files


def test_drive_put_creates_folders_then_updates(drive, tmp_path):
    adapter, files = drive
    f = tmp_path / "a.csv"
    f.write_text("x")
    first = adapter.put_file("manifests/acme/2026-10-29.csv", f)
    folders = {v["name"]: k for k, v in files.items.items() if v["mimeType"].endswith("folder")}
    assert set(folders) == {"manifests", "acme"}
    assert files.items[first]["parents"] == [folders["acme"]]
    # A fresh adapter finds existing folders and updates the file in place.
    adapter2 = DriveAdapter(adapter.drive, "ROOT", sleep=lambda s: None)
    second = adapter2.put_file("manifests/acme/2026-10-29.csv", f)
    assert second == first and files.updates == [first]
    assert len(files.items) == 3


def test_drive_refuses_raw(drive, tmp_path):
    adapter, _ = drive
    with pytest.raises(ValueError):
        adapter.put_file("raw/acme/x.json", tmp_path)


def test_drive_requires_folder():
    with pytest.raises(ValueError):
        DriveAdapter(MagicMock(), "")


def test_find_or_create_sheet(drive):
    adapter, files = drive
    sid = adapter.find_or_create_sheet("TiC Payer Registry")
    assert files.items[sid]["mimeType"] == "application/vnd.google-apps.spreadsheet"
    assert adapter.find_or_create_sheet("TiC Payer Registry") == sid


def row(pid, n=0):
    return RegistryRow(pid, pid.upper(), "", "", "", "", "", n, 0, "TRUE", "")


def test_sheet_upsert_in_place():
    sheets = MagicMock()
    sheets.spreadsheets().get.return_value = Req([{"sheets": [{"properties": {"title": "Registry"}}]}])
    values = sheets.spreadsheets().values()
    values.get.return_value = Req([{"values": [["payer_id"], ["b"], [], ["a"]]}])
    values.batchUpdate.return_value = Req([{}])
    res = SheetRegistry(sheets, "SID", sleep=lambda s: None).upsert([row("a", 5), row("c"), row("b")])
    assert res == {"updated": 2, "appended": 1}
    data = values.batchUpdate.call_args.kwargs["body"]["data"]
    ranges = {d["values"][0][0]: d["range"] for d in data}
    assert ranges["payer_id"] == "'Registry'!A1"
    assert ranges["a"] == "'Registry'!A4"
    assert ranges["b"] == "'Registry'!A2"
    assert ranges["c"] == "'Registry'!A5"
    assert data[1]["values"][0][7] == 5


def test_sheet_upsert_empty_sheet():
    sheets = MagicMock()
    sheets.spreadsheets().get.return_value = Req([{"sheets": []}])
    values = sheets.spreadsheets().values()
    values.get.return_value = Req([{}])
    values.batchUpdate.return_value = Req([{}])
    SheetRegistry(sheets, "SID", sleep=lambda s: None).upsert([row("a")])
    data = values.batchUpdate.call_args.kwargs["body"]["data"]
    assert data[1]["range"] == "'Sheet1'!A2"


def test_make_raw_store(tmp_path):
    assert isinstance(make_raw_store(str(tmp_path)), LocalRawStore)
    for bad in ("https://drive.google.com/drive/folders/x", "drive:abc", "gdrive:x"):
        with pytest.raises(ValueError):
            make_raw_store(bad)
    with pytest.raises(ValueError):
        make_raw_store("")


def test_gcs_store_uses_prefix():
    client = MagicMock()
    store = GcsRawStore("bucket", "/pre/", client=client)
    store.open_writer("raw/a/b")
    client.bucket.assert_called_with("bucket")
    client.bucket().blob.assert_called_with("pre/raw/a/b")


def test_local_backend(tmp_path):
    src = tmp_path / "s.csv"
    src.write_text("hi")
    dest = LocalBackend(tmp_path / "root").put_file("a/b.csv", src)
    assert open(dest).read() == "hi"
