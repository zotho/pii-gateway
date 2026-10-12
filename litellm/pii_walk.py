"""Pure, dependency-free protocol walkers for the PII bridge.

A walker visits exactly the fields listed in docs/DESIGN.md ("What the bridge rewrites"),
collects their string values in a stable order (``Plan.texts``) and writes replacements
back *in place* (``Plan.apply(new_texts)``).  Everything else (thinking, redacted_thinking,
signature, encrypted_content, reasoning, compaction, ids, cache_control, images, tools
definitions ...) is never visited, hence never modified.

JSON-bearing fields (``tool_use.input`` objects, ``function_call.arguments`` strings) are
walked down to their string *leaves* (values only, never keys) so the result stays valid JSON.
An ``arguments`` string is only re-serialised when at least one leaf actually changed, so
unchanged history stays byte-identical (prompt-cache friendly).

"Pattern-only" slots (``Plan.pattern_only``): with ``system_pattern_only=True`` the request walker
marks agent system/developer text (Anthropic ``system``, chat role system/developer, Responses
``instructions`` and system/developer input messages) so Palena skips NER-derived entities there
(product names in vendor prompts are NER false positives) while pattern recognizers and
already-known session values still apply.  User messages and tool results are never marked.

Request walkers:  collect_request(protocol, body, system_pattern_only=False) -> Plan
Response walkers: collect_response(protocol, resp) -> Plan   (resp: dict or attribute object)
"""
from __future__ import annotations

import contextlib
import json
from typing import Any, Callable

ANTHROPIC = "anthropic"
CHAT = "chat"
RESPONSES = "responses"

_CALL_TYPES = {
    "anthropic_messages": ANTHROPIC,
    "aanthropic_messages": ANTHROPIC,
    "acompletion": CHAT,
    "completion": CHAT,
    "aresponses": RESPONSES,
    "responses": RESPONSES,
    "acompact_responses": RESPONSES,  # request side is a normal Responses `input`; output is opaque
    "compact_responses": RESPONSES,
}


def protocol_for_call_type(call_type: Any) -> str | None:
    return _CALL_TYPES.get(getattr(call_type, "value", call_type))


# ----------------------------------------------------------------- dict-or-object access
def _get(o: Any, k: str, default: Any = None) -> Any:
    if isinstance(o, dict):
        return o.get(k, default)
    return getattr(o, k, default)


def _set(o: Any, k: str, v: Any) -> None:
    if isinstance(o, dict):
        o[k] = v
    else:
        setattr(o, k, v)


# ----------------------------------------------------------------- plan
class Plan:
    def __init__(self) -> None:
        self._slots: list[tuple[str, Callable[[str], None]]] = []
        self._finalizers: list[Callable[[], None]] = []
        self._po: list[bool] = []   # parallel to _slots: pattern-only flag
        self._po_on = False         # applied to slots added while True

    @contextlib.contextmanager
    def pattern_only_scope(self, on: bool = True):
        prev, self._po_on = self._po_on, bool(on)
        try:
            yield
        finally:
            self._po_on = prev

    @property
    def pattern_only(self) -> list[int]:
        """Indices into ``texts`` that should be analysed pattern-only."""
        return [i for i, f in enumerate(self._po) if f]

    @property
    def texts(self) -> list[str]:
        return [t for t, _ in self._slots]

    def __len__(self) -> int:
        return len(self._slots)

    def add(self, text: Any, setter: Callable[[str], None]) -> None:
        if isinstance(text, str) and text.strip():
            self._slots.append((text, setter))
            self._po.append(self._po_on)

    def add_field(self, container: Any, key: str) -> None:
        v = _get(container, key)
        self.add(v, lambda n, c=container, k=key: _set(c, k, n))

    def add_json_value(self, node: Any) -> None:
        """All string leaves of an already-parsed JSON value, mutated in place."""
        if isinstance(node, dict):
            for k in list(node.keys()):
                v = node[k]
                if isinstance(v, str):
                    self.add(v, lambda n, c=node, kk=k: c.__setitem__(kk, n))
                else:
                    self.add_json_value(v)
        elif isinstance(node, list):
            for i, v in enumerate(node):
                if isinstance(v, str):
                    self.add(v, lambda n, c=node, ii=i: c.__setitem__(ii, n))
                else:
                    self.add_json_value(v)

    def add_json_string_field(self, container: Any, key: str) -> None:
        """A field holding a JSON document as a string (function_call.arguments)."""
        raw = _get(container, key)
        if not isinstance(raw, str) or not raw.strip():
            return
        try:
            obj = json.loads(raw)
        except ValueError:
            self.add_field(container, key)  # not JSON: treat as opaque text
            return
        if not isinstance(obj, (dict, list)):
            self.add_field(container, key)
            return
        changed = [False]
        sub = Plan()
        sub.add_json_value(obj)
        for text, setter in sub._slots:
            def s(n: str, text=text, setter=setter) -> None:
                if n != text:
                    changed[0] = True
                    setter(n)
            self._slots.append((text, s))
            self._po.append(self._po_on)

        def fin() -> None:
            if changed[0]:
                _set(container, key, json.dumps(obj, ensure_ascii=False, separators=(",", ":")))
        self._finalizers.append(fin)

    def apply(self, new_texts: list[str]) -> None:
        if len(new_texts) != len(self._slots):
            raise ValueError(f"length mismatch: {len(new_texts)} != {len(self._slots)}")
        for (old, setter), new in zip(self._slots, new_texts):
            if new != old:
                setter(new)
        for f in self._finalizers:
            f()


# ----------------------------------------------------------------- shared helpers
def _text_parts(plan: Plan, content: Any, part_types: tuple[str, ...] = ("text",)) -> None:
    """content: str or list of parts; visit `text` of text-like parts only."""
    if isinstance(content, list):
        for p in content:
            if _get(p, "type") in part_types:
                plan.add_field(p, "text")


# ----------------------------------------------------------------- requests
_SYSTEM_ROLES = ("system", "developer")


def _anthropic_request(plan: Plan, body: dict, spo: bool = False) -> None:
    system = body.get("system")
    with plan.pattern_only_scope(spo):
        if isinstance(system, str):
            plan.add_field(body, "system")
        else:
            _text_parts(plan, system)
    for m in body.get("messages") or []:
        # Claude Code also sends vendor-injected role=system messages (skills/plugins listing)
        with plan.pattern_only_scope(spo and m.get("role") in _SYSTEM_ROLES):
            _anthropic_message(plan, m)


def _anthropic_message(plan: Plan, m: dict) -> None:
    content = m.get("content")
    if isinstance(content, str):
        plan.add_field(m, "content")
        return
    for b in content or []:
        t = _get(b, "type")
        if t == "text":
            plan.add_field(b, "text")
        elif t == "tool_result":
            c = _get(b, "content")
            if isinstance(c, str):
                plan.add_field(b, "content")
            else:
                _text_parts(plan, c)
        elif t == "tool_use":
            inp = _get(b, "input")
            if isinstance(inp, (dict, list)):
                plan.add_json_value(inp)
            # thinking / redacted_thinking / image / document / others: untouched


def _chat_request(plan: Plan, body: dict, spo: bool = False) -> None:
    for m in body.get("messages") or []:
        content = m.get("content")
        with plan.pattern_only_scope(spo and m.get("role") in _SYSTEM_ROLES):
            if isinstance(content, str):
                plan.add_field(m, "content")
            else:
                _text_parts(plan, content)
        for tc in m.get("tool_calls") or []:
            fn = _get(tc, "function")
            if fn is not None:
                plan.add_json_string_field(fn, "arguments")
        fc = m.get("function_call")
        if isinstance(fc, dict):
            plan.add_json_string_field(fc, "arguments")
        # reasoning_content / thinking_blocks: untouched


_RESP_TEXT_PARTS = ("input_text", "output_text", "text")


def _responses_request(plan: Plan, body: dict, spo: bool = False) -> None:
    with plan.pattern_only_scope(spo):
        plan.add_field(body, "instructions")
    inp = body.get("input")
    if isinstance(inp, str):
        plan.add_field(body, "input")
        return
    for it in inp or []:
        if not isinstance(it, dict):
            continue
        t = it.get("type") or ("message" if "role" in it else None)
        if t == "message":
            c = it.get("content")
            with plan.pattern_only_scope(spo and it.get("role") in _SYSTEM_ROLES):
                if isinstance(c, str):
                    plan.add_field(it, "content")
                else:
                    _text_parts(plan, c, _RESP_TEXT_PARTS)
        elif t == "function_call":
            plan.add_json_string_field(it, "arguments")
        elif t in ("function_call_output", "custom_tool_call_output"):
            o = it.get("output")
            if isinstance(o, str):
                plan.add_field(it, "output")
            else:
                _text_parts(plan, o, _RESP_TEXT_PARTS)
        elif t == "custom_tool_call":
            plan.add_field(it, "input")
        # reasoning / compaction / item_reference / others: untouched


_REQUEST = {ANTHROPIC: _anthropic_request, CHAT: _chat_request, RESPONSES: _responses_request}


def collect_request(protocol: str, body: dict, system_pattern_only: bool = False) -> Plan:
    plan = Plan()
    _REQUEST[protocol](plan, body, system_pattern_only)
    return plan


# ----------------------------------------------------------------- responses
def _anthropic_response(plan: Plan, resp: Any) -> None:
    for b in _get(resp, "content") or []:
        t = _get(b, "type")
        if t == "text":
            plan.add_field(b, "text")
        elif t == "tool_use":
            inp = _get(b, "input")
            if isinstance(inp, (dict, list)):
                plan.add_json_value(inp)


def _chat_response(plan: Plan, resp: Any) -> None:
    for ch in _get(resp, "choices") or []:
        msg = _get(ch, "message")
        if msg is None:
            continue
        plan.add_field(msg, "content")
        for tc in _get(msg, "tool_calls") or []:
            fn = _get(tc, "function")
            if fn is not None:
                plan.add_json_string_field(fn, "arguments")


def _responses_response(plan: Plan, resp: Any) -> None:
    for it in _get(resp, "output") or []:
        t = _get(it, "type")
        if t == "message":
            for p in _get(it, "content") or []:
                if _get(p, "type") == "output_text":
                    plan.add_field(p, "text")
        elif t == "function_call":
            plan.add_json_string_field(it, "arguments")
        elif t == "custom_tool_call":
            plan.add_field(it, "input")


_RESPONSE = {ANTHROPIC: _anthropic_response, CHAT: _chat_response, RESPONSES: _responses_response}


def collect_response(protocol: str, resp: Any) -> Plan:
    plan = Plan()
    _RESPONSE[protocol](plan, resp)
    return plan
