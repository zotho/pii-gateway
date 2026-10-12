"""1. Request masking in every field, for every protocol."""
import pytest

from helpers import (AWS_KEY, CHAT, PROTOS, RICH, TOKEN_RE, ANTHROPIC, assert_no_canary, upstream)

SYSTEM_CASES = {"system_str", "system_blocks", "system", "instructions"}
CASES = [(p, c) for p in PROTOS for c in p.cases()]


@pytest.mark.parametrize("proto,case", CASES, ids=[f"{p.name}-{c.id}" for p, c in CASES])
def test_field_masked(proto, case, sid):
    r = proto.send(sid, case.body)
    assert r.status == 200, r.raw[:500]
    reqs = proto.up()
    assert len(reqs) == 1
    # system prompts are pattern-only (docs/DESIGN.md): first-sighting NER names stay, email/phone are masked
    assert_no_canary(upstream(), pattern_only=case.id in SYSTEM_CASES)
    up = reqs[0]["body"]
    located = case.locate(up)
    assert TOKEN_RE.search(located), f"no pseudonym token in {case.id}: {located[:500]}"
    if case.args_check:
        case.args_check(up)  # JSON args still parse, non-string leaves untouched, string leaves tokenised


@pytest.mark.parametrize("proto", [ANTHROPIC, CHAT], ids=lambda p: p.name)
def test_aws_key_masked(proto, sid):
    """Secret-looking key. Needs a secrets entity configured in Palena (default entity list is
    PERSON/ORGANIZATION/EMAIL/PHONE only), so this tracks that gap."""
    r = proto.send(sid, proto.body([("user", f"my key is {AWS_KEY} please keep it safe")]))
    assert r.status == 200, r.raw[:300]
    assert_no_canary(upstream(), secrets=True)


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_short_first_name_masked(proto, sid):
    r = proto.send(sid, proto.body([("user", "Вчера Коля и Святослав обсуждали проект.")]))
    assert r.status == 200, r.raw[:300]
    blob = str(upstream())
    assert "Коля" not in blob and "Святослав" not in blob
    assert len(set(TOKEN_RE.findall(proto.msg_texts(proto.last_up())[-1]))) == 2


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_unicode_escaped_body_still_caught(proto, sid):
    """The helper must catch \\u-escaped Cyrillic: sanity-check the helper itself."""
    import json
    fake = [{"path": "/x", "query": "", "body": json.loads('{"a":"\\u041d\\u0438\\u043a\\u043e\\u043b\\u0430\\u0439 \\u041f\\u0435\\u0442\\u0440\\u043e\\u0432"}')}]
    with pytest.raises(AssertionError):
        assert_no_canary(fake)


@pytest.mark.parametrize("lang", ["ru", "mixed"])
@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_ru_and_mixed_language_masked(proto, lang, sid):
    """Russian names must be caught alone and inside a mixed RU+EN message."""
    from helpers import TEXT_RU
    r = proto.send(sid, proto.body([("user", TEXT_RU if lang == "ru" else RICH)]))
    assert r.status == 200, r.raw[:300]
    assert_no_canary(upstream())
