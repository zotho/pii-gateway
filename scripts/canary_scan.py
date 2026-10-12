"""Scan .run/real-upstream/*.json for canaries; print hits and per-request pseudonym tokens.
usage: canary_scan.py [DIR] [--since N] [--session ID]  (DIR default .run/real-upstream; N = first capture index)"""
import json, re, sys
from pathlib import Path
_pos = []
_a = sys.argv[1:]
for _i, _x in enumerate(_a):
    if _x.startswith("--") or (_i and _a[_i - 1] in ("--since", "--session")):
        continue
    _pos.append(_x)
D = Path(_pos[0]) if _pos else Path(__file__).resolve().parents[1] / ".run" / "real-upstream"
if not D.is_dir():
    sys.exit(f"not a directory: {D}")
since = int(sys.argv[sys.argv.index("--since") + 1]) if "--since" in sys.argv else 0
CAN = ["Николай Петров", "Nikolai Petrov", "Sviatoslav Ivanov", "nikolai.petrov@example.com", "+7 916 123-45-67",
       "Николай", "Петров", "Nikolai", "Petrov", "Sviatoslav", "Ivanov", "Святослав", "Иванов", "Коля", "Коли", "Коле", "Колей", "Колю", "Петрова", "Петрову", "Петровым", "Иванова", "Иванову", "Ивановым", "Святославу", "Святослава", "Николая", "Николаю", "Николаем", "Petrova", "Ivanova",
       "916 123-45-67", "s.ivanov@example.org", "AKIAIOSFODNN7EXAMPLE", "IOSFODNN7"]
TOK = re.compile(r"<[A-Z][A-Z_]*_\d+>")
import subprocess
sess = sys.argv[sys.argv.index("--session") + 1] if "--session" in sys.argv else None
mapping = {}
if sess:
    raw = subprocess.run(["redis-cli", "-p", "16379", "hget", f"palena:pseudonymizer:{sess}", "mapping"],
                         capture_output=True, text=True).stdout.strip()
    mapping = json.loads(raw) if raw else {}
canary_tok = {v: k for k, v in mapping.items() if any(c.lower() in k.lower() for c in CAN)}
print("canary mapping:", {k: v for v, k in canary_tok.items()})
hits = {c: 0 for c in CAN}
n = 0
for p in sorted(D.glob("*.json")):
    if int(p.stem) < since: continue
    r = json.loads(p.read_text())
    if not r.get("body"): continue
    n += 1
    blob = json.dumps(r["body"], ensure_ascii=False)
    low = blob.lower()
    for c in CAN:
        k = low.count(c.lower())
        if k: hits[c] += k
    toks = sorted(set(TOK.findall(blob)))
    ctoks = {t: blob.count(t) for t in toks if t in canary_tok}
    print(p.name, r["url"].split("//")[1][:40], r.get("status"), "n_tokens=%d" % len(toks), "canary_tokens=", ctoks)
print("requests scanned:", n)
print("HITS:", {k: v for k, v in hits.items() if v} or "none")
