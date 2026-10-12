"""Streaming restore (pseudonym -> real value) for the PII bridge. No litellm imports.

Core: ``Reverser`` replaces pseudonyms in a text stream and *holds back* any trailing fragment
that could still grow into a pseudonym (a proper prefix of one), so ``<PER`` + ``SON_1>``
arriving in two SSE events is restored correctly.  Only the raw, not-yet-decided tail is kept;
already-emitted output is never re-scanned.

Per-route restorers keep one Reverser per text stream (content block / choice / output item) and
rewrite events in place, preserving event structure and order:

* ``AnthropicRestorer``  /v1/messages        events (dicts) ; ``AnthropicSseRestorer`` adds SSE framing
* ``ChatRestorer``       /v1/chat/completions chunks (ModelResponseStream-like objects)
* ``ResponsesRestorer``  /v1/responses       event objects

Tool-argument streams (input_json_delta / function arguments) use json_mode: replacements are
JSON-string-escaped and the ``\\u003c``/``\\u003e`` spelling of ``<``/``>`` is matched as well.
Thinking / reasoning / signature events are never touched.
"""
from __future__ import annotations

import copy
import json
import re
from collections import Counter
from typing import Any

HANDLE_RE = re.compile(r"<[A-Z][A-Z_]*_\d+>")


class RestoreStats:
    """Optional counters filled by Reverser: restored[handle] and leftover[handle] (handles seen in
    the emitted text that were NOT restored: kept-masked or unknown). Audit/metrics only."""

    def __init__(self) -> None:
        self.restored: Counter = Counter()
        self.leftover: Counter = Counter()


class StatDict(dict):
    """reverse map (pseudonym -> real) that carries a RestoreStats for the Reversers built from it."""

    def __init__(self, data: dict, stats: RestoreStats):
        super().__init__(data)
        self.stats = stats


class Reverser:
    def __init__(self, rev: dict[str, str], json_mode: bool = False):
        pats: dict[str, str] = {}
        for pseudo, real in rev.items():
            if not pseudo:
                continue
            rep = json.dumps(real, ensure_ascii=False)[1:-1] if json_mode else real
            pats[pseudo] = rep
            if json_mode:
                alt = pseudo.replace("<", "\\u003c").replace(">", "\\u003e")
                if alt != pseudo:
                    pats[alt] = rep
        self._pats = pats
        self._stats: RestoreStats | None = getattr(rev, "stats", None)
        self._canon = {a: p for p in rev for a in (p, p.replace("<", "\\u003c").replace(">", "\\u003e"))}
        self._re = re.compile("|".join(re.escape(p) for p in sorted(pats, key=len, reverse=True))) if pats else None
        self._prefixes = {p[:i] for p in pats for i in range(1, len(p))}
        self._maxlen = max((len(p) for p in pats), default=0)
        self.tail = ""

    def full(self, s: str) -> str:
        """Stateless reversal of a complete string."""
        if not self._re or not s:
            return self._left(s)
        if self._stats is not None:
            return self._re.sub(lambda m: self._hit(m), s)
        return self._re.sub(lambda m: self._pats[m.group(0)], s)

    def _hit(self, m) -> str:
        self._stats.restored[self._canon.get(m.group(0), m.group(0))] += 1
        return self._pats[m.group(0)]

    def _left(self, seg: str) -> str:
        if self._stats is not None and "<" in seg:
            self._stats.leftover.update(HANDLE_RE.findall(seg))
        return seg

    def feed(self, s: str) -> str:
        if not self._re:
            return self._left(s)
        buf = self.tail + s
        out: list[str] = []
        pos = 0
        for m in self._re.finditer(buf):
            out.append(self._left(buf[pos:m.start()]))
            out.append(self._hit(m) if self._stats is not None else self._pats[m.group(0)])
            pos = m.end()
        rest = buf[pos:]
        k = 0
        for k in range(min(len(rest), self._maxlen - 1), 0, -1):
            if rest[-k:] in self._prefixes:
                break
        else:
            k = 0
        out.append(self._left(rest[:len(rest) - k]))
        self.tail = rest[len(rest) - k:]
        return "".join(out)

    def flush(self) -> str:
        t, self.tail = self.tail, ""
        return t


class _Reversers:
    """key -> Reverser factory; keeps the pseudonym map in one place."""

    def __init__(self, rev: dict[str, str]):
        self.rev = rev
        self._m: dict[Any, Reverser] = {}

    def get(self, key: Any, json_mode: bool = False) -> Reverser:
        r = self._m.get(key)
        if r is None:
            r = self._m[key] = Reverser(self.rev, json_mode)
        return r

    def pop_with_tail(self, pred) -> list[tuple[Any, str]]:
        out = []
        for k in [k for k in self._m if pred(k)]:
            t = self._m.pop(k).flush()
            if t:
                out.append((k, t))
        return out


# =============================================================== Anthropic /v1/messages
class AnthropicRestorer:
    """Operates on parsed event dicts. process(ev) -> (events, changed)."""

    def __init__(self, rev: dict[str, str]):
        self.rs = _Reversers(rev)

    def _flush_events(self, pred) -> list[dict]:
        evs = []
        for (idx, kind), tail in self.rs.pop_with_tail(pred):
            d = ({"type": "text_delta", "text": tail} if kind == "text"
                 else {"type": "input_json_delta", "partial_json": tail})
            evs.append({"type": "content_block_delta", "index": idx, "delta": d})
        return evs

    def process(self, ev: dict) -> tuple[list[dict], bool]:
        t = ev.get("type")
        if t == "content_block_delta":
            d = ev.get("delta") or {}
            dt = d.get("type")
            if dt == "text_delta" and isinstance(d.get("text"), str):
                new = self.rs.get((ev.get("index"), "text")).feed(d["text"])
            elif dt == "input_json_delta" and isinstance(d.get("partial_json"), str):
                old = d["partial_json"]
                d["partial_json"] = self.rs.get((ev.get("index"), "json"), True).feed(old)
                return [ev], d["partial_json"] != old
            else:
                return [ev], False  # thinking_delta, signature_delta, citations_delta ... untouched
            changed = new != d["text"]
            d["text"] = new
            return [ev], changed
        if t == "content_block_stop":
            extras = self._flush_events(lambda k: k[0] == ev.get("index"))
            return extras + [ev], bool(extras)
        if t in ("message_delta", "message_stop"):
            extras = self._flush_events(lambda k: True)
            return extras + [ev], bool(extras)
        return [ev], False


class AnthropicSseRestorer:
    """Bytes in -> bytes out. Re-frames arbitrary byte chunks into SSE events, restores only the
    events that change, passes every other event through byte-identical."""

    def __init__(self, rev: dict[str, str]):
        self.core = AnthropicRestorer(rev)
        self._buf = b""

    def feed(self, chunk: bytes | str) -> bytes:
        if isinstance(chunk, str):
            chunk = chunk.encode()
        self._buf += chunk
        out: list[bytes] = []
        while True:
            i = self._buf.find(b"\n\n")
            if i < 0:
                break
            raw, self._buf = self._buf[:i + 2], self._buf[i + 2:]
            out.append(self._one(raw))
        return b"".join(out)

    def finish(self) -> bytes:
        rest, self._buf = self._buf, b""
        return rest

    @staticmethod
    def _frame(ev: dict, with_event_line: bool) -> bytes:
        s = f"event: {ev.get('type')}\n" if with_event_line else ""
        return (s + "data: " + json.dumps(ev, ensure_ascii=False) + "\n\n").encode()

    def _one(self, raw: bytes) -> bytes:
        try:
            text = raw.decode()
            name = None
            data_lines = []
            for line in text.split("\n"):
                if line.startswith("event:"):
                    name = line[6:].strip()
                elif line.startswith("data:"):
                    data_lines.append(line[5:].lstrip(" "))
            ev = json.loads("\n".join(data_lines)) if data_lines else None
        except (UnicodeDecodeError, ValueError):
            return raw
        if not isinstance(ev, dict):
            return raw
        events, changed = self.core.process(ev)
        if not changed:
            return raw
        return b"".join(self._frame(e, name is not None) for e in events)


# =============================================================== OpenAI chat
def _g(o: Any, k: str) -> Any:
    return o.get(k) if isinstance(o, dict) else getattr(o, k, None)


def _s(o: Any, k: str, v: Any) -> None:
    if isinstance(o, dict):
        o[k] = v
    else:
        setattr(o, k, v)


class ChatRestorer:
    def __init__(self, rev: dict[str, str]):
        self.rs = _Reversers(rev)
        self._last: Any = None

    def process(self, chunk: Any) -> list[Any]:
        self._last = chunk
        for ch in _g(chunk, "choices") or []:
            idx = _g(ch, "index") or 0
            delta = _g(ch, "delta")
            if delta is None:
                continue
            c = _g(delta, "content")
            if isinstance(c, str) and c:
                new = self.rs.get((idx, "c")).feed(c)
                if new != c:
                    _s(delta, "content", new)
            for tc in _g(delta, "tool_calls") or []:
                fn = _g(tc, "function")
                a = _g(fn, "arguments") if fn is not None else None
                if isinstance(a, str) and a:
                    new = self.rs.get((idx, "t", _g(tc, "index") or 0), True).feed(a)
                    if new != a:
                        _s(fn, "arguments", new)
            if _g(ch, "finish_reason"):
                for _, tail in self.rs.pop_with_tail(lambda k, i=idx: k[0] == i and k[1] == "c"):
                    _s(delta, "content", (_g(delta, "content") or "") + tail)
        return [chunk]

    def finish(self) -> list[Any]:
        """Text tails never released by a finish_reason chunk (stream cut short)."""
        tails = self.rs.pop_with_tail(lambda k: k[1] == "c")
        if not tails or self._last is None:
            return []
        out = []
        for (idx, _), tail in tails:
            c = copy.deepcopy(self._last)
            for ch in _g(c, "choices") or []:
                if (_g(ch, "index") or 0) == idx:
                    _s(ch, "finish_reason", None)
                    d = _g(ch, "delta")
                    _s(d, "content", tail)
                    _s(d, "tool_calls", None)
                    _s(d, "role", None)
            out.append(c)
        return out


# =============================================================== OpenAI Responses
def _etype(ev: Any) -> str:
    t = _g(ev, "type")
    return getattr(t, "value", t) or ""


class ResponsesRestorer:
    def __init__(self, rev: dict[str, str]):
        self.rs = _Reversers(rev)
        self._plain = Reverser(rev)
        self._json = Reverser(rev, True)
        self._last_delta: dict[Any, Any] = {}

    def _item(self, item: Any) -> None:
        t = _g(item, "type")
        if t == "message":
            for p in _g(item, "content") or []:
                if _g(p, "type") == "output_text" and isinstance(_g(p, "text"), str):
                    _s(p, "text", self._plain.full(_g(p, "text")))
        elif t == "function_call" and isinstance(_g(item, "arguments"), str):
            _s(item, "arguments", self._json.full(_g(item, "arguments")))
        elif t == "custom_tool_call" and isinstance(_g(item, "input"), str):
            _s(item, "input", self._plain.full(_g(item, "input")))

    def _tail_event(self, key: Any, field: str) -> list[Any]:
        r = self.rs._m.pop(key, None)
        tail = r.flush() if r else ""
        tmpl = self._last_delta.pop(key, None)
        if not tail or tmpl is None:
            return []
        e = copy.deepcopy(tmpl)
        _s(e, field, tail)
        return [e]

    def process(self, ev: Any) -> list[Any]:
        t = _etype(ev)
        if t == "response.output_text.delta" and isinstance(_g(ev, "delta"), str):
            key = ("text", _g(ev, "item_id"), _g(ev, "content_index"))
            self._last_delta[key] = copy.deepcopy(ev)
            _s(ev, "delta", self.rs.get(key).feed(_g(ev, "delta")))
        elif t == "response.output_text.done":
            key = ("text", _g(ev, "item_id"), _g(ev, "content_index"))
            extra = self._tail_event(key, "delta")
            if isinstance(_g(ev, "text"), str):
                _s(ev, "text", self._plain.full(_g(ev, "text")))
            return extra + [ev]
        elif t == "response.function_call_arguments.delta" and isinstance(_g(ev, "delta"), str):
            key = ("args", _g(ev, "item_id"))
            self._last_delta[key] = copy.deepcopy(ev)
            _s(ev, "delta", self.rs.get(key, True).feed(_g(ev, "delta")))
        elif t == "response.function_call_arguments.done":
            key = ("args", _g(ev, "item_id"))
            extra = self._tail_event(key, "delta")
            if isinstance(_g(ev, "arguments"), str):
                _s(ev, "arguments", self._json.full(_g(ev, "arguments")))
            return extra + [ev]
        elif t == "response.custom_tool_call_input.delta" and isinstance(_g(ev, "delta"), str):
            key = ("custom", _g(ev, "item_id"))
            self._last_delta[key] = copy.deepcopy(ev)
            _s(ev, "delta", self.rs.get(key).feed(_g(ev, "delta")))
        elif t == "response.custom_tool_call_input.done":
            key = ("custom", _g(ev, "item_id"))
            extra = self._tail_event(key, "delta")
            if isinstance(_g(ev, "input"), str):
                _s(ev, "input", self._plain.full(_g(ev, "input")))
            return extra + [ev]
        elif t == "response.content_part.done":
            p = _g(ev, "part")
            if p is not None and _g(p, "type") == "output_text" and isinstance(_g(p, "text"), str):
                _s(p, "text", self._plain.full(_g(p, "text")))
        elif t in ("response.output_item.added", "response.output_item.done"):
            it = _g(ev, "item")
            if it is not None:
                self._item(it)
        elif t in ("response.completed", "response.incomplete", "response.failed"):
            r = _g(ev, "response")
            for it in (_g(r, "output") or []) if r is not None else []:
                self._item(it)
        return [ev]
