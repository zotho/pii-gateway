"""7. Fail closed: with Palena down nothing may reach the upstream."""
import time

import pytest

from helpers import PROTOS, TEXT, assert_no_canary, restart_palena, stop_service, upstream, wait_ready


@pytest.fixture
def palena_down():
    stop_service("palena")
    time.sleep(1)
    yield
    restart_palena()
    wait_ready()


@pytest.mark.slow
@pytest.mark.parametrize("stream", [False, True], ids=["nonstream", "stream"])
@pytest.mark.parametrize("proto", PROTOS, ids=lambda p: p.name)
def test_palena_down_blocks_request(proto, stream, sid, palena_down):
    r = proto.send(sid, proto.body([("user", TEXT)]), stream=stream)
    assert r.status >= 400, f"request succeeded with palena down: {r.raw[:300]}"
    time.sleep(0.5)
    assert upstream() == [], "request reached upstream while palena was down"
