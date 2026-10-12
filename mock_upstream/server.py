"""Mock LLM upstream (Anthropic Messages + OpenAI Chat + OpenAI Responses) on :19000.

Records every inbound request to .run/upstream/NNNNN.json and in memory.
Control API:
  GET  /_requests            all recorded requests (?path=/v1/messages and/or ?since=<unix ts> to filter)
  POST /_reset               clear recordings + script queue (+ numbering)
  POST /_script              enqueue response(s): an object or a list of objects:
        {"text": "...", "thinking": "...", "tool_call": {"name": "f", "args": {...}},
         "stop": "end_turn|tool_use|...", "chunk": 3}
     Consumed FIFO by the next generation requests (any API). Empty queue => echo.
Default echo reply text: "ECHO: " + <last user text>. With "thinking" unset, echo has no
thinking block unless the request enabled thinking (anthropic `thinking`, responses `reasoning`),
in which case a short "THINKING: <last user text>" thinking block is emitted too.
Streaming text is split into small chunks (default 3 chars) so tokens straddle SSE events.
"""
import asyncio
import json
import os
import re
import time
import uuid
from collections import deque
from pathlib import Path

import uvicorn
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, StreamingResponse
from starlette.routing import Route

ROOT = Path(__file__).resolve().parent.parent
REC_DIR = ROOT / ".run" / "upstream"
REC_DIR.mkdir(parents=True, exist_ok=True)

RECORDS: list[dict] = []
SCRIPTS: deque = deque()
COUNTER = 0
DEFAULT_CHUNK = int(os.environ.get("MOCK_CHUNK", "3"))
SSE_DELAY = float(os.environ.get("MOCK_SSE_DELAY", "0"))


def rid_headers(kind: str) -> dict:
    """Provider request-id header like the real APIs: Anthropic `request-id`, OpenAI `x-request-id`."""
    return {"request-id": f"req_mock_{COUNTER:05d}"} if kind == "anthropic" else {"x-request-id": f"mock-{COUNTER:05d}"}


def uid(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex[:24]}"


def chunks(s: str, n: int):
    n = max(1, n)
    return [s[i:i + n] for i in range(0, len(s), n)] or [""]


# ---------------------------------------------------------------- recording
async def record(request: Request) -> dict:
    global COUNTER
    raw = await request.body()
    try:
        body = json.loads(raw) if raw else None
    except Exception:
        body = {"_unparsed": raw.decode("utf-8", "replace")}
    COUNTER += 1
    rec = {
        "n": COUNTER,
        "ts": time.time(),
        "method": request.method,
        "path": request.url.path,
        "query": request.url.query,
        "headers": dict(request.headers),
        "body": body,
    }
    RECORDS.append(rec)
    (REC_DIR / f"{COUNTER:05d}.json").write_text(json.dumps(rec, ensure_ascii=False, indent=2))
    return rec


# ---------------------------------------------------------------- last-user-text extraction
def _blocks_text(content) -> str:
    if isinstance(content, str):
        return content
    out = []
    for b in content or []:
        if not isinstance(b, dict):
            continue
        t = b.get("type")
        if t in ("text", "input_text", "output_text"):
            out.append(b.get("text", ""))
        elif t == "tool_result":
            out.append(_blocks_text(b.get("content")))
    return "\n".join(x for x in out if x)


def last_user_text(body: dict) -> str:
    if not isinstance(body, dict):
        return ""
    msgs = body.get("messages")
    if msgs is not None:  # anthropic / openai chat
        for m in reversed(msgs):
            if m.get("role") == "user":
                txt = _blocks_text(m.get("content"))
                if txt:
                    return txt
        return ""
    inp = body.get("input")  # responses
    if isinstance(inp, str):
        return inp
    for it in reversed(inp or []):
        if isinstance(it, dict) and it.get("role") == "user":
            txt = _blocks_text(it.get("content"))
            if txt:
                return txt
    return ""


def plan(body: dict, want_thinking: bool) -> dict:
    """Return the response plan: text, thinking, tool_call, stop, chunk."""
    if SCRIPTS:
        p = dict(SCRIPTS.popleft())
    else:
        txt = last_user_text(body)
        p = {"text": "ECHO: " + txt}
        if want_thinking:
            p["thinking"] = "THINKING: " + txt
    p.setdefault("chunk", DEFAULT_CHUNK)
    return p


HANDLE_RE = re.compile(r"<([A-Z][A-Z_]*)_(\d+)>")


def _uniq(xs):
    return list(dict.fromkeys(xs))


def agent_plan(body: dict) -> dict:
    """Deterministic coding-agent script for model `mock-agent` (demo app).

    no function_call_output yet -> read CLAUDE.md; one -> read src/billing.py; two+ -> final answer
    that names every <TYPE_n> handle seen in tool outputs (contact, e-mail, billing maintainer).
    """
    inp = body.get("input")
    outs = []
    for it in (inp if isinstance(inp, list) else []):
        if isinstance(it, dict) and it.get("type") == "function_call_output":
            o = it.get("output")
            outs.append(o if isinstance(o, str) else json.dumps(o, ensure_ascii=False))
    if len(outs) == 0:
        return {"tool_call": {"name": "read", "args": {"path": "CLAUDE.md"}}, "chunk": DEFAULT_CHUNK}
    if len(outs) == 1:
        return {"tool_call": {"name": "read", "args": {"path": "src/billing.py"}}, "chunk": DEFAULT_CHUNK}
    seen = [[m.group(0) for m in HANDLE_RE.finditer(o)] for o in outs]
    first, second = _uniq(seen[0]), _uniq(seen[1])
    persons1 = [h for h in first if h.startswith("<PERSON_")]
    emails1 = [h for h in first if h.startswith("<EMAIL_ADDRESS_")]
    persons2 = [h for h in second if h.startswith("<PERSON_") and h not in first]
    parts, used = [], set()
    if persons1:
        c = f"Contact: {persons1[0]}"
        used.add(persons1[0])
        if len(persons1) > 1:
            c += f" ({persons1[1]})"
            used.add(persons1[1])
        parts.append(c)
    if emails1:
        parts.append(f"email {emails1[0]}")
        used.add(emails1[0])
    if persons2:
        parts.append(f"billing maintainer {persons2[0]}")
        used.add(persons2[0])
    rest = [h for h in _uniq(first + second) if h not in used]
    text = "; ".join(parts) if parts else "No entities in tool outputs"
    if rest:
        text += ". Also seen: " + ", ".join(rest)
    return {"text": text, "chunk": DEFAULT_CHUNK}


def sse(event: str | None, data) -> bytes:
    s = ""
    if event:
        s += f"event: {event}\n"
    s += "data: " + (data if isinstance(data, str) else json.dumps(data, ensure_ascii=False)) + "\n\n"
    return s.encode()


async def stream(events):
    for e in events:
        yield e
        if SSE_DELAY:
            await asyncio.sleep(SSE_DELAY)


def approx_tokens(s: str) -> int:
    return max(1, len(s) // 4)


# ---------------------------------------------------------------- Anthropic /v1/messages
def anthropic_blocks(p: dict) -> list[dict]:
    blocks = []
    if p.get("thinking") is not None:
        blocks.append({"type": "thinking", "thinking": p["thinking"],
                       "signature": "sig_" + uuid.uuid4().hex + "=="})
    if p.get("text") is not None:
        blocks.append({"type": "text", "text": p["text"]})
    if p.get("tool_call"):
        tc = p["tool_call"]
        blocks.append({"type": "tool_use", "id": uid("toolu"), "name": tc["name"],
                       "input": tc.get("args", {})})
    return blocks


async def messages(request: Request):
    rec = await record(request)
    body = rec["body"] or {}
    model = body.get("model", "mock-claude")
    p = plan(body, bool((body.get("thinking") or {}).get("type") in ("enabled", "adaptive")))
    blocks = anthropic_blocks(p)
    stop = p.get("stop") or ("tool_use" if p.get("tool_call") else "end_turn")
    in_tok = approx_tokens(json.dumps(body.get("messages", "")))
    out_tok = approx_tokens(json.dumps(blocks))
    mid = uid("msg")
    if not body.get("stream"):
        return JSONResponse({"id": mid, "type": "message", "role": "assistant", "model": model,
                             "content": blocks, "stop_reason": stop, "stop_sequence": None,
                             "usage": {"input_tokens": in_tok, "output_tokens": out_tok}},
                            headers=rid_headers("anthropic"))
    n = p["chunk"]
    ev = [sse("message_start", {"type": "message_start", "message": {
        "id": mid, "type": "message", "role": "assistant", "model": model, "content": [],
        "stop_reason": None, "stop_sequence": None,
        "usage": {"input_tokens": in_tok, "output_tokens": 1}}})]
    ev.append(sse("ping", {"type": "ping"}))
    for i, b in enumerate(blocks):
        t = b["type"]
        if t == "thinking":
            ev.append(sse("content_block_start", {"type": "content_block_start", "index": i,
                          "content_block": {"type": "thinking", "thinking": ""}}))
            for c in chunks(b["thinking"], n):
                ev.append(sse("content_block_delta", {"type": "content_block_delta", "index": i,
                              "delta": {"type": "thinking_delta", "thinking": c}}))
            ev.append(sse("content_block_delta", {"type": "content_block_delta", "index": i,
                          "delta": {"type": "signature_delta", "signature": b["signature"]}}))
        elif t == "text":
            ev.append(sse("content_block_start", {"type": "content_block_start", "index": i,
                          "content_block": {"type": "text", "text": ""}}))
            for c in chunks(b["text"], n):
                ev.append(sse("content_block_delta", {"type": "content_block_delta", "index": i,
                              "delta": {"type": "text_delta", "text": c}}))
        else:
            ev.append(sse("content_block_start", {"type": "content_block_start", "index": i,
                          "content_block": {"type": "tool_use", "id": b["id"], "name": b["name"],
                                            "input": {}}}))
            for c in chunks(json.dumps(b["input"], ensure_ascii=False), n):
                ev.append(sse("content_block_delta", {"type": "content_block_delta", "index": i,
                              "delta": {"type": "input_json_delta", "partial_json": c}}))
        ev.append(sse("content_block_stop", {"type": "content_block_stop", "index": i}))
    ev.append(sse("message_delta", {"type": "message_delta",
              "delta": {"stop_reason": stop, "stop_sequence": None},
              "usage": {"output_tokens": out_tok}}))
    ev.append(sse("message_stop", {"type": "message_stop"}))
    return StreamingResponse(stream(ev), media_type="text/event-stream", headers=rid_headers("anthropic"))


async def count_tokens(request: Request):
    rec = await record(request)
    return JSONResponse({"input_tokens": approx_tokens(json.dumps(rec["body"] or {}))})


# ---------------------------------------------------------------- OpenAI /v1/chat/completions
async def chat(request: Request):
    rec = await record(request)
    body = rec["body"] or {}
    model = body.get("model", "mock-gpt")
    p = plan(body, False)
    text = p.get("text")
    tc = p.get("tool_call")
    finish = p.get("stop") or ("tool_calls" if tc else "stop")
    cid = uid("chatcmpl")
    created = int(time.time())
    tool_calls = None
    if tc:
        tool_calls = [{"id": uid("call"), "type": "function",
                       "function": {"name": tc["name"], "arguments": json.dumps(tc.get("args", {}), ensure_ascii=False)}}]
    in_tok = approx_tokens(json.dumps(body.get("messages", "")))
    out_tok = approx_tokens((text or "") + json.dumps(tool_calls or ""))
    usage = {"prompt_tokens": in_tok, "completion_tokens": out_tok, "total_tokens": in_tok + out_tok}
    if not body.get("stream"):
        msg = {"role": "assistant", "content": text, "refusal": None}
        if p.get("thinking") is not None:
            msg["reasoning_content"] = p["thinking"]
        if tool_calls:
            msg["tool_calls"] = tool_calls
        return JSONResponse({"id": cid, "object": "chat.completion", "created": created,
                             "model": model, "choices": [{"index": 0, "message": msg,
                             "finish_reason": finish}], "usage": usage},
                            headers=rid_headers("openai"))
    n = p["chunk"]

    def ch(delta, fr=None):
        return sse(None, {"id": cid, "object": "chat.completion.chunk", "created": created,
                          "model": model, "choices": [{"index": 0, "delta": delta, "finish_reason": fr}]})
    ev = [ch({"role": "assistant", "content": ""})]
    if p.get("thinking") is not None:
        for c in chunks(p["thinking"], n):
            ev.append(ch({"reasoning_content": c}))
    if text is not None:
        for c in chunks(text, n):
            ev.append(ch({"content": c}))
    if tool_calls:
        t0 = tool_calls[0]
        ev.append(ch({"tool_calls": [{"index": 0, "id": t0["id"], "type": "function",
                      "function": {"name": t0["function"]["name"], "arguments": ""}}]}))
        for c in chunks(t0["function"]["arguments"], n):
            ev.append(ch({"tool_calls": [{"index": 0, "function": {"arguments": c}}]}))
    ev.append(ch({}, finish))
    if (body.get("stream_options") or {}).get("include_usage"):
        ev.append(sse(None, {"id": cid, "object": "chat.completion.chunk", "created": created,
                             "model": model, "choices": [], "usage": usage}))
    ev.append(sse(None, "[DONE]"))
    return StreamingResponse(stream(ev), media_type="text/event-stream", headers=rid_headers("openai"))


# ---------------------------------------------------------------- OpenAI /v1/responses
def responses_output(p: dict) -> list[dict]:
    out = []
    if p.get("thinking") is not None:
        out.append({"type": "reasoning", "id": uid("rs"),
                    "summary": [{"type": "summary_text", "text": p["thinking"]}],
                    "encrypted_content": "gAAAA" + uuid.uuid4().hex + uuid.uuid4().hex})
    if p.get("text") is not None:
        out.append({"type": "message", "id": uid("msg"), "status": "completed", "role": "assistant",
                    "content": [{"type": "output_text", "text": p["text"], "annotations": []}]})
    if p.get("tool_call"):
        tc = p["tool_call"]
        out.append({"type": "function_call", "id": uid("fc"), "call_id": uid("call"),
                    "name": tc["name"], "arguments": json.dumps(tc.get("args", {}), ensure_ascii=False),
                    "status": "completed"})
    return out


async def responses(request: Request):
    rec = await record(request)
    body = rec["body"] or {}
    model = body.get("model", "mock-gpt")
    if str(model).endswith("mock-agent"):
        p = agent_plan(body)
    else:
        p = plan(body, bool(body.get("reasoning")))
    out = responses_output(p)
    rid = uid("resp")
    created = int(time.time())
    in_tok = approx_tokens(json.dumps(body.get("input", "")))
    out_tok = approx_tokens(json.dumps(out))
    usage = {"input_tokens": in_tok, "output_tokens": out_tok, "total_tokens": in_tok + out_tok,
             "input_tokens_details": {"cached_tokens": 0},
             "output_tokens_details": {"reasoning_tokens": 0}}

    def resp(status, output):
        return {"id": rid, "object": "response", "created_at": created, "status": status,
                "model": model, "output": output, "usage": usage if status == "completed" else None,
                "parallel_tool_calls": True, "tool_choice": "auto", "tools": body.get("tools", []),
                "instructions": body.get("instructions"), "store": False}
    if not body.get("stream"):
        return JSONResponse(resp("completed", out), headers=rid_headers("openai"))
    n = p["chunk"]
    seq = [0]

    def e(t, **kw):
        kw.update(type=t, sequence_number=seq[0])
        seq[0] += 1
        return sse(t, kw)
    ev = [e("response.created", response=resp("in_progress", [])),
          e("response.in_progress", response=resp("in_progress", []))]
    for i, it in enumerate(out):
        t = it["type"]
        if t == "reasoning":
            ev.append(e("response.output_item.added", output_index=i,
                        item={"type": "reasoning", "id": it["id"], "summary": []}))
            ev.append(e("response.reasoning_summary_part.added", item_id=it["id"], output_index=i,
                        summary_index=0, part={"type": "summary_text", "text": ""}))
            for c in chunks(it["summary"][0]["text"], n):
                ev.append(e("response.reasoning_summary_text.delta", item_id=it["id"],
                            output_index=i, summary_index=0, delta=c))
            ev.append(e("response.reasoning_summary_text.done", item_id=it["id"], output_index=i,
                        summary_index=0, text=it["summary"][0]["text"]))
            ev.append(e("response.reasoning_summary_part.done", item_id=it["id"], output_index=i,
                        summary_index=0, part=it["summary"][0]))
            ev.append(e("response.output_item.done", output_index=i, item=it))
        elif t == "message":
            txt = it["content"][0]["text"]
            ev.append(e("response.output_item.added", output_index=i,
                        item={**it, "status": "in_progress", "content": []}))
            ev.append(e("response.content_part.added", item_id=it["id"], output_index=i,
                        content_index=0, part={"type": "output_text", "text": "", "annotations": []}))
            for c in chunks(txt, n):
                ev.append(e("response.output_text.delta", item_id=it["id"], output_index=i,
                            content_index=0, delta=c))
            ev.append(e("response.output_text.done", item_id=it["id"], output_index=i,
                        content_index=0, text=txt))
            ev.append(e("response.content_part.done", item_id=it["id"], output_index=i,
                        content_index=0, part=it["content"][0]))
            ev.append(e("response.output_item.done", output_index=i, item=it))
        else:
            ev.append(e("response.output_item.added", output_index=i,
                        item={**it, "arguments": "", "status": "in_progress"}))
            for c in chunks(it["arguments"], n):
                ev.append(e("response.function_call_arguments.delta", item_id=it["id"],
                            output_index=i, delta=c))
            ev.append(e("response.function_call_arguments.done", item_id=it["id"],
                        output_index=i, arguments=it["arguments"]))
            ev.append(e("response.output_item.done", output_index=i, item=it))
    ev.append(e("response.completed", response=resp("completed", out)))
    return StreamingResponse(stream(ev), media_type="text/event-stream", headers=rid_headers("openai"))


async def responses_compact(request: Request):
    rec = await record(request)
    body = rec["body"] or {}
    return JSONResponse({
        "id": uid("resp"), "object": "response.compaction", "created_at": int(time.time()),
        "output": [{"type": "compaction", "id": uid("cmp"),
                    "encrypted_content": "gAAAAcmp" + uuid.uuid4().hex * 3}],
        "usage": {"input_tokens": approx_tokens(json.dumps(body.get("input", ""))),
                  "output_tokens": 10, "total_tokens": 10}})


# ---------------------------------------------------------------- control
async def get_requests(request: Request):
    path = request.query_params.get("path")
    since = float(request.query_params.get("since") or 0)  # unix ts; only newer records
    return JSONResponse([r for r in RECORDS if (not path or r["path"] == path) and r["ts"] > since])


async def reset(request: Request):
    global COUNTER
    RECORDS.clear()
    SCRIPTS.clear()
    COUNTER = 0
    for f in REC_DIR.glob("*.json"):
        f.unlink()
    return JSONResponse({"ok": True})


async def script(request: Request):
    body = await request.json()
    items = body if isinstance(body, list) else [body]
    SCRIPTS.extend(items)
    return JSONResponse({"queued": len(items), "pending": len(SCRIPTS)})


async def health(request: Request):
    return JSONResponse({"ok": True, "recorded": len(RECORDS), "pending_scripts": len(SCRIPTS)})


app = Starlette(routes=[
    Route("/v1/messages", messages, methods=["POST"]),
    Route("/v1/messages/count_tokens", count_tokens, methods=["POST"]),
    Route("/v1/chat/completions", chat, methods=["POST"]),
    Route("/v1/responses", responses, methods=["POST"]),
    Route("/v1/responses/compact", responses_compact, methods=["POST"]),
    Route("/_requests", get_requests, methods=["GET"]),
    Route("/_reset", reset, methods=["POST"]),
    Route("/_script", script, methods=["POST"]),
    Route("/_health", health, methods=["GET"]),
])

if __name__ == "__main__":
    uvicorn.run(app, host="127.0.0.1", port=int(os.environ.get("PORT", "19000")), log_level="info")
