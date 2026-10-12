"""Bridge hook behaviour with a fake Palena (httpx MockTransport): fail-closed, guard, session."""
import asyncio
import json

import httpx
import pytest
from fastapi import HTTPException

import pii_bridge as pb


def bridge(handler):
    b = pb.PiiBridge()
    b._http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    b.cache_on = False
    return b


def ok_upper(request):
    j = json.loads(request.content)
    return httpx.Response(200, json={"action": "GUARDRAIL_INTERVENED", "texts": [t.upper() for t in j["texts"]]})


def pre(b, data, ct="acompletion"):
    return asyncio.run(b.async_pre_call_hook(None, None, data, ct))


def test_single_call_and_session_is_trace_id_not_call_id():
    seen = []

    def h(req):
        seen.append(json.loads(req.content))
        return ok_upper(req)
    b = bridge(h)
    data = {"litellm_call_id": "CALL", "litellm_trace_id": "sess-1",
            "messages": [{"role": "user", "content": "a"}, {"role": "user", "content": "b"}]}
    pre(b, data)
    assert len(seen) == 1 and seen[0]["texts"] == ["a", "b"] and seen[0]["input_type"] == "request"
    assert seen[0]["litellm_trace_id"] == "sess-1" and seen[0]["litellm_call_id"] == "CALL"
    assert data["messages"][1]["content"] == "B"


@pytest.mark.parametrize("resp", [
    lambda r: httpx.Response(500, text="boom"),
    lambda r: httpx.Response(200, text="not json"),
    lambda r: httpx.Response(200, json={"action": "GUARDRAIL_INTERVENED", "texts": ["only one"]}),
])
def test_fail_closed_503(resp):
    b = bridge(resp)
    with pytest.raises(HTTPException) as e:
        pre(b, {"litellm_trace_id": "s", "messages": [{"role": "user", "content": "a"}, {"role": "user", "content": "b"}]})
    assert e.value.status_code == 503


def test_unreachable_503():
    def h(req):
        raise httpx.ConnectError("down")
    with pytest.raises(HTTPException) as e:
        pre(bridge(h), {"litellm_trace_id": "s", "messages": [{"role": "user", "content": "a"}]})
    assert e.value.status_code == 503


def test_blocked_400():
    b = bridge(lambda r: httpx.Response(200, json={"action": "BLOCKED", "blocked_reason": "nope"}))
    with pytest.raises(HTTPException) as e:
        pre(b, {"litellm_trace_id": "s", "messages": [{"role": "user", "content": "a"}]})
    assert e.value.status_code == 400


def test_unsupported_call_type_with_content_blocked_but_empty_ok():
    b = bridge(ok_upper)
    for ct, d in [("aembedding", {"input": "x"}), ("atext_completion", {"prompt": "x"}),
                  ("_arealtime", {"messages": [{"role": "user", "content": "x"}]})]:
        with pytest.raises(HTTPException) as e:
            pre(b, d, ct)
        assert 400 <= e.value.status_code < 500
    assert pre(b, {"model": "m"}, "aembedding") == {"model": "m"}


def test_compact_is_processed_with_responses_walker():
    b = bridge(ok_upper)
    d = {"litellm_trace_id": "s", "input": [{"type": "message", "role": "user", "content": "hi"},
                                              {"type": "compaction", "encrypted_content": "gAAA"}]}
    pre(b, d, "acompact_responses")
    assert d["input"][0]["content"] == "HI" and d["input"][1]["encrypted_content"] == "gAAA"


def test_no_texts_no_call():
    def h(req):
        raise AssertionError("must not call palena")
    pre(bridge(h), {"litellm_trace_id": "s", "messages": [{"role": "user", "content": ""}]})


def test_cache_skips_second_call():
    calls = []

    def h(req):
        calls.append(1)
        return ok_upper(req)
    b = bridge(h)
    b.cache_on = True
    for _ in range(2):
        pre(b, {"litellm_trace_id": "s", "messages": [{"role": "user", "content": "same"}]})
    assert len(calls) == 1
    pre(b, {"litellm_trace_id": "other", "messages": [{"role": "user", "content": "same"}]})
    assert len(calls) == 2


def test_session_derivation_order():
    d = pb.derive_session
    assert d({"litellm_trace_id": "T", "litellm_session_id": "S"}) == ("T", "litellm_trace_id")
    assert d({"litellm_session_id": "S"})[0] == "S"
    assert d({"metadata": {"session_id": "M"}})[0] == "M"
    assert d({"proxy_server_request": {"headers": {"x-claude-code-session-id": "abcdef12-0000"}}})[0] == "abcdef12-0000"
    assert d({"litellm_call_id": "CALL"}, None)[1] == "api_key_fallback"
    assert pb.sanitize_session("ok-id") == "ok-id"
    assert pb.sanitize_session("x" * 300).startswith("hashed-")


def test_post_call_restores_via_palena_response():
    seen = []

    def h(req):
        j = json.loads(req.content)
        seen.append(j)
        return httpx.Response(200, json={"action": "GUARDRAIL_INTERVENED", "texts": ["Bob" for _ in j["texts"]]})
    b = bridge(h)
    resp = {"content": [{"type": "text", "text": "<PERSON_1>"}]}
    out = asyncio.run(b.async_post_call_success_hook({"litellm_trace_id": "s"}, None, resp))
    assert out["content"][0]["text"] == "Bob" and seen[0]["input_type"] == "response"


# ------------------------------------------------------------------ pattern-only system text
def _capture():
    seen = []

    def h(req):
        seen.append(json.loads(req.content))
        return ok_upper(req)
    return seen, h


ANTH = {"litellm_trace_id": "s", "system": [{"type": "text", "text": "sys"}],
        "messages": [{"role": "user", "content": "usr"}]}


def test_system_pattern_only_indices_sent_to_palena():
    seen, h = _capture()
    import copy
    pre(bridge(h), copy.deepcopy(ANTH), "anthropic_messages")
    assert seen[0]["texts"] == ["sys", "usr"]
    assert seen[0]["additional_provider_specific_params"]["pattern_only_indices"] == [0]


def test_system_pattern_only_disabled_env_flag():
    seen, h = _capture()
    import copy
    b = bridge(h)
    b.system_pattern_only = False
    pre(b, copy.deepcopy(ANTH), "anthropic_messages")
    assert "pattern_only_indices" not in seen[0]["additional_provider_specific_params"]


def test_system_pattern_only_env_default_and_off(monkeypatch):
    assert pb.PiiBridge().system_pattern_only is True
    monkeypatch.setenv("PII_BRIDGE_SYSTEM_PATTERN_ONLY", "0")
    assert pb.PiiBridge().system_pattern_only is False


def test_cache_key_separates_pattern_only_mode():
    seen, h = _capture()
    b = bridge(h)
    b.cache_on = True
    import copy
    same = {"litellm_trace_id": "s", "system": "same text", "messages": [{"role": "user", "content": "same text"}]}
    pre(b, copy.deepcopy(same), "anthropic_messages")
    # one unique text per mode -> both sent, with the flag only on the system copy
    assert seen[0]["texts"] == ["same text", "same text"]
    assert seen[0]["additional_provider_specific_params"]["pattern_only_indices"] == [0]
    pre(b, copy.deepcopy(same), "anthropic_messages")
    assert len(seen) == 1  # fully cached on replay
