"""8. Unguarded routes must not leak canaries (blocked, or masked)."""
import pytest

from helpers import (ANTHROPIC, KEY, RESPONSES, TEXT, assert_no_canary, http_post, new_sid, upstream)

H_ANTH = {"anthropic-version": "2023-06-01", "x-api-key": KEY}


def _outcome(path, headers, body, sid):
    r = http_post(path, headers, body)
    reqs = upstream()
    assert_no_canary(reqs)  # either nothing reached upstream or what did is masked
    return r, reqs


def test_responses_compact(sid):
    h = RESPONSES.headers(sid)
    body = {"model": "mock-gpt", "input": [RESPONSES.msg("user", TEXT)]}
    r, reqs = _outcome("/v1/responses/compact", h, body, sid)
    assert r.status >= 400 or reqs, "neither blocked nor forwarded?"


def test_anthropic_passthrough_prefix(sid):
    h = {**H_ANTH, "x-claude-code-session-id": sid}
    body = {"model": "mock-claude", "max_tokens": 32, "messages": [{"role": "user", "content": TEXT}]}
    _outcome("/anthropic/v1/messages", h, body, sid)


def test_anthropic_count_tokens(sid):
    h = {**H_ANTH, "x-claude-code-session-id": sid}
    body = {"model": "mock-claude", "messages": [{"role": "user", "content": TEXT}]}
    _outcome("/v1/messages/count_tokens", h, body, sid)


def test_openai_prefixed_route(sid):
    h = {"x-session-id": sid}
    body = {"model": "mock-gpt", "messages": [{"role": "user", "content": TEXT}]}
    _outcome("/openai/v1/chat/completions", h, body, sid)


def test_chat_completions_alt_route(sid):
    h = {"x-session-id": sid}
    body = {"model": "mock-gpt", "messages": [{"role": "user", "content": TEXT}]}
    _outcome("/chat/completions", h, body, sid)


def test_legacy_completions(sid):
    h = {"x-session-id": sid}
    body = {"model": "mock-gpt", "prompt": TEXT}
    _outcome("/v1/completions", h, body, sid)
