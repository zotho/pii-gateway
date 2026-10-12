"""5. Concurrent first-time requests in one session agree on the token."""
from concurrent.futures import ThreadPoolExecutor

import pytest

from helpers import NAME_EN, PROTOS, TOKEN_RE, dumps, upstream


@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_parallel_first_mentions_same_token(proto, sid):
    N = 8

    def one(i):
        return proto.send(sid, proto.body([("user", f"Request {i}: please ask {NAME_EN} about item {i}.")]))
    with ThreadPoolExecutor(N) as ex:
        resps = list(ex.map(one, range(N)))
    assert [r.status for r in resps] == [200] * N, [r.raw[:200] for r in resps if r.status != 200]
    reqs = proto.up()
    assert len(reqs) == N
    seen = set()
    for rec in reqs:
        last = proto.msg_texts(rec["body"])[-1]
        toks = TOKEN_RE.findall(last)
        assert len(toks) == 1, last
        seen.add(toks[0])
    assert len(seen) == 1, f"divergent tokens for the same name: {seen}"
    for r in resps:
        assert NAME_EN in proto.text(r)
