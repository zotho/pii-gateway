"""PII bridge: protocol-aware pseudonymization for LiteLLM (see docs/DESIGN.md).

Registered with ``litellm_settings.callbacks: pii_bridge.proxy_handler_instance``.

* pre_call   : walk the request (Anthropic / OpenAI chat / OpenAI Responses), send ALL scanned
               strings to Palena in ONE call (input_type=request, litellm_trace_id=<session>),
               write the pseudonymized strings back in place.  Fail closed (503) on any error.
* post_call  : non-stream response -> same walk, Palena input_type=response (reversal).
* streaming  : restore locally.  The session's reverse map (pseudonym -> real) is read ONCE per
               stream straight from Redis (key ``<prefix>:<session>``, hash field ``mapping``,
               JSON {"real": "pseudo"}; Palena's Redis, same value Palena itself writes) and
               applied with a holdback buffer so a pseudonym split across SSE events is restored.
               Redis failure => the stream is passed through un-restored (nothing leaks; the
               user just sees pseudonyms) and an ERROR is logged.
* route guard: call types we cannot process safely and that carry content -> HTTP 400.

Session key: litellm_trace_id / litellm_session_id (LiteLLM derives them from
x-claude-code-session-id, Codex ``session-id``, ``x-session-id``, x-litellm-*-id headers), then
body metadata, then raw headers, then API-key alias/hash (WARN).  Never litellm_call_id.

Env: PII_BRIDGE_PALENA_URL (http://127.0.0.1:18080), PII_BRIDGE_PALENA_API_KEY,
PII_BRIDGE_REDIS_URL (redis://127.0.0.1:16379/0), PII_BRIDGE_REDIS_PREFIX (palena:pseudonymizer),
PII_BRIDGE_TIMEOUT (60), PII_BRIDGE_NO_RESTORE (SECRET; types never restored in streams), PII_BRIDGE_CACHE (1 = per-session text->pseudonymized LRU cache),
PII_BRIDGE_SYSTEM_PATTERN_ONLY (1 default; system/developer/instructions text is sent to Palena as
"pattern-only": no NER entities there, patterns + known session values still masked).

Observability (docs/OBSERVABILITY.md): audit events via pii_audit.AuditLog (PII_AUDIT_LOG, PII_AUDIT_KEY;
PII_AUDIT_REQUIRED=1 => a failed audit write refuses the request), leak scan of the outgoing body
(PII_BRIDGE_LEAK_ACTION=log|block), Prometheus metrics on PII_BRIDGE_METRICS_PORT.
"""
from __future__ import annotations

import asyncio
import hashlib
import json
import os
import re
import sys
import time
import uuid
from collections import Counter, OrderedDict
from typing import Any

import httpx
from fastapi import HTTPException

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # sibling modules regardless of cwd

from litellm._logging import verbose_proxy_logger as log  # noqa: E402
from litellm.integrations.custom_logger import CustomLogger  # noqa: E402

import pii_leak  # noqa: E402
import pii_metrics as M  # noqa: E402
import pii_stream  # noqa: E402
import pii_walk  # noqa: E402
from pii_audit import AuditLog  # noqa: E402

AUDIT = AuditLog()  # one per process; tests may monkeypatch pii_bridge.AUDIT
_HANDLE_TYPE = re.compile(r"^<([A-Z][A-Z_]*?)_\d+>$")

_CONTENT_KEYS = ("messages", "input", "prompt", "system", "instructions", "query", "documents", "contents")
_PRINTABLE = re.compile(r"^[\x20-\x7E]{1,256}$")


def handle_type(h: str) -> str:
    m = _HANDLE_TYPE.match(h)
    return m.group(1) if m else "UNKNOWN"


def principal_of(key_dict: Any) -> str:
    uid = getattr(key_dict, "user_id", None)
    return getattr(key_dict, "key_alias", None) or (uid if uid != "default_user_id" else None) or "master"


_REQ_ID_KEYS = ("x-request-id", "request-id", "openai-request-id", "x-oai-request-id", "anthropic-request-id")


def _pick_request_id(hdrs: Any) -> str | None:
    """First request-id-like header (case-insensitive; LiteLLM prefixes upstream headers with 'llm_provider-')."""
    try:
        low = {str(k).lower().removeprefix("llm_provider-"): v for k, v in dict(hdrs or {}).items()}
    except Exception:
        return None
    for k in _REQ_ID_KEYS:
        if low.get(k):
            return str(low[k])[:128]
    return None


def provider_request_id(*objs: Any, data: dict | None = None) -> str | None:
    """Best effort provider (upstream) request id, for audit correlation.  Where LiteLLM keeps the upstream
    response headers depends on the route:
      * Responses / chat, non-stream: response._hidden_params['additional_headers']['llm_provider-x-request-id']
      * any stream: the stream wrapper handed to the streaming hook (NOT its items) has the same _hidden_params
      * Anthropic /v1/messages, non-stream: the response is a plain dict; the raw httpx response is
        data['litellm_logging_obj'].model_call_details['httpx_response'].headers
    """
    try:
        for obj in objs:
            hp = getattr(obj, "_hidden_params", None) or (obj.get("_hidden_params") if isinstance(obj, dict) else None) or {}
            rid = _pick_request_id(hp.get("additional_headers")) or _pick_request_id(hp.get("headers"))
            if rid:
                return rid
        mcd = getattr(data.get("litellm_logging_obj"), "model_call_details", None) if data else None
        hr = (mcd or {}).get("httpx_response")
        if hr is not None:
            return _pick_request_id(getattr(hr, "headers", None))
    except Exception:
        pass
    return None


def sanitize_session(v: str) -> str:
    """Mirror of palena internal/session sanitize() so we can compute the Redis key."""
    v = v.strip()
    if _PRINTABLE.match(v):
        return v
    return "hashed-" + hashlib.sha256(v.encode()).hexdigest()[:32]


def _headers(data: dict) -> dict:
    for path in (("proxy_server_request", "headers"), ("metadata", "headers"), ("litellm_metadata", "headers")):
        cur: Any = data
        for p in path:
            cur = cur.get(p) if isinstance(cur, dict) else None
        if isinstance(cur, dict):
            return cur
    return {}


def derive_session(data: dict, user_api_key_dict: Any = None) -> tuple[str, str]:
    """-> (session_id, source)."""
    for key in ("litellm_trace_id", "litellm_session_id"):
        v = data.get(key)
        if isinstance(v, str) and v.strip():
            return v.strip(), key
    for mk in ("litellm_metadata", "metadata"):
        md = data.get(mk)
        if isinstance(md, dict):
            for k in ("trace_id", "session_id"):
                v = md.get(k)
                if isinstance(v, str) and v.strip():
                    return v.strip(), f"{mk}.{k}"
    hdrs = _headers(data)
    if hdrs:
        try:
            from litellm.proxy.litellm_pre_call_utils import get_chain_id_from_headers
            v = get_chain_id_from_headers(dict(hdrs))
            if v:
                return v, "headers"
        except Exception:  # pragma: no cover
            pass
        low = {str(k).lower(): v for k, v in hdrs.items()}
        v = low.get("x-litellm-session-id") or low.get("x-session-id") or low.get("session-id")
        if v:
            return v, "headers"
    alias = getattr(user_api_key_dict, "key_alias", None) or getattr(user_api_key_dict, "token", None) or "anon"
    return f"key:{alias}", "api_key_fallback"


class PiiBridge(CustomLogger):
    def __init__(self) -> None:
        super().__init__()
        self.palena_url = os.environ.get("PII_BRIDGE_PALENA_URL", "http://127.0.0.1:18080").rstrip("/")
        self.palena_key = os.environ.get("PII_BRIDGE_PALENA_API_KEY", "")
        self.redis_url = os.environ.get("PII_BRIDGE_REDIS_URL", "redis://127.0.0.1:16379/0")
        self.redis_prefix = os.environ.get("PII_BRIDGE_REDIS_PREFIX", "palena:pseudonymizer")
        self.timeout = float(os.environ.get("PII_BRIDGE_TIMEOUT", "60"))
        self.cache_on = os.environ.get("PII_BRIDGE_CACHE", "1") != "0"
        self.system_pattern_only = os.environ.get("PII_BRIDGE_SYSTEM_PATTERN_ONLY", "1") != "0"
        self._cache: OrderedDict[tuple[str, str], str] = OrderedDict()
        self._cache_max = 20000
        self._http: httpx.AsyncClient | None = None
        self._redis: Any = None
        self._warned_fallback = False
        self._seen_sessions: set[str] = set()
        self._t0: OrderedDict[str, float] = OrderedDict()  # call_id -> monotonic at end of pre-call
        self.leak_action = os.environ.get("PII_BRIDGE_LEAK_ACTION", "log").strip().lower()
        self.audit_required = os.environ.get("PII_AUDIT_REQUIRED", "0") == "1"
        M.start_server()
        # mirror Palena's NO_RESTORE_ENTITIES: those pseudonyms (e.g. <SECRET_1>) are never restored
        nr = os.environ.get("PII_BRIDGE_NO_RESTORE", os.environ.get("PALENA_PSEUDONYMIZER_NO_RESTORE_ENTITIES", "SECRET"))
        self.no_restore = tuple(f"<{t.strip()}_" for t in nr.split(",") if t.strip())

    # ------------------------------------------------------------------ plumbing
    def _client(self) -> httpx.AsyncClient:
        if self._http is None:
            self._http = httpx.AsyncClient(timeout=self.timeout)
        return self._http

    def _session(self, data: dict, key_dict: Any) -> str:
        sid, src = derive_session(data, key_dict)
        if sid not in self._seen_sessions and len(self._seen_sessions) < 10000:
            self._seen_sessions.add(sid)
            sh = {k: v for k, v in _headers(data).items()
                  if any(w in str(k).lower() for w in ("session", "trace", "thread", "conversation"))}
            log.info("pii_bridge: new session %s (source=%s) inbound session-ish headers=%s", sid, src, sh)
        if src == "api_key_fallback" and not self._warned_fallback:
            self._warned_fallback = True
            log.warning("pii_bridge: request has no session id (x-claude-code-session-id / session-id / "
                        "x-session-id); falling back to %s - all unsessioned traffic shares one mapping", sid)
        return sid

    async def _palena(self, input_type: str, texts: list[str], session: str, data: dict,
                      pattern_only: list[int] | None = None, timing: list | None = None) -> list[str]:
        extra: dict[str, Any] = {"source": "pii_bridge"}
        if pattern_only:
            extra["pattern_only_indices"] = pattern_only
        payload = {
            "input_type": input_type,
            "texts": texts,
            "litellm_call_id": str(data.get("litellm_call_id") or uuid.uuid4()),
            "litellm_trace_id": session,
            "additional_provider_specific_params": extra,
        }
        headers = {"x-api-key": self.palena_key} if self.palena_key else {}
        t0 = time.perf_counter()
        try:
            r = await self._client().post(f"{self.palena_url}/beta/litellm_basic_guardrail_api",
                                          json=payload, headers=headers)
            if timing is not None:
                timing.append(time.perf_counter() - t0)
            if input_type == "request":
                M.palena_seconds.observe(time.perf_counter() - t0)
        except httpx.HTTPError as e:
            log.error("pii_bridge: palena unreachable: %r", e)
            raise HTTPException(503, detail="PII gateway unavailable (pseudonymizer unreachable); request refused")
        if r.status_code != 200:
            log.error("pii_bridge: palena HTTP %s: %s", r.status_code, r.text[:300])
            raise HTTPException(503, detail=f"PII gateway error (pseudonymizer HTTP {r.status_code}); request refused")
        try:
            j = r.json()
        except ValueError:
            raise HTTPException(503, detail="PII gateway error (bad pseudonymizer response); request refused")
        action = j.get("action")
        if action == "BLOCKED":
            raise HTTPException(400, detail=f"Blocked by PII gateway: {j.get('blocked_reason') or 'blocked'}")
        if action == "NONE":
            return texts
        out = j.get("texts")
        if action != "GUARDRAIL_INTERVENED" or not isinstance(out, list) or len(out) != len(texts):
            log.error("pii_bridge: unexpected palena reply action=%r", action)
            raise HTTPException(503, detail="PII gateway error (unexpected pseudonymizer reply); request refused")
        return out

    async def _transform_request(self, texts: list[str], session: str, data: dict,
                                 pattern_only: list[int] | None = None, timing: list | None = None) -> list[str]:
        po = set(pattern_only or ())
        if not self.cache_on:
            return await self._palena("request", texts, session, data, sorted(po), timing)
        # a text is a unique (text, pattern_only?) pair: the same string yields different
        # output in the two modes, so the flag is part of the cache key
        items = [(t, i in po) for i, t in enumerate(texts)]
        result: dict[tuple[str, bool], str] = {}
        miss: list[tuple[str, bool]] = []

        def ck(it: tuple[str, bool]) -> tuple[str, str]:
            return (session, ("p:" if it[1] else "n:") + hashlib.sha256(it[0].encode()).hexdigest())

        for it in dict.fromkeys(items):
            hit = self._cache.get(ck(it))
            if hit is not None:
                self._cache.move_to_end(ck(it))
                result[it] = hit
            else:
                miss.append(it)
        if miss:
            out = await self._palena("request", [t for t, _ in miss], session, data,
                                     [i for i, (_, f) in enumerate(miss) if f], timing)
            for it, o in zip(miss, out):
                result[it] = o
                if len(it[0]) <= 1_000_000:
                    self._cache[ck(it)] = o
            while len(self._cache) > self._cache_max:
                self._cache.popitem(last=False)
        return [result[it] for it in items]

    # ------------------------------------------------------------------ audit
    async def _audit(self, event: str, **fields: Any) -> None:
        """Append an audit event.  A failure never breaks traffic unless PII_AUDIT_REQUIRED=1."""
        try:
            await asyncio.to_thread(AUDIT.append, event, **fields)
        except Exception as e:
            M.audit_errors_total.inc()
            log.error("pii_bridge: audit write failed (%s): %r", event, e)
            if self.audit_required:
                M.fail_closed_total.labels("audit_unavailable").inc()
                raise HTTPException(503, detail="PII gateway: audit log unavailable; request refused")

    async def _blocked(self, session: str | None, principal: str, call_id: str, route: str, reason: str,
                       **extra: Any) -> None:
        M.requests_total.labels(route, "blocked").inc()
        M.fail_closed_total.labels(reason).inc()
        try:
            await self._audit("request.blocked", session=session, principal=principal, call_id=call_id,
                              route=route, reason=reason, **extra)
        except HTTPException:
            pass  # already refusing this request

    # ------------------------------------------------------------------ hooks
    async def async_pre_call_hook(self, user_api_key_dict, cache, data: dict, call_type):
        route = str(getattr(call_type, "value", call_type))
        principal = principal_of(user_api_key_dict)
        call_id = str(data.get("litellm_call_id") or "")
        proto = pii_walk.protocol_for_call_type(call_type)
        if proto is None:
            if any(data.get(k) for k in _CONTENT_KEYS):
                await self._blocked(self._session(data, user_api_key_dict), principal, call_id, route,
                                    "unsupported_route")
                raise HTTPException(
                    400, detail=f"Route/call type '{route}' is not supported by the PII gateway "
                                f"(content would bypass pseudonymization). Use /v1/messages, "
                                f"/v1/chat/completions or /v1/responses.")
            return data
        plan = pii_walk.collect_request(proto, data, self.system_pattern_only)
        if len(plan):
            session = self._session(data, user_api_key_dict)
            before = await self._mapping(session)
            timing: list[float] = []
            try:
                out = await self._transform_request(plan.texts, session, data, plan.pattern_only, timing)
            except HTTPException as e:
                await self._blocked(session, principal, call_id, route,
                                    "palena_blocked" if e.status_code == 400 else "palena_unreachable")
                raise
            plan.apply(out)
            after = await self._mapping(session)
            await self._observe_request(session, principal, call_id, route, data, plan, out, before, after, timing)
            if call_id:
                self._t0[call_id] = time.monotonic()
                while len(self._t0) > 10000:
                    self._t0.popitem(last=False)
        return data

    async def _observe_request(self, session, principal, call_id, route, data, plan, out, before, after, timing):
        mapped = after or {}
        known = set(mapped.values())
        counts: Counter = Counter()
        for t in out:
            if "<" in t:
                for h in pii_stream.HANDLE_RE.findall(t):
                    if h in known or not mapped:
                        counts[handle_type(h)] += 1
        new = []
        if before is not None:
            for real, h in mapped.items():
                if real not in before:
                    ty = handle_type(h)
                    new.append({"handle": h, "type": ty, "fp": AUDIT.fp(ty, real)})
        suspects = pii_leak.find_leaks(pii_leak.outgoing_body(data), mapped) if mapped else []
        for ty, n in counts.items():
            M.entities_total.labels(ty).inc(n)
        for n_ in new:
            M.new_mappings_total.labels(n_["type"]).inc()
        if suspects:
            M.leak_suspects_total.labels(route).inc(len(suspects))
            log.error("pii_bridge: LEAK SUSPECTS session=%s route=%s %s", session, route, suspects[:5])
        block = bool(suspects) and self.leak_action == "block"
        M.requests_total.labels(route, "blocked" if block else "forwarded").inc()
        await self._audit("request.masked", session=session, principal=principal, call_id=call_id,
                          route=route, model=data.get("model"), texts=len(plan), pattern_only=len(plan.pattern_only),
                          entities=dict(counts), new=new, palena_ms=round(sum(timing) * 1000) if timing else 0,
                          leak_suspects=suspects)
        if block:
            M.fail_closed_total.labels("leak").inc()
            await self._audit("request.blocked", session=session, principal=principal, call_id=call_id,
                              route=route, reason="leak", leak_suspects=suspects)
            raise HTTPException(400, detail="Blocked by PII gateway: real value found in outgoing request "
                                            "after masking (leak guard)")

    async def _emit_restored(self, session, principal, call_id, stream, restored, kept, unknown,
                             status, resp=None, data=None) -> None:
        for ty, n in restored.items():
            M.restore_total.labels(ty, "restored").inc(n)
        for ty, n in kept.items():
            M.restore_total.labels(ty, "kept_masked").inc(n)
        if unknown:
            M.restore_total.labels("UNKNOWN", "unknown").inc(unknown)
        t0 = self._t0.pop(call_id, None)
        await self._audit("response.restored", session=session, principal=principal, call_id=call_id,
                          stream=stream, restored=dict(restored), kept_masked=dict(kept), unknown_handles=unknown,
                          upstream_status=status,
                          upstream_ms=round((time.monotonic() - t0) * 1000) if t0 else None,
                          provider_request_id=provider_request_id(*(resp if isinstance(resp, tuple) else (resp,)), data=data))

    async def async_post_call_success_hook(self, data: dict, user_api_key_dict, response):
        try:
            proto = self._response_protocol(response)
            if proto is None:
                return response
            plan = pii_walk.collect_response(proto, response)
            if len(plan):
                session = self._session(data, user_api_key_dict)
                texts = plan.texts
                plan.apply(await self._palena("response", texts, session, data))
                pseudo = set((await self._mapping(session) or {}).values())
                restored: Counter = Counter()
                kept: Counter = Counter()
                unknown = 0
                for t in texts:
                    for h in (pii_stream.HANDLE_RE.findall(t) if "<" in t else ()):
                        if h not in pseudo:
                            unknown += 1
                        elif h.startswith(self.no_restore):
                            kept[handle_type(h)] += 1
                        else:
                            restored[handle_type(h)] += 1
                await self._emit_restored(session, principal_of(user_api_key_dict),
                                          str(data.get("litellm_call_id") or ""), False,
                                          restored, kept, unknown, 200, response, data)
        except HTTPException:
            raise
        except Exception:
            log.exception("pii_bridge: response restore failed (response returned un-restored)")
        return response

    @staticmethod
    def _response_protocol(resp: Any) -> str | None:
        g = pii_walk._get
        if g(resp, "choices") is not None:
            return pii_walk.CHAT
        if isinstance(g(resp, "output"), list):
            return pii_walk.RESPONSES
        if isinstance(g(resp, "content"), list):
            return pii_walk.ANTHROPIC
        return None

    async def _mapping(self, session: str) -> dict[str, str] | None:
        """Forward mapping {real: handle} from Palena's Redis; None if Redis cannot be read."""
        key = f"{self.redis_prefix}:{sanitize_session(session)}"
        for attempt in (1, 2):
            try:
                if self._redis is None:
                    import redis.asyncio as aioredis
                    self._redis = aioredis.from_url(self.redis_url, decode_responses=True,
                                                    health_check_interval=15)
                raw = await self._redis.hget(key, "mapping")
                fwd = json.loads(raw) if raw else {}
                return {k: v for k, v in fwd.items() if isinstance(v, str) and v}
            except Exception:
                # stale pooled connection (server closed it): drop the client and retry once
                self._redis = None
                if attempt == 2:
                    log.exception("pii_bridge: cannot load mapping from redis")
        return None

    async def async_post_call_streaming_iterator_hook(self, user_api_key_dict, response, request_data: dict):
        session = self._session(request_data, user_api_key_dict)
        fwd = await self._mapping(session) or {}
        if not fwd:  # nothing to restore: pass through untouched
            first = None
            async for item in response:
                first = first if first is not None else item
                yield item
            await self._emit_restored(session, principal_of(user_api_key_dict),
                                      str(request_data.get("litellm_call_id") or ""), True, {}, {}, 0, 200, (response, first), request_data)
            return
        known = set(fwd.values())
        stats = pii_stream.RestoreStats()
        rev = pii_stream.StatDict({v: k for k, v in fwd.items() if not v.startswith(self.no_restore)}, stats)
        sse = pii_stream.AnthropicSseRestorer(rev)
        chat = pii_stream.ChatRestorer(rev)
        resp = pii_stream.ResponsesRestorer(rev)
        anth = pii_stream.AnthropicRestorer(rev)
        was_str = False
        first: Any = None
        async for item in response:
            if first is None:
                first = item
            try:
                if isinstance(item, (bytes, bytearray, str)):
                    was_str = isinstance(item, str)
                    out = sse.feed(bytes(item) if not was_str else item)
                    if out:
                        yield out.decode() if was_str else out
                    continue
                outs = self._restore_obj(item, chat, resp, anth)
            except Exception:
                log.exception("pii_bridge: stream restore failed for one item; passing it through")
                outs = [item]
            for o in outs:
                yield o
        rest = sse.finish()
        if rest:
            yield rest.decode() if was_str else rest
        for o in chat.finish():
            yield o
        restored: Counter = Counter()
        for h, n in stats.restored.items():
            restored[handle_type(h)] += n
        kept: Counter = Counter()
        unknown = 0
        for h, n in stats.leftover.items():
            if h in known:
                kept[handle_type(h)] += n
            else:
                unknown += n
        await self._emit_restored(session, principal_of(user_api_key_dict),
                                  str(request_data.get("litellm_call_id") or ""), True,
                                  restored, kept, unknown, 200, (response, first), request_data)

    @staticmethod
    def _restore_obj(item: Any, chat, resp, anth) -> list[Any]:
        g = pii_stream._g
        if g(item, "choices") is not None:
            return chat.process(item)
        t = pii_stream._etype(item)
        if isinstance(t, str) and t.startswith("response."):
            return resp.process(item)
        if isinstance(item, dict) and t:
            return anth.process(item)[0]
        return [item]


proxy_handler_instance = PiiBridge()
