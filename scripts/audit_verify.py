#!/usr/bin/env python3
"""Audit log operations (docs/OBSERVABILITY.md, "Operations").

  audit_verify.py                       verify the hash chain, print event counts
  audit_verify.py --session SID         also list that session's events
  audit_verify.py --fp TYPE VALUE       fingerprint of a value + sessions/handles where it appeared
Exit code 1 if the chain is broken."""
from __future__ import annotations

import argparse
import sys
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "litellm"))
from pii_audit import AuditLog  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--log", help="audit.jsonl path (default: PII_AUDIT_LOG or .run/audit/audit.jsonl)")
    ap.add_argument("--session")
    ap.add_argument("--fp", nargs=2, metavar=("TYPE", "VALUE"))
    a = ap.parse_args()
    log = AuditLog(a.log) if a.log else AuditLog()
    ok, n, msg = log.verify()
    print(f"chain: {'OK' if ok else 'BROKEN'} ({n} events checked) {msg}")
    print("events by type:", dict(Counter(e["event"] for e in log.read())))
    if a.session:
        for e in log.read(session=a.session):
            print(f"  {e['ts']} {e['event']:18} call={e.get('call_id')} "
                  + " ".join(f"{k}={e[k]}" for k in ("reason", "entities", "restored", "leak_suspects") if e.get(k)))
    if a.fp:
        t, v = a.fp
        fp = log.fp(t, v)
        print(f"fp({t}) = {fp}")
        hits = [(e["session"], x["handle"], e["ts"]) for e in log.read() if e["event"] == "request.masked"
                for x in e.get("new", []) if x["fp"] == fp]
        for s, h, ts in hits:
            print(f"  session={s} handle={h} first_seen={ts}")
        if not hits:
            print("  not found")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
