"""2. Response restoration: non-stream + stream (pseudonym split across 3-char SSE chunks)."""
import pytest

from helpers import (RICH, TEXT_RU, EMAIL, NAME_EN, NAME_RU, PHONE, PROTOS, TEXT, TOKEN_RE, assert_no_canary,
                     mock_script, person_and_email_tokens, upstream, dumps)

MODES = [False, True]


@pytest.mark.parametrize("stream", MODES, ids=["nonstream", "stream"])
@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
@pytest.mark.parametrize("lang", ["en", "ru", "mixed"])
def test_echo_restored(proto, stream, lang, sid):
    src = {"en": TEXT, "ru": TEXT_RU, "mixed": RICH}[lang]
    r = proto.send(sid, proto.body([("user", src)]), stream=stream)
    assert r.status == 200, r.raw[:500]
    assert_no_canary(upstream())
    text = proto.text(r)
    for real in [x for x in (NAME_RU, NAME_EN, EMAIL, PHONE) if x in src]:
        assert real in text, f"{real!r} not restored in: {text!r}"
    assert not TOKEN_RE.search(text), text
    assert not TOKEN_RE.search(r.raw), "pseudonym leaked in raw response/SSE"


def _prime(proto, sid):
    """turn 1: let the model 'see' the pseudonyms; returns (person_tok, email_tok)"""
    r = proto.send(sid, proto.body([("user", f"Contact {NAME_EN} at {EMAIL} about the release.")]))
    assert r.status == 200, r.raw[:300]
    p, e = person_and_email_tokens(dumps(proto.last_up()))
    assert p and e, "no tokens upstream"
    return p, e


@pytest.mark.parametrize("stream", MODES, ids=["nonstream", "stream"])
@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_tool_call_args_restored(proto, stream, sid):
    p, e = _prime(proto, sid)
    args = {"path": f"/home/{p}/notes.txt", "emails": [e], "count": 3, "ok": True}
    mock_script({**proto.tool_script(args), "text": f"Writing for {p}"})
    r = proto.send(sid, proto.body([("user", f"Contact {NAME_EN} at {EMAIL} about the release."),
                                    ("assistant", "Done."), ("user", "continue")]), stream=stream)
    assert r.status == 200, r.raw[:500]
    got = proto.tool_args(r)
    assert got == {"path": f"/home/{NAME_EN}/notes.txt", "emails": [EMAIL], "count": 3, "ok": True}, got
    assert NAME_EN in proto.text(r) and not TOKEN_RE.search(proto.text(r))
    assert not TOKEN_RE.search(r.raw), "pseudonym leaked in raw response/SSE"


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_unknown_token_not_invented(proto, sid):
    """a token the session never produced must pass through (not crash, not restore to someone else)."""
    mock_script({"text": "see <PERSON_77> and plain text"})
    r = proto.send(sid, proto.body([("user", "hello there")]), stream=True)
    assert r.status == 200, r.raw[:300]
    assert proto.text(r) == "see <PERSON_77> and plain text"
