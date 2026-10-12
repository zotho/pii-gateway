"""10. System prompts are "pattern-only": NER-derived entities (product names -> PERSON/ORGANIZATION
false positives) are NOT masked there, while pattern recognizers (email, phone, SECRET) and values
already known to the session still are.  User text keeps full NER."""
import pytest

from helpers import (ANTHROPIC, AWS_KEY, CHAT, EMAIL, NAME_EN, NAME_EN2, PHONE, PROTOS, RESPONSES,
                     assert_no_canary, dumps, new_sid, tokens)

PRODUCTS = ["Upstash Redis", "Recharts", "Google Docs", "OAuth", "Vercel Functions"]
PRODUCT_SENTENCE = ("You can use Upstash Redis, Recharts, Google Docs and OAuth together with "
                    "Vercel Functions when building the dashboard.")


def system_blob(proto, up) -> str:
    if proto is ANTHROPIC:
        return dumps(up.get("system"))
    if proto is CHAT:
        return dumps([m for m in up["messages"] if m["role"] == "system"])
    return dumps(up.get("instructions"))


def user_blob(proto, up) -> str:
    return dumps(proto.msg_texts(up))


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_product_names_in_system_not_masked(proto, sid):
    r = proto.send(sid, proto.body([("user", "hello")], system=PRODUCT_SENTENCE))
    assert r.status == 200, r.raw[:300]
    up = proto.last_up()
    sysb = system_blob(proto, up)
    for p in PRODUCTS:
        assert p in sysb, f"{p!r} was masked in system: {sysb}"
    assert not tokens(sysb), sysb


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_email_phone_secret_in_system_masked(proto, sid):
    system = f"{PRODUCT_SENTENCE} Contact {EMAIL} or {PHONE}. Deploy key: {AWS_KEY}"
    r = proto.send(sid, proto.body([("user", "hello")], system=system))
    assert r.status == 200, r.raw[:300]
    sysb = system_blob(proto, proto.last_up())
    for leak in (EMAIL, PHONE, AWS_KEY, "916 123-45-67"):
        assert leak not in sysb, f"{leak!r} leaked in system: {sysb}"
    toks = tokens(sysb)
    assert any(t.startswith("<EMAIL_ADDRESS_") for t in toks), sysb
    assert any(t.startswith("<SECRET_") for t in toks), sysb
    assert "Recharts" in sysb and "Upstash Redis" in sysb  # still pattern-only


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_name_known_to_session_masked_in_system(proto, sid):
    # first sighting in USER text (full NER), then the same name reappears in the system prompt
    r1 = proto.send(sid, proto.body([("user", f"Hello, I am {NAME_EN}, please remember that.")]))
    assert r1.status == 200, r1.raw[:300]
    r2 = proto.send(sid, proto.body([("user", "next")], system=f"Owner of this repo: {NAME_EN}. {PRODUCT_SENTENCE}"))
    assert r2.status == 200, r2.raw[:300]
    sysb = system_blob(proto, proto.last_up())
    assert "Nikolai" not in sysb and "Petrov" not in sysb, sysb
    assert any(t.startswith("<PERSON_") for t in tokens(sysb)), sysb
    assert "Recharts" in sysb


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_unknown_name_in_system_is_not_ner_masked(proto, sid):
    """Documented trade-off: a never-seen name that only the system prompt carries is not NER-detected."""
    r = proto.send(sid, proto.body([("user", "hello")], system=f"Maintainer: {NAME_EN2}."))
    assert r.status == 200, r.raw[:300]
    assert "Sviatoslav Ivanov" in system_blob(proto, proto.last_up())


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_user_text_keeps_full_ner(proto, sid):
    text = f"{PRODUCT_SENTENCE} I am {NAME_EN}, my manager is {NAME_EN2}, mail {EMAIL}."
    r = proto.send(sid, proto.body([("user", text)], system=PRODUCT_SENTENCE))
    assert r.status == 200, r.raw[:300]
    up = proto.last_up()
    ub = user_blob(proto, up)
    for stem in (NAME_EN, NAME_EN2, "Nikolai", "Petrov", "Sviatoslav", "Ivanov", EMAIL):
        assert stem not in ub, f"{stem!r} leaked in user text: {ub}"
    assert any(t.startswith("<PERSON_") for t in tokens(ub)), ub
    assert_no_canary([{"path": proto.path, "body": {"u": ub}}])


def test_claude_md_in_user_system_reminder_keeps_ner(sid):
    """Claude Code puts CLAUDE.md into a USER message <system-reminder>: it must NOT be pattern-only."""
    body = {"max_tokens": 64, "system": PRODUCT_SENTENCE, "messages": [{"role": "user", "content": [
        {"type": "text", "text": f"<system-reminder>CLAUDE.md: the maintainer is {NAME_EN2}.</system-reminder>"},
        {"type": "text", "text": "hi"}]}]}
    r = ANTHROPIC.send(sid, body)
    assert r.status == 200, r.raw[:300]
    up = ANTHROPIC.last_up()
    assert "Sviatoslav" not in dumps(up["messages"]) and "Ivanov" not in dumps(up["messages"])
    assert "Recharts" in dumps(up["system"])
