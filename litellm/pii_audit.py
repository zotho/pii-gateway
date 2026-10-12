"""Append-only, hash-chained audit log (docs/OBSERVABILITY.md).

Events never carry real values: only handles, types, counts, paths and keyed fingerprints.
Multiple processes (LiteLLM bridge, demo reveal API) append to the same file; an fcntl lock
serialises the read-last-hash / append step so the chain stays linear.
"""
from __future__ import annotations

import fcntl
import hashlib
import hmac
import json
import os
import secrets
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterator

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_PATH = Path(os.environ.get("PII_AUDIT_LOG", ROOT / ".run" / "audit" / "audit.jsonl"))
GENESIS = "0" * 64


def _canonical(ev: dict) -> bytes:
    return json.dumps(ev, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def _load_key() -> bytes:
    k = os.environ.get("PII_AUDIT_KEY")
    if k:
        return k.encode()
    f = DEFAULT_PATH.parent / "fp.key"  # dev only; production: KMS / secret manager
    f.parent.mkdir(parents=True, exist_ok=True)
    if not f.exists():
        f.write_text(secrets.token_hex(32))
        f.chmod(0o600)
    return f.read_text().strip().encode()


class AuditLog:
    def __init__(self, path: Path | str = DEFAULT_PATH, key: bytes | None = None) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._key = key

    @property
    def key(self) -> bytes:
        if self._key is None:
            self._key = _load_key()
        return self._key

    def fp(self, entity_type: str, value: str) -> str:
        """Keyed fingerprint of a real value; lets an investigator holding the key search the log."""
        msg = f"{entity_type}|{value.strip().casefold()}".encode()
        return hmac.new(self.key, msg, hashlib.sha256).hexdigest()[:16]

    def append(self, event: str, **fields: Any) -> dict:
        ev = {"ts": datetime.now(timezone.utc).isoformat(timespec="milliseconds"), "event": event, **fields}
        with open(self.path, "a+b") as f:
            fcntl.flock(f, fcntl.LOCK_EX)
            try:
                ev["prev"] = self._last_hash(f)
                ev["hash"] = hashlib.sha256(ev["prev"].encode() + _canonical(ev)).hexdigest()
                f.seek(0, os.SEEK_END)
                f.write(_canonical(ev) + b"\n")
                f.flush()
                os.fsync(f.fileno())
            finally:
                fcntl.flock(f, fcntl.LOCK_UN)
        return ev

    @staticmethod
    def _last_hash(f) -> str:
        f.seek(0, os.SEEK_END)
        size = f.tell()
        if size == 0:
            return GENESIS
        back = min(size, 65536)
        f.seek(size - back)
        last = f.read(back).rstrip(b"\n").rsplit(b"\n", 1)[-1]
        return json.loads(last)["hash"]

    def read(self, session: str | None = None, since: str | None = None) -> Iterator[dict]:
        if not self.path.exists():
            return
        with open(self.path, "rb") as f:
            for line in f:
                if not line.strip():
                    continue
                ev = json.loads(line)
                if session and ev.get("session") != session:
                    continue
                if since and ev["ts"] <= since:
                    continue
                yield ev

    def verify(self) -> tuple[bool, int, str]:
        """(ok, events_checked, message). Detects edited, reordered or deleted lines."""
        prev, n = GENESIS, 0
        for ev in self.read():
            n += 1
            h = ev.pop("hash")
            if ev.get("prev") != prev:
                return False, n, f"chain break at event {n}"
            if hashlib.sha256(prev.encode() + _canonical(ev)).hexdigest() != h:
                return False, n, f"hash mismatch at event {n}"
            prev = h
        return True, n, "ok"
