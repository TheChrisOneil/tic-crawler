"""Storage backends (Strategy) with Drive, Sheets and GCS adapters.

Drive holds the catalog only: TOCs, manifests, run logs and the registry Sheet.
Raw in-network and allowed-amount files never go to Drive.
"""

from __future__ import annotations

import random
import shutil
import time
from abc import ABC, abstractmethod
from collections.abc import Callable
from pathlib import Path, PurePosixPath
from typing import Any

from .models import RegistryRow

FOLDER_MIME = "application/vnd.google-apps.folder"
SHEET_MIME = "application/vnd.google-apps.spreadsheet"
MIME_BY_SUFFIX = {".gz": "application/gzip", ".csv": "text/csv", ".json": "application/json"}


class StorageBackend(ABC):
    """Where catalog files go. Paths are POSIX-style relative paths like toc/aetna/2026-10-29.json.gz."""

    @abstractmethod
    def put_file(self, rel_path: str, local_path: Path) -> str:
        """Store a file, replacing an existing one at the same path. Returns a location id."""


class LocalBackend(StorageBackend):
    def __init__(self, root: str | Path):
        self.root = Path(root)

    def put_file(self, rel_path: str, local_path: Path) -> str:
        dest = self.root / rel_path
        if dest.resolve() != Path(local_path).resolve():
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(local_path, dest)
        return str(dest)


# Google API helpers -----------------------------------------------------------------------

def _is_retryable(exc: Exception) -> bool:
    from googleapiclient.errors import HttpError

    if not isinstance(exc, HttpError):
        return False
    status = exc.resp.status
    if status in (429, 500, 502, 503, 504):
        return True
    if status == 403:
        content = exc.content.decode("utf-8", "replace") if isinstance(exc.content, bytes) else ""
        text = f"{exc} {content}".lower()
        return "ratelimitexceeded" in text or "userratelimitexceeded" in text or "quota" in text
    return False


def execute_with_backoff(request: Any, max_retries: int = 6, base: float = 1.0,
                         sleep: Callable[[float], None] = time.sleep) -> Any:
    """Run a googleapiclient request, backing off on 403 rate-limit and 429/5xx responses."""
    for attempt in range(max_retries + 1):
        try:
            return request.execute()
        except Exception as e:
            if attempt == max_retries or not _is_retryable(e):
                raise
            delay = min(64.0, base * 2**attempt) + random.uniform(0, 1)
            sleep(delay)
    raise RuntimeError("unreachable")


def build_google_services(credentials_path: str):
    """Service-account clients for Drive v3 and Sheets v4."""
    from google.oauth2 import service_account
    from googleapiclient.discovery import build

    scopes = ["https://www.googleapis.com/auth/drive", "https://www.googleapis.com/auth/spreadsheets"]
    creds = service_account.Credentials.from_service_account_file(credentials_path, scopes=scopes)
    drive = build("drive", "v3", credentials=creds, cache_discovery=False)
    sheets = build("sheets", "v4", credentials=creds, cache_discovery=False)
    return drive, sheets


def _q(value: str) -> str:
    return value.replace("\\", "\\\\").replace("'", "\\'")


class DriveAdapter(StorageBackend):
    """Adapter from StorageBackend to the Drive v3 API. Works in Shared Drives."""

    def __init__(self, drive_service: Any, root_folder_id: str,
                 sleep: Callable[[float], None] = time.sleep):
        if not root_folder_id:
            raise ValueError("TIC_DRIVE_FOLDER_ID is not set")
        self.drive = drive_service
        self.root = root_folder_id
        self._folders: dict[str, str] = {"": root_folder_id}
        self._sleep = sleep

    def _exec(self, req: Any) -> Any:
        return execute_with_backoff(req, sleep=self._sleep)

    def _find(self, parent: str, name: str, mime: str | None = None) -> str | None:
        q = f"'{_q(parent)}' in parents and name = '{_q(name)}' and trashed = false"
        if mime:
            q += f" and mimeType = '{mime}'"
        res = self._exec(self.drive.files().list(
            q=q, fields="files(id, name)", pageSize=10,
            supportsAllDrives=True, includeItemsFromAllDrives=True,
        ))
        files = res.get("files", [])
        return files[0]["id"] if files else None

    def folder_id(self, rel_dir: str) -> str:
        rel_dir = rel_dir.strip("/")
        if rel_dir in self._folders:
            return self._folders[rel_dir]
        parent_rel, _, name = rel_dir.rpartition("/")
        parent = self.folder_id(parent_rel)
        fid = self._find(parent, name, FOLDER_MIME)
        if fid is None:
            fid = self._exec(self.drive.files().create(
                body={"name": name, "mimeType": FOLDER_MIME, "parents": [parent]},
                fields="id", supportsAllDrives=True,
            ))["id"]
        self._folders[rel_dir] = fid
        return fid

    def put_file(self, rel_path: str, local_path: Path) -> str:
        from googleapiclient.http import MediaFileUpload

        p = PurePosixPath(rel_path)
        if p.parts and p.parts[0] == "raw":
            raise ValueError("raw MRF files are never stored in Drive")
        parent = self.folder_id(str(p.parent) if str(p.parent) != "." else "")
        mime = MIME_BY_SUFFIX.get(p.suffix, "application/octet-stream")
        media = MediaFileUpload(str(local_path), mimetype=mime, resumable=True)
        existing = self._find(parent, p.name)
        if existing:
            res = self._exec(self.drive.files().update(
                fileId=existing, media_body=media, fields="id", supportsAllDrives=True))
        else:
            res = self._exec(self.drive.files().create(
                body={"name": p.name, "parents": [parent]}, media_body=media,
                fields="id", supportsAllDrives=True))
        return res["id"]

    def find_or_create_sheet(self, name: str) -> str:
        sid = self._find(self.root, name, SHEET_MIME)
        if sid:
            return sid
        return self._exec(self.drive.files().create(
            body={"name": name, "mimeType": SHEET_MIME, "parents": [self.root]},
            fields="id", supportsAllDrives=True,
        ))["id"]


class SheetRegistry:
    """The TiC Payer Registry Sheet: one row per payer, updated in place by payer_id."""

    def __init__(self, sheets_service: Any, spreadsheet_id: str, tab: str = "Sheet1",
                 sleep: Callable[[float], None] = time.sleep):
        self.sheets = sheets_service
        self.sid = spreadsheet_id
        self.tab = tab
        self._sleep = sleep

    def _exec(self, req: Any) -> Any:
        return execute_with_backoff(req, sleep=self._sleep)

    def _first_tab(self) -> str:
        meta = self._exec(self.sheets.spreadsheets().get(
            spreadsheetId=self.sid, fields="sheets.properties.title"))
        sheets = meta.get("sheets", [])
        return sheets[0]["properties"]["title"] if sheets else self.tab

    def upsert(self, rows: list[RegistryRow]) -> dict[str, int]:
        tab = self._first_tab()
        values = self.sheets.spreadsheets().values()
        current = self._exec(values.get(spreadsheetId=self.sid, range=f"'{tab}'!A:A")).get(
            "values", [])
        ids = [r[0] if r else "" for r in current]
        header = list(RegistryRow.COLUMNS)
        data = [{"range": f"'{tab}'!A1", "values": [header]}]
        index = {pid: i + 1 for i, pid in enumerate(ids) if i > 0 and pid}
        next_row = max(len(ids), 1) + 1
        updated = appended = 0
        for row in rows:
            vals = ["" if v is None else v for v in row.values()]
            if row.payer_id in index:
                r = index[row.payer_id]
                updated += 1
            else:
                r = next_row
                next_row += 1
                index[row.payer_id] = r
                appended += 1
            data.append({"range": f"'{tab}'!A{r}", "values": [vals]})
        self._exec(values.batchUpdate(
            spreadsheetId=self.sid,
            body={"valueInputOption": "RAW", "data": data},
        ))
        return {"updated": updated, "appended": appended}


# Raw MRF stores ---------------------------------------------------------------------------

class RawStore(ABC):
    @abstractmethod
    def open_writer(self, rel_path: str):
        """Return a binary file-like object; closing it commits the object."""


class LocalRawStore(RawStore):
    def __init__(self, root: str | Path):
        self.root = Path(root)

    def open_writer(self, rel_path: str):
        dest = self.root / rel_path
        dest.parent.mkdir(parents=True, exist_ok=True)
        return dest.open("wb")


class GcsRawStore(RawStore):
    """Adapter over google-cloud-storage (install the `gcs` extra)."""

    def __init__(self, bucket: str, prefix: str = "", client: Any = None):
        if client is None:
            from google.cloud import storage  # type: ignore[import-not-found]

            client = storage.Client()
        self.bucket = client.bucket(bucket)
        self.prefix = prefix.strip("/")

    def open_writer(self, rel_path: str):
        name = f"{self.prefix}/{rel_path}" if self.prefix else rel_path
        return self.bucket.blob(name).open("wb")


def make_raw_store(spec: str) -> RawStore:
    if not spec:
        raise ValueError("TIC_RAW_STORE is not set; raw download needs gcs://bucket or a local path")
    low = spec.lower()
    if "drive.google" in low or low.startswith(("drive:", "gdrive:")):
        raise ValueError("raw MRF files must never be stored in Google Drive")
    if low.startswith("gcs://") or low.startswith("gs://"):
        rest = spec.split("://", 1)[1]
        bucket, _, prefix = rest.partition("/")
        return GcsRawStore(bucket, prefix)
    return LocalRawStore(spec)
