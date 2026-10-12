"""Demo app: Pi-like agent + session vault + provider-side view.  :17000

  uv run python demo/server.py --mode mock|real [--port 17000]

Env: LITELLM_MASTER_KEY (else .run/master.key), DEMO_LITELLM_MOCK / DEMO_LITELLM_REAL (proxy URLs),
DEMO_MODEL_MOCK / DEMO_MODEL_REAL (model names), DEMO_UPSTREAM_MOCK, DEMO_REDIS_HOST / DEMO_REDIS_PORT.

Upstream association: LiteLLM does not forward the session header to the provider, and the
bridge may rewrite arbitrary text, so every agent call carries a letters-only marker in
`instructions` ("demo-session-<32 letters>", hex of the session id mapped to g..v so no
PII recognizer fires on it). /api/upstream keeps recorded upstream requests whose body contains
the marker (or whose headers carry the session id), restricted to the time window since start.
"""
from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import re
import sys
import time
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "litellm"))

import httpx  # noqa: E402
import redis  # noqa: E402
import uvicorn  # noqa: E402
from fastapi import FastAPI, HTTPException, Request  # noqa: E402
from fastapi.responses import StreamingResponse  # noqa: E402
from fastapi.staticfiles import StaticFiles  # noqa: E402

from demo.workspace import TOOLS, Workspace  # noqa: E402
from pii_audit import AuditLog  # noqa: E402

STATIC = Path(__file__).parent / "static"
WS_ROOT = ROOT / ".run" / "demo-workspace"
REAL_UP = ROOT / ".run" / "real-upstream"
MOCK_URL = os.environ.get("DEMO_UPSTREAM_MOCK", "http://127.0.0.1:19000")
REDIS_HOST = os.environ.get("DEMO_REDIS_HOST", "127.0.0.1")
REDIS_PORT = int(os.environ.get("DEMO_REDIS_PORT", "16379"))
MAX_STEPS = 8
HANDLE_RE = re.compile(r"^<([A-Z][A-Z_]*)_(\d+)>$")

def _master_key() -> str:
    key = os.environ.get("LITELLM_MASTER_KEY")
    if key:
        return key
    f = ROOT / ".run" / "master.key"
    return f.read_text().strip() if f.exists() else ""


MODES = {
    "mock": {"litellm": os.environ.get("DEMO_LITELLM_MOCK", "http://127.0.0.1:14000"),
             "model": os.environ.get("DEMO_MODEL_MOCK", "mock-agent"), "key": _master_key()},
    "real": {"litellm": os.environ.get("DEMO_LITELLM_REAL", "http://127.0.0.1:14001"),
             "model": os.environ.get("DEMO_MODEL_REAL", "gpt-6-astra"), "key": _master_key()},
}
CFG = {"mode": "mock"}
INSTRUCTIONS = ("You are a coding agent working in a small repository. Use the tools ls, read, grep, write "
                "to inspect files before answering. Be brief.")

app = FastAPI()
SESSIONS: dict[str, dict] = {}
_audit: AuditLog | None = None
_redis: redis.Redis | None = None


def audit() -> AuditLog:
    global _audit
    if _audit is None:
        _audit = AuditLog()
    return _audit


def rds() -> redis.Redis:
    global _redis
    if _redis is None:
        _redis = redis.Redis(host=REDIS_HOST, port=REDIS_PORT, decode_responses=True)
    return _redis


def marker(sid: str) -> str:
    h = sid.replace("-", "")
    return "demo-session-" + "".join(chr(ord("g") + int(c, 16)) for c in h if c in "0123456789abcdef")


def new_session() -> dict:
    sid = str(uuid.uuid4())
    ws = Workspace(WS_ROOT / sid)
    ws.seed()
    s = {"id": sid, "start": time.time(), "ws": ws, "history": [], "events": [], "seen": {}, "seq": 0,
         "revealed_ver": None, "busy": asyncio.Lock()}
    SESSIONS[sid] = s
    return s


def get_session(sid: str) -> dict:
    s = SESSIONS.get(sid)
    if not s:
        raise HTTPException(404, "unknown session")
    return s


@app.get("/api/config")
def config():
    return {"mode": CFG["mode"], "model": MODES[CFG["mode"]]["model"]}


@app.post("/api/session")
def create_session():
    s = new_session()
    return {"id": s["id"], "start": s["start"]}


@app.get("/api/session")
def session_state(session: str):
    s = SESSIONS.get(session)
    if not s:
        return {"exists": False}
    return {"exists": True, "id": s["id"], "start": s["start"], "events": s["events"]}


# ------------------------------------------------------------------ agent loop
def sse(ev: dict) -> bytes:
    return ("data: " + json.dumps(ev, ensure_ascii=False) + "\n\n").encode()


async def llm_step(client: httpx.AsyncClient, s: dict) -> list[dict]:
    m = MODES[CFG["mode"]]
    body = {"model": m["model"], "instructions": f"{INSTRUCTIONS} [{marker(s['id'])}]", "input": s["history"],
            "tools": TOOLS, "stream": True, "store": False}
    if CFG["mode"] == "real":
        body["include"] = ["reasoning.encrypted_content"]
    headers = {"Authorization": f"Bearer {m['key']}", "session-id": s["id"], "content-type": "application/json"}
    items, completed = [], None
    async with client.stream("POST", m["litellm"] + "/v1/responses", json=body, headers=headers) as r:
        if r.status_code != 200:
            raise RuntimeError(f"LiteLLM {r.status_code}: {(await r.aread()).decode('utf-8', 'replace')[:500]}")
        async for line in r.aiter_lines():
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if not data or data == "[DONE]":
                continue
            try:
                ev = json.loads(data)
            except ValueError:
                continue
            t = ev.get("type")
            if t == "response.output_item.done":
                items.append(ev["item"])
            elif t == "response.completed":
                completed = (ev.get("response") or {}).get("output") or []
            elif t in ("response.failed", "error", "response.incomplete"):
                raise RuntimeError(json.dumps(ev.get("response", ev).get("error", ev), ensure_ascii=False)[:500])
    return completed if completed else items


async def agent_run(s: dict, text: str):
    def emit(ev):
        s["events"].append(ev)
        return sse(ev)
    yield emit({"type": "user", "text": text})
    s["history"].append({"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]})
    try:
        async with httpx.AsyncClient(timeout=180) as client:
            for _ in range(MAX_STEPS):
                output = await llm_step(client, s)
                calls = []
                for it in output:
                    s["history"].append(it)
                    t = it.get("type")
                    if t == "message":
                        txt = "".join(c.get("text", "") for c in it.get("content", []) if c.get("type") == "output_text")
                        if txt:
                            yield emit({"type": "assistant", "text": txt})
                    elif t == "function_call":
                        calls.append(it)
                if not calls:
                    break
                for c in calls:
                    try:
                        args = json.loads(c.get("arguments") or "{}")
                    except ValueError:
                        args = {}
                    yield emit({"type": "tool_call", "id": c["call_id"], "name": c["name"], "args": args})
                    res = await asyncio.to_thread(s["ws"].call, c["name"], args)
                    s["history"].append({"type": "function_call_output", "call_id": c["call_id"], "output": res})
                    yield emit({"type": "tool_result", "id": c["call_id"], "name": c["name"], "text": res})
            else:
                yield emit({"type": "error", "text": f"stopped after {MAX_STEPS} steps"})
    except Exception as e:  # surfaced to the UI
        yield emit({"type": "error", "text": f"{type(e).__name__}: {e}"})
    yield emit({"type": "done"})


@app.post("/api/chat")
async def chat(req: Request):
    body = await req.json()
    s = get_session(body.get("session", ""))
    text = (body.get("message") or "").strip()
    if not text:
        raise HTTPException(400, "empty message")

    async def gen():
        async with s["busy"]:
            async for chunk in agent_run(s, text):
                yield chunk
    return StreamingResponse(gen(), media_type="text/event-stream")


# ------------------------------------------------------------------ vault / audit / upstream
@app.get("/api/mapping")
def mapping(session: str):
    s = get_session(session)
    raw = rds().hget(f"palena:pseudonymizer:{session}", "mapping")
    m = json.loads(raw) if raw else {}
    ver = hashlib.sha1(json.dumps(m, sort_keys=True, ensure_ascii=False).encode()).hexdigest()
    if m and s["revealed_ver"] != ver:  # audited reveal: log once per new mapping version, before returning
        audit().append("mapping.revealed", session=session, actor="demo-ui", reason="demo", handles="*")
        s["revealed_ver"] = ver
    new = [(real, h) for real, h in m.items() if real not in s["seen"]]
    new.sort(key=lambda x: int((HANDLE_RE.match(x[1]) or [0, 0, 0])[2] or 0))
    for real, h in new:
        s["seq"] += 1
        s["seen"][real] = s["seq"]
    rows = []
    for real, h in m.items():
        mt = HANDLE_RE.match(h)
        rows.append({"real": real, "handle": h, "type": mt.group(1) if mt else "?", "seq": s["seen"][real]})
    rows.sort(key=lambda r: -r["seq"])
    return {"version": ver, "rows": rows}


@app.get("/api/audit")
def audit_events(session: str, limit: int = 100):
    evs = list(audit().read(session=session))[-limit:]
    for e in evs:
        e.pop("prev", None)
    return {"events": evs[::-1]}


def _belongs(rec: dict, s: dict) -> bool:
    mk = marker(s["id"])
    if mk in json.dumps(rec.get("body"), ensure_ascii=False):
        return True
    hdrs = rec.get("headers") or {}
    return any(str(v) == s["id"] for k, v in hdrs.items() if k.lower() in ("session-id", "x-session-id"))


@app.get("/api/upstream")
def upstream(session: str):
    s = get_session(session)
    since = s["start"] - 1
    out = []
    if CFG["mode"] == "mock":
        try:
            recs = httpx.get(f"{MOCK_URL}/_requests", params={"since": since}, timeout=5).json()
        except Exception:
            recs = []
        for r in recs:
            if _belongs(r, s):
                out.append({"n": r["n"], "ts": r["ts"], "method": r["method"], "path": r["path"],
                            "status": None, "body": r["body"]})
    else:
        for f in sorted(REAL_UP.glob("*.json")):
            try:
                if f.stat().st_mtime < since:
                    continue
                r = json.loads(f.read_text())
            except (OSError, ValueError):
                continue
            if r.get("ts", 0) <= since or r.get("method") != "POST" or not _belongs(r, s):
                continue
            out.append({"n": int(f.stem) if f.stem.isdigit() else 0, "ts": r["ts"], "method": r["method"],
                        "path": re.sub(r"^https?://[^/]+", "", r.get("url", "")), "status": r.get("status"),
                        "body": r.get("body")})
    for r in out:
        r["size"] = len(json.dumps(r["body"], ensure_ascii=False))
    return {"requests": out}


app.mount("/", StaticFiles(directory=STATIC, html=True), name="static")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", choices=list(MODES), default="mock")
    ap.add_argument("--port", type=int, default=17000)
    ap.add_argument("--host", default="127.0.0.1")
    a = ap.parse_args()
    CFG["mode"] = a.mode
    uvicorn.run(app, host=a.host, port=a.port, log_level="warning")


if __name__ == "__main__":
    main()
