"""Post-masking leak scan: does any real value of the session mapping still appear in the
outgoing request body?  (docs/OBSERVABILITY.md)

Matching is case-insensitive on *word boundaries* (Unicode-aware): "Ivan" does not hit inside
"Ivanov", "contact" does not hit inside "contacts"; e-mails and phone numbers are whole tokens
("a@b.com" does not hit inside "xa@b.com").

Cost: one json.dumps + lower() of the body, one C-level substring pre-filter per mapped value
(plain ``in``; handles removed first so a value like "PERSON" cannot match a handle), and a
boundary regex only for the values that pre-filter hit.  The JSON path of each hit is found by
a second walk over the body, only when something was found."""
from __future__ import annotations

import json
import re
from typing import Any

_HANDLE_RE = re.compile(r"<[A-Z][A-Z_]*_\d+>")
# keys of the LiteLLM request dict that are not part of the provider body (raw client body, auth, headers...)
INTERNAL_KEYS = frozenset({
    "proxy_server_request", "metadata", "litellm_metadata", "secret_fields", "litellm_logging_obj",
    "litellm_call_id", "litellm_trace_id", "litellm_session_id", "api_key", "api_base", "headers",
    "extra_headers", "user_api_key_dict", "request_timeout", "stream_options_internal",
})


def outgoing_body(data: dict) -> dict:
    return {k: v for k, v in data.items()
            if k not in INTERNAL_KEYS and not k.startswith(("litellm_", "user_api_key"))}


def _forms(value: str) -> list[str]:
    v = value.strip().lower()
    esc = json.dumps(v, ensure_ascii=False)[1:-1]
    return [v] if esc == v else [v, esc]


def _boundary_re(form: str) -> re.Pattern:
    """Whole-token regex for an already lower-cased value."""
    body = re.escape(form)
    if "@" in form:  # e-mail: not glued to a longer address
        return re.compile(r"(?<![\w@.+\-])" + body + r"(?![\w@\-]|\.\w)")
    if form[:1] in "+(" or form[:1].isdigit():  # phone-like: not glued to more digits / a "+" prefix
        return re.compile(r"(?<![\w+])" + body + r"(?!\w)")
    return re.compile(r"(?<!\w)" + body + r"(?!\w)")


def find_leaks(body: Any, mapping: dict[str, str], min_len: int = 3, max_hits: int = 50) -> list[dict]:
    """mapping: real -> handle.  -> [{"handle", "path"}] (no real values)."""
    cands = [(f, h) for real, h in mapping.items()
             if isinstance(real, str) and len(real.strip()) >= min_len for f in _forms(real)]
    if not cands:
        return []
    blob = _HANDLE_RE.sub("", json.dumps(body, ensure_ascii=False, default=str)).lower()
    hits = [(_boundary_re(f), h) for f, h in cands if f in blob]
    hits = [(rx, h) for rx, h in hits if rx.search(blob)]
    if not hits:
        return []
    out: list[dict] = []
    seen: set[tuple[str, str]] = set()

    def which(s: str) -> list[tuple[str, str]]:
        sl = _HANDLE_RE.sub("", s).lower()
        esc = None
        res = []
        for rx, h in hits:
            if rx.search(sl):
                res.append((rx, h))
                continue
            if esc is None:
                esc = json.dumps(sl, ensure_ascii=False)[1:-1]
            if esc != sl and rx.search(esc):
                res.append((rx, h))
        return res

    def rec(node: Any, path: str) -> None:
        if len(out) >= max_hits:
            return
        if isinstance(node, str):
            for _, h in which(node):
                if (h, path) not in seen:
                    seen.add((h, path))
                    out.append({"handle": h, "path": path})
        elif isinstance(node, dict):
            for k, v in node.items():
                p = f"{path}.{k}" if path else str(k)
                if isinstance(k, str):
                    for _, h in which(k):
                        if (h, p + "#key") not in seen:
                            seen.add((h, p + "#key"))
                            out.append({"handle": h, "path": p + "#key"})
                rec(v, p)
        elif isinstance(node, (list, tuple)):
            for i, v in enumerate(node):
                rec(v, f"{path}[{i}]")

    rec(body, "")
    if not out:  # matched only across serialisation (e.g. escapes): report without a path
        seen_h = set()
        for _, h in hits:
            if h not in seen_h:
                seen_h.add(h)
                out.append({"handle": h, "path": "?"})
    return out
