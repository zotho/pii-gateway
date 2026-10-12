"""6. Persistence: mappings survive palena/redis restarts and do not expire."""
import pytest

from helpers import (ANTHROPIC, CHAT, NAME_EN, PROTOS, TOKEN_RE, mock_reset, new_sid, redis_client, redis_keys,
                     restart_palena, restart_redis, stack_up)

MONTH = 30 * 24 * 3600


def _tok(proto, sid, text):
    r = proto.send(sid, proto.body([("user", text)]))
    assert r.status == 200, r.raw[:300]
    toks = TOKEN_RE.findall(proto.msg_texts(proto.last_up())[-1])
    return toks


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_mapping_has_no_short_ttl(proto):
    before = redis_keys()
    sid = new_sid("ttl")
    _tok(proto, sid, f"Hello, I am {NAME_EN}.")
    new = redis_keys() - before
    assert new, "no redis keys created for session"
    r = redis_client()
    ttls = {k: r.ttl(k) for k in new}
    bad = {k: t for k, t in ttls.items() if t != -1 and t < MONTH}
    assert not bad, f"mapping keys expire too soon (ttl seconds): {bad}"


@pytest.mark.slow
@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_token_survives_palena_restart(proto):
    sid = new_sid("pal")
    t1 = _tok(proto, sid, f"Вчера Коля и Святослав обсуждали проект, а {NAME_EN} молчал.")
    restart_palena()
    mock_reset()
    t2 = _tok(proto, sid, f"Святослав, {NAME_EN} и Коля снова обсуждали проект.")
    assert len(t1) == 3
    assert t2 == [t1[1], t1[2], t1[0]], (t1, t2)


@pytest.mark.slow
def test_token_survives_redis_restart():
    proto = ANTHROPIC
    sid = new_sid("red")
    t1 = _tok(proto, sid, f"Вчера Коля и Святослав обсуждали проект, а {NAME_EN} молчал.")
    restart_redis()
    mock_reset()
    t2 = _tok(proto, sid, f"Святослав, {NAME_EN} и Коля снова обсуждали проект.")
    assert t2 == [t1[1], t1[2], t1[0]], (t1, t2)
    stack_up()
