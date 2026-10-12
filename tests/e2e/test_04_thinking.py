"""4. thinking / reasoning blocks: byte-identical on request, unchanged on response."""
import re

import pytest

from helpers import ANTHROPIC, CHAT, RESPONSES, NAME_EN, TEXT, TOKEN_RE, assert_no_canary, find, mock_script, upstream

THINK = "Let me think:\t  tabs, unicode ✓ é — 日本語, lookalike <PERSON_9> and trailing space  "
SIG = "EuYBCkQYAiJA+/Ab12=="  # opaque base64-ish
REDACTED = "EmwKAhgBEgy3va3pzix/LafPsn4aDFIT90qSyvr+3GJ2CIwWA=="
ENC = "gAAAAB" + "Zx9_-" * 40 + "=="
SUMMARY = "Summary with  double  spaces ✓"

MODES = [False, True]


def test_anthropic_request_thinking_byte_identical(sid):
    body = ANTHROPIC.history_thinking(TEXT, THINK, SIG, REDACTED)
    r = ANTHROPIC.send(sid, body)
    assert r.status == 200, r.raw[:500]
    up = ANTHROPIC.last_up()
    ANTHROPIC.check_thinking_passthrough(up, THINK, SIG, REDACTED)
    assert_no_canary(upstream())
    assert TOKEN_RE.search(str(up["messages"][-1]))  # user text still masked


def test_responses_request_reasoning_byte_identical(sid):
    item = {"type": "reasoning", "id": "rs_0123456789", "summary": [{"type": "summary_text", "text": SUMMARY}],
            "encrypted_content": ENC}
    body = {"store": False, "include": ["reasoning.encrypted_content"], "input": [
        RESPONSES.msg("user", "Question one"), item, RESPONSES.msg("assistant", "Answer one"),
        RESPONSES.msg("user", TEXT)]}
    r = RESPONSES.send(sid, body)
    assert r.status == 200, r.raw[:500]
    got = find(RESPONSES.last_up(), "reasoning")
    assert got, "reasoning item dropped upstream"
    assert got[0]["encrypted_content"] == ENC
    assert got[0]["summary"] == item["summary"]
    assert got[0]["id"] == item["id"]
    assert_no_canary(upstream())


def test_responses_compaction_item_passthrough(sid):
    comp = {"type": "compaction", "id": "cmp_1", "encrypted_content": "gAAAAcmp" + "ab" * 50}
    body = {"store": False, "input": [RESPONSES.msg("user", "q"), comp, RESPONSES.msg("user", TEXT)]}
    r = RESPONSES.send(sid, body)
    assert r.status == 200, r.raw[:500]
    got = find(RESPONSES.last_up(), "compaction")
    assert got and got[0]["encrypted_content"] == comp["encrypted_content"]
    assert_no_canary(upstream())


@pytest.mark.parametrize("stream", MODES, ids=["nonstream", "stream"])
@pytest.mark.parametrize("proto", [ANTHROPIC, RESPONSES, CHAT], ids=lambda p: p.name)
def test_response_thinking_unchanged(proto, stream, sid):
    mock_script({"thinking": "Pondering the options carefully", "text": "ok done"})
    r = proto.send(sid, proto.body([("user", f"Hi I am {NAME_EN}")], thinking=True), stream=stream)
    assert r.status == 200, r.raw[:500]
    th = proto.thinking(r)
    assert th, f"no thinking/reasoning reached client: {r.raw[:600]}"
    text, sig = th[0]
    assert text == "Pondering the options carefully"
    if proto is ANTHROPIC:
        assert re.fullmatch(r"sig_[0-9a-f]{32}==", sig), sig
    elif proto is RESPONSES:
        assert re.fullmatch(r"gAAAA[0-9a-f]{64}", sig), sig
    assert proto.text(r) == "ok done"
