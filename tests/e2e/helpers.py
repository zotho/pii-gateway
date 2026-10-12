"""Black-box helpers: HTTP to LiteLLM + mock control API (+ Redis, + scripts/*.sh)."""
from __future__ import annotations

import json
import os
import re
import subprocess
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import httpx

ROOT = Path(__file__).resolve().parents[2]
LITELLM = "http://127.0.0.1:14000"
MOCK = "http://127.0.0.1:19000"
PALENA = "http://127.0.0.1:18080"
REDIS_PORT = 16379


def _master_key() -> str:
    key = os.environ.get("LITELLM_MASTER_KEY")
    if key:
        return key
    f = ROOT / ".run" / "master.key"
    return f.read_text().strip() if f.exists() else ""


KEY = _master_key()

TOKEN_RE = re.compile(r"<[A-Z][A-Z_]*_\d+>")

# --- canaries -----------------------------------------------------------------------------
NAME_RU = "Николай Петров"
NAME_EN = "Nikolai Petrov"
NAME_EN2 = "Sviatoslav Ivanov"
EMAIL = "nikolai.petrov@example.com"
PHONE = "+7 916 123-45-67"
AWS_KEY = "AKIAIOSFODNN7EXAMPLE"
# stems catch declined / partial leaks too
PII_CANARIES = [NAME_RU, NAME_EN, NAME_EN2, EMAIL, PHONE, "Николай", "Петров", "Nikolai", "Petrov",
                "Sviatoslav", "Ivanov", "916 123-45-67"]
SECRET_CANARIES = [AWS_KEY]

# single-language texts (detection is per-language) carrying the PII canaries
TEXT = (f"Hello, I am {NAME_EN}, my email is {EMAIL} and my phone is {PHONE}. "
        f"My manager is {NAME_EN2}.")
TEXT_RU = f"Привет, меня зовут {NAME_RU}, моя почта {EMAIL}, телефон {PHONE}."
# mixed RU+EN in one string (realistic for coding agents)
RICH = (f"Привет, меня зовут {NAME_RU}, моя почта {EMAIL}, телефон {PHONE}. "
        f"Hello, I am {NAME_EN} and my manager is {NAME_EN2}.")


def new_sid(tag: str = "") -> str:
    return f"e2e-{tag}-{uuid.uuid4().hex[:12]}"


# --- mock control -------------------------------------------------------------------------
def mock_reset():
    httpx.post(f"{MOCK}/_reset", timeout=10).raise_for_status()


def mock_script(*items: dict):
    httpx.post(f"{MOCK}/_script", json=list(items), timeout=10).raise_for_status()


def upstream(path: str | None = None) -> list[dict]:
    r = httpx.get(f"{MOCK}/_requests", params={"path": path} if path else None, timeout=10)
    r.raise_for_status()
    return r.json()


def dumps(o: Any) -> str:
    return json.dumps(o, ensure_ascii=False, sort_keys=True)


PATTERN_CANARIES = [EMAIL, PHONE, "916 123-45-67"]  # what still gets masked in pattern-only (system) text


def assert_no_canary(reqs: list[dict], secrets: bool = False, pattern_only: bool = False):
    """JSON-decoded bodies are re-serialised with ensure_ascii=False, so \\u-escaped Cyrillic is caught.
    pattern_only: the text under test is a system prompt (NER names are intentionally not masked there)."""
    bad = (PATTERN_CANARIES if pattern_only else PII_CANARIES) + (SECRET_CANARIES if secrets else [])
    for rec in reqs:
        blob = dumps(rec.get("body")) + " " + (rec.get("query") or "") + " " + rec.get("path", "")
        leaks = [c for c in bad if c.lower() in blob.lower()]
        assert not leaks, f"canary leaked upstream ({rec['path']}): {leaks}\n{blob[:1500]}"


def walk(o):
    """yield every dict nested in o"""
    if isinstance(o, dict):
        yield o
        for v in o.values():
            yield from walk(v)
    elif isinstance(o, list):
        for v in o:
            yield from walk(v)


def find(o, type_: str) -> list[dict]:
    return [d for d in walk(o) if d.get("type") == type_]


def tokens(s: str) -> list[str]:
    return TOKEN_RE.findall(s)


# --- HTTP ---------------------------------------------------------------------------------
@dataclass
class Resp:
    status: int
    raw: str
    json: Any = None
    events: list = field(default_factory=list)  # [(event_name|None, data)]
    stream: bool = False


def parse_sse(raw: str):
    evs = []
    for block in raw.replace("\r\n", "\n").split("\n\n"):
        name, data = None, []
        for line in block.split("\n"):
            if line.startswith("event:"):
                name = line[6:].strip()
            elif line.startswith("data:"):
                data.append(line[5:].lstrip())
        if data:
            d = "\n".join(data)
            try:
                d = json.loads(d)
            except Exception:
                pass
            evs.append((name, d))
    return evs


def http_post(path: str, headers: dict, body: dict, stream: bool = False, timeout=60) -> Resp:
    h = {"content-type": "application/json", "authorization": f"Bearer {KEY}", **headers}
    with httpx.Client(timeout=timeout) as c:
        r = c.post(LITELLM + path, headers=h, json=body)
    raw = r.text
    resp = Resp(r.status_code, raw, stream=stream)
    if r.status_code == 200 and stream:
        resp.events = parse_sse(raw)
    else:
        try:
            resp.json = r.json()
        except Exception:
            pass
    return resp


# --- protocols ----------------------------------------------------------------------------
@dataclass
class Case:
    id: str
    body: dict
    locate: Callable[[dict], str]  # upstream body -> serialised text that must hold the token(s)
    args_check: Callable[[dict], None] | None = None


def _txt(content) -> str:
    if isinstance(content, str):
        return content
    out = []
    for b in content or []:
        if isinstance(b, dict):
            if b.get("type") in ("text", "input_text", "output_text"):
                out.append(b.get("text", ""))
            elif b.get("type") == "tool_result":
                out.append(_txt(b.get("content")))
    return "\n".join(out)


ARG_TMPL = {"path": f"/home/{NAME_EN2}/a.txt", "to": [EMAIL, 3, True, None],
            "nested": {"who": NAME_EN, "n": 1.5}}


def _check_args_struct(a):
    assert isinstance(a, dict), a
    assert a["to"][1:] == [3, True, None], a
    assert a["nested"]["n"] == 1.5, a
    assert TOKEN_RE.search(a["path"]) and TOKEN_RE.search(a["to"][0]) and TOKEN_RE.search(a["nested"]["who"]), a


class Proto:
    name: str
    path: str
    model: str = "mock-gpt"
    session_header: dict

    def headers(self, sid: str) -> dict:
        return self.session_header(sid)

    def send(self, sid: str, body: dict, stream: bool = False, model=None) -> Resp:
        body = dict(body)
        body["model"] = model or self.model
        if stream:
            body["stream"] = True
        return http_post(self.path, self.headers(sid), body, stream)

    def up(self) -> list[dict]:
        return upstream(self.path)

    def last_up(self) -> dict:
        reqs = self.up()
        assert reqs, f"no upstream request recorded on {self.path}"
        return reqs[-1]["body"]

    def __repr__(self):
        return self.name


class Anthropic(Proto):
    name = "anthropic"
    path = "/v1/messages"
    model = "mock-claude"

    def headers(self, sid):
        return {"x-claude-code-session-id": sid, "anthropic-version": "2023-06-01", "x-api-key": KEY}

    def body(self, turns, system=None, thinking=False):
        b = {"max_tokens": 256, "messages": [{"role": r, "content": t} for r, t in turns]}
        if system:
            b["system"] = system
        if thinking:
            b["thinking"] = {"type": "enabled", "budget_tokens": 1024}
        return b

    def msg_texts(self, up):
        return [_txt(m.get("content")) for m in up["messages"]]

    # ---- response extraction
    def text(self, r):
        if r.stream:
            return "".join(d["delta"]["text"] for _, d in r.events
                           if isinstance(d, dict) and d.get("delta", {}).get("type") == "text_delta")
        return "".join(b.get("text", "") for b in r.json["content"] if b["type"] == "text")

    def tool_args(self, r):
        if r.stream:
            s = "".join(d["delta"]["partial_json"] for _, d in r.events
                        if isinstance(d, dict) and d.get("delta", {}).get("type") == "input_json_delta")
            return json.loads(s)
        return [b for b in r.json["content"] if b["type"] == "tool_use"][0]["input"]

    def thinking(self, r):
        if r.stream:
            t = "".join(d["delta"]["thinking"] for _, d in r.events
                        if isinstance(d, dict) and d.get("delta", {}).get("type") == "thinking_delta")
            s = "".join(d["delta"]["signature"] for _, d in r.events
                        if isinstance(d, dict) and d.get("delta", {}).get("type") == "signature_delta")
            return [(t, s)] if (t or s) else []
        return [(b["thinking"], b.get("signature")) for b in r.json["content"] if b["type"] == "thinking"]

    def tool_script(self, tool_args):
        return {"text": "calling", "tool_call": {"name": "write_file", "args": tool_args}}

    def cases(self, t=TEXT):
        u = lambda body: body  # noqa
        hist_tool = lambda tu_input, tr_content: {  # noqa
            "max_tokens": 64, "messages": [
                {"role": "user", "content": "go"},
                {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_01", "name": "read",
                                                   "input": tu_input}]},
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_01",
                                              "content": tr_content}]}]}
        return [
            Case("system_str", {"max_tokens": 64, "system": t, "messages": [{"role": "user", "content": "hi"}]},
                 lambda up: dumps(up.get("system"))),
            Case("system_blocks", {"max_tokens": 64, "system": [{"type": "text", "text": t}],
                                   "messages": [{"role": "user", "content": "hi"}]},
                 lambda up: dumps(up.get("system"))),
            Case("user_str", {"max_tokens": 64, "messages": [{"role": "user", "content": t}]},
                 lambda up: dumps(up["messages"])),
            Case("user_text_blocks", {"max_tokens": 64, "messages": [
                {"role": "user", "content": [{"type": "text", "text": "x"}, {"type": "text", "text": t}]}]},
                 lambda up: dumps(up["messages"])),
            Case("assistant_text_history", {"max_tokens": 64, "messages": [
                {"role": "user", "content": "q"}, {"role": "assistant", "content": t},
                {"role": "user", "content": "go on"}]},
                 lambda up: dumps(up["messages"][1])),
            Case("tool_result_str", hist_tool({"p": "x"}, t), lambda up: dumps(find(up, "tool_result"))),
            Case("tool_result_blocks", hist_tool({"p": "x"}, [{"type": "text", "text": t}]),
                 lambda up: dumps(find(up, "tool_result"))),
            Case("tool_use_input", hist_tool(ARG_TMPL, "ok"), lambda up: dumps(find(up, "tool_use")),
                 lambda up: _check_args_struct(find(up, "tool_use")[0]["input"])),
        ]

    def history_thinking(self, user_text, think, sig, redacted):
        return {"max_tokens": 2048, "thinking": {"type": "enabled", "budget_tokens": 1024}, "messages": [
            {"role": "user", "content": "Question one"},
            {"role": "assistant", "content": [
                {"type": "thinking", "thinking": think, "signature": sig},
                {"type": "redacted_thinking", "data": redacted},
                {"type": "text", "text": "Answer one"}]},
            {"role": "user", "content": user_text}]}

    def check_thinking_passthrough(self, up, think, sig, redacted):
        th = find(up, "thinking")
        assert th, f"thinking block missing upstream: {dumps(up)[:800]}"
        assert th[0]["thinking"] == think and th[0]["signature"] == sig, th[0]
        rd = find(up, "redacted_thinking")
        assert rd and rd[0]["data"] == redacted, rd


class Chat(Proto):
    name = "chat"
    path = "/v1/chat/completions"

    def headers(self, sid):
        return {"x-session-id": sid}

    def body(self, turns, system=None, thinking=False):
        m = ([{"role": "system", "content": system}] if system else []) + \
            [{"role": r, "content": t} for r, t in turns]
        return {"messages": m, "max_tokens": 256}

    def msg_texts(self, up):
        return [_txt(m.get("content")) for m in up["messages"] if m["role"] != "system"]

    def text(self, r):
        if r.stream:
            return "".join(d["choices"][0]["delta"].get("content") or "" for _, d in r.events
                           if isinstance(d, dict) and d.get("choices"))
        return r.json["choices"][0]["message"].get("content") or ""

    def tool_args(self, r):
        if r.stream:
            s = "".join(tc["function"].get("arguments") or "" for _, d in r.events
                        if isinstance(d, dict) and d.get("choices")
                        for tc in d["choices"][0]["delta"].get("tool_calls") or [])
            return json.loads(s)
        return json.loads(r.json["choices"][0]["message"]["tool_calls"][0]["function"]["arguments"])

    def thinking(self, r):
        if r.stream:
            t = "".join(d["choices"][0]["delta"].get("reasoning_content") or "" for _, d in r.events
                        if isinstance(d, dict) and d.get("choices"))
            return [(t, None)] if t else []
        t = r.json["choices"][0]["message"].get("reasoning_content")
        return [(t, None)] if t else []

    def tool_script(self, tool_args):
        return {"text": "calling", "tool_call": {"name": "write_file", "args": tool_args}}

    def cases(self, t=TEXT):
        def hist(args, tool_content):
            return {"max_tokens": 64, "messages": [
                {"role": "user", "content": "go"},
                {"role": "assistant", "content": None, "tool_calls": [
                    {"id": "call_1", "type": "function",
                     "function": {"name": "read", "arguments": json.dumps(args, ensure_ascii=False)}}]},
                {"role": "tool", "tool_call_id": "call_1", "content": tool_content},
                {"role": "user", "content": "thanks"}]}

        def asst_args(up):
            return [m for m in up["messages"] if m.get("tool_calls")][0]["tool_calls"][0]["function"]["arguments"]

        return [
            Case("system", {"max_tokens": 64, "messages": [{"role": "system", "content": t},
                                                           {"role": "user", "content": "hi"}]},
                 lambda up: dumps(up["messages"][0])),
            Case("user_str", {"max_tokens": 64, "messages": [{"role": "user", "content": t}]},
                 lambda up: dumps(up["messages"])),
            Case("user_parts", {"max_tokens": 64, "messages": [{"role": "user", "content": [
                {"type": "text", "text": "x"}, {"type": "text", "text": t}]}]},
                 lambda up: dumps(up["messages"])),
            Case("assistant_text_history", {"max_tokens": 64, "messages": [
                {"role": "user", "content": "q"}, {"role": "assistant", "content": t},
                {"role": "user", "content": "go on"}]}, lambda up: dumps(up["messages"][1])),
            Case("tool_message_str", hist({"p": "x"}, t),
                 lambda up: dumps([m for m in up["messages"] if m["role"] == "tool"])),
            Case("tool_message_parts", hist({"p": "x"}, [{"type": "text", "text": t}]),
                 lambda up: dumps([m for m in up["messages"] if m["role"] == "tool"])),
            Case("tool_calls_args", hist(ARG_TMPL, "ok"), asst_args,
                 lambda up: _check_args_struct(json.loads(asst_args(up)))),
        ]


class Responses(Proto):
    name = "responses"
    path = "/v1/responses"

    def headers(self, sid):
        return {"session-id": sid, "user-agent": "codex_cli_rs/0.99.0 (e2e)"}

    @staticmethod
    def msg(role, text):
        return {"type": "message", "role": role,
                "content": [{"type": "input_text" if role == "user" else "output_text", "text": text}]}

    def body(self, turns, system=None, thinking=False):
        b = {"input": [self.msg(r, t) for r, t in turns], "max_output_tokens": 256, "store": False}
        if system:
            b["instructions"] = system
        if thinking:
            b["reasoning"] = {"effort": "low", "summary": "auto"}
        return b

    def msg_texts(self, up):
        inp = up["input"]
        if isinstance(inp, str):
            return [inp]
        return [_txt(i.get("content")) for i in inp if i.get("type", "message") == "message"]

    def _items(self, r):
        if r.stream:
            for _, d in r.events:
                if isinstance(d, dict) and d.get("type") == "response.completed":
                    return d["response"]["output"]
            return []
        return r.json["output"]

    def text(self, r):
        if r.stream:
            return "".join(d["delta"] for _, d in r.events
                           if isinstance(d, dict) and d.get("type") == "response.output_text.delta")
        return "".join(c["text"] for it in r.json["output"] if it["type"] == "message"
                       for c in it["content"] if c["type"] == "output_text")

    def tool_args(self, r):
        if r.stream:
            s = "".join(d["delta"] for _, d in r.events
                        if isinstance(d, dict) and d.get("type") == "response.function_call_arguments.delta")
            # the .done event and final response must agree with the deltas
            for _, d in r.events:
                if isinstance(d, dict) and d.get("type") == "response.function_call_arguments.done":
                    assert json.loads(d["arguments"]) == json.loads(s), "done event != deltas"
            for it in self._items(r):
                if it["type"] == "function_call":
                    assert json.loads(it["arguments"]) == json.loads(s), "completed item != deltas"
            return json.loads(s)
        return json.loads([i for i in r.json["output"] if i["type"] == "function_call"][0]["arguments"])

    def thinking(self, r):
        out = []
        if r.stream:
            items = [d["item"] for _, d in r.events
                     if isinstance(d, dict) and d.get("type") == "response.output_item.done"
                     and d["item"]["type"] == "reasoning"]
        else:
            items = [i for i in r.json["output"] if i["type"] == "reasoning"]
        for it in items:
            out.append(("".join(s["text"] for s in it.get("summary", [])), it.get("encrypted_content")))
        return out

    def tool_script(self, tool_args):
        return {"text": "calling", "tool_call": {"name": "write_file", "args": tool_args}}

    def cases(self, t=TEXT):
        U = lambda x: self.msg("user", x)  # noqa

        def hist(args=None, output="ok"):
            return {"store": False, "input": [
                U("go"),
                {"type": "function_call", "call_id": "call_1", "name": "read",
                 "arguments": json.dumps(args if args is not None else {"p": "x"}, ensure_ascii=False)},
                {"type": "function_call_output", "call_id": "call_1", "output": output},
                U("thanks")]}

        def fc_args(up):
            return find(up, "function_call")[0]["arguments"]

        return [
            Case("instructions", {"instructions": t, "input": [U("hi")]}, lambda up: dumps(up.get("instructions"))),
            Case("input_string", {"input": t}, lambda up: dumps(up["input"])),
            Case("message_input_text", {"input": [U(t)]}, lambda up: dumps(up["input"])),
            Case("assistant_output_text_history", {"input": [U("q"), self.msg("assistant", t), U("go on")]},
                 lambda up: dumps(up["input"][1])),
            Case("function_call_output", hist(output=t), lambda up: dumps(find(up, "function_call_output"))),
            Case("function_call_arguments", hist(args=ARG_TMPL), fc_args,
                 lambda up: _check_args_struct(json.loads(fc_args(up)))),
        ]


ANTHROPIC, CHAT, RESPONSES = Anthropic(), Chat(), Responses()
PROTOS = [ANTHROPIC, CHAT, RESPONSES]


def person_and_email_tokens(up_text: str):
    toks = tokens(up_text)
    p = next((t for t in toks if t.startswith("<PERSON_")), None)
    e = next((t for t in toks if t.startswith("<EMAIL_ADDRESS_")), None)
    return p, e


# --- stack control ------------------------------------------------------------------------
def sh(cmd: str, env_extra: dict | None = None, timeout=300):
    import os
    env = {**os.environ, **(env_extra or {})}
    p = subprocess.run(["bash", "-c", cmd], cwd=ROOT, capture_output=True, text=True, timeout=timeout, env=env)
    return p


def stop_service(name: str):
    """stop exactly the pid recorded in .run/<name>.pid (scripts/_env.sh stop_pid)"""
    assert name in {"palena", "redis"}
    sh(f'source scripts/_env.sh; stop_pid {name}')


def stack_up():
    p = sh("scripts/up.sh", timeout=400)
    assert p.returncode == 0, p.stdout + p.stderr
    wait_ready()


def restart_palena():
    p = sh("scripts/restart-palena.sh", {"NOBUILD": "1"})
    assert p.returncode == 0, p.stdout + p.stderr
    wait_ready()


def restart_redis():
    stop_service("redis")
    time.sleep(0.5)
    stack_up()  # up.sh only starts what is not running


def wait_ready(timeout=90):
    end = time.time() + timeout
    while time.time() < end:
        try:
            if (httpx.get(f"{PALENA}/readyz", timeout=3).status_code == 200
                    and httpx.get(f"{LITELLM}/health/liveliness", timeout=3).status_code == 200):
                return
        except Exception:
            pass
        time.sleep(1)
    raise AssertionError("stack not ready")


def redis_client():
    import redis
    return redis.Redis(port=REDIS_PORT, decode_responses=True)


def redis_keys() -> set[str]:
    return set(redis_client().scan_iter("*", count=1000))
