import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "litellm"))


@pytest.fixture(autouse=True)
def _isolated_audit(tmp_path, monkeypatch):
    """unit tests must never write to the real audit log (shared with the live stack / e2e tests)"""
    import pii_audit
    import pii_bridge
    monkeypatch.setattr(pii_bridge, "AUDIT", pii_audit.AuditLog(tmp_path / "audit.jsonl", key=b"unit"))
