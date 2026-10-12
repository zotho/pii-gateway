"""3. Token stability across turns; session isolation."""
import pytest

from helpers import (NAME_EN, NAME_RU, PROTOS, TOKEN_RE, mock_script, new_sid, redis_keys, upstream)

T1 = "Вчера Коля и Святослав обсуждали проект."
T2 = "Святослав сказал, что Коля опоздает."


def _toks(s):
    return TOKEN_RE.findall(s)


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_same_token_across_turns_and_order(proto, sid):
    r1 = proto.send(sid, proto.body([("user", T1)]))
    assert r1.status == 200, r1.raw[:300]
    t1 = _toks(proto.msg_texts(proto.last_up())[-1])
    assert len(t1) == 2 and t1[0] != t1[1], t1
    kolya, svyat = t1  # order of appearance in T1
    reply = proto.text(r1)
    assert "Коля" in reply and "Святослав" in reply
    # turn 2: swapped order + replayed history (client sends real values)
    r2 = proto.send(sid, proto.body([("user", T1), ("assistant", reply), ("user", T2)]))
    assert r2.status == 200, r2.raw[:300]
    texts = proto.msg_texts(proto.last_up())
    assert _toks(texts[0]) == [kolya, svyat], texts
    assert _toks(texts[1]) == [kolya, svyat], texts  # assistant echo replay
    assert _toks(texts[2]) == [svyat, kolya], texts  # new order, same mapping
    assert proto.text(r2).endswith("Святослав сказал, что Коля опоздает.")


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_sessions_isolated(proto):
    a, b = new_sid("A"), new_sid("B")
    before = redis_keys()
    assert proto.send(a, proto.body([("user", f"Hello, I am {NAME_EN}.")])).status == 200
    keys_a = redis_keys() - before
    assert keys_a, "session A created no redis state"
    mid = redis_keys()
    # session B: different person gets its own mapping
    assert proto.send(b, proto.body([("user", "Hello, I am Sviatoslav Ivanov.")])).status == 200
    keys_b = redis_keys() - mid
    assert keys_b and not (keys_a & keys_b), "sessions share redis keys"
    # B receives a model reply containing a token that only A produced: must not restore to A's value
    tokA = [t for t in _toks(str(upstream(proto.path)[0]["body"])) if t.startswith("<PERSON_")][0]
    mock_script({"text": f"hello {tokA} and <PERSON_2> and <PERSON_5>"})
    r = proto.send(b, proto.body([("user", "ping")]), stream=True)
    txt = proto.text(r)
    assert "Nikolai" not in txt and "Petrov" not in txt, f"cross-session restore: {txt}"
    # and symmetric: A's own mapping still intact
    mock_script({"text": f"hello {tokA}"})
    ra = proto.send(a, proto.body([("user", "ping")]))
    assert NAME_EN in proto.text(ra)
