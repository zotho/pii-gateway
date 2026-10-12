"""Leak scan + audit event building (no live stack: fake Palena via MockTransport, fake Redis mapping)."""
import asyncio
import json
import time

import httpx
import pytest
from fastapi import HTTPException

import pii_audit
import pii_bridge as pb
import pii_leak
import pii_stream

MAP = {"Nikolai Petrov": "<PERSON_1>", "nikolai@example.com": "<EMAIL_ADDRESS_1>", "AB": "<PERSON_9>",
       "hunter2-secret": "<SECRET_1>"}


# ---------------------------------------------------------------- leak scan
def test_leak_finds_value_with_path_and_ignores_handles_and_short_values():
    body = {"model": "m", "messages": [{"role": "user", "content": "hi <PERSON_1>, AB"}],
            "tools": [{"name": "t", "description": "send to NIKOLAI@example.com please"}]}
    got = pii_leak.find_leaks(body, MAP)
    assert got == [{"handle": "<EMAIL_ADDRESS_1>", "path": "tools[0].description"}]


def test_leak_value_in_key_and_json_escaped_value():
    got = pii_leak.find_leaks({"a": {"Nikolai Petrov": 1}}, MAP)
    assert got == [{"handle": "<PERSON_1>", "path": "a.Nikolai Petrov#key"}]
    got = pii_leak.find_leaks({"x": ['say "hi"\nthere']}, {'say "hi"\nthere': "<PERSON_2>"})
    assert got and got[0]["handle"] == "<PERSON_2>" and got[0]["path"] == "x[0]"


def test_leak_handle_name_does_not_match_value_named_like_type():
    assert pii_leak.find_leaks({"c": "<PERSON_1> <PERSON_2>"}, {"PERSON": "<PERSON_7>"}) == []


def test_leak_clean_and_never_returns_values():
    body = {"messages": [{"role": "user", "content": "nothing here"}]}
    assert pii_leak.find_leaks(body, MAP) == []
    leaky = pii_leak.find_leaks({"d": "Nikolai Petrov / hunter2-secret"}, MAP)
    assert "Nikolai" not in json.dumps(leaky) and "hunter2" not in json.dumps(leaky)


def test_outgoing_body_excludes_litellm_internals():
    data = {"model": "m", "messages": [], "proxy_server_request": {"body": "Nikolai Petrov"},
            "metadata": {"x": "Nikolai Petrov"}, "litellm_call_id": "c", "user_api_key_hash": "h"}
    assert set(pii_leak.outgoing_body(data)) == {"model", "messages"}


def test_leak_scan_perf_200kb():
    mapping = {f"Person Number{i} Surname{i}": f"<PERSON_{i}>" for i in range(300)}
    text = ("lorem ipsum dolor sit amet <PERSON_1> consectetur " * 4000)[:200_000]
    body = {"messages": [{"role": "user", "content": text}]}
    t = time.perf_counter()
    assert pii_leak.find_leaks(body, mapping) == []
    assert time.perf_counter() - t < 0.5


# ---------------------------------------------------------------- event building
class FakeAudit:
    def __init__(self, fail=False):
        self.events, self.fail = [], fail

    def append(self, event, **f):
        if self.fail:
            raise OSError("disk full")
        self.events.append({"event": event, **f})

    fp = staticmethod(lambda t, v: pii_audit.AuditLog(path="/tmp/x-unused.jsonl", key=b"k").fp(t, v))


def bridge(maps, texts_out=lambda t: t, status=200):
    """maps: list of mapping snapshots returned by successive _mapping() calls."""
    def h(req):
        j = json.loads(req.content)
        return httpx.Response(status, json={"action": "GUARDRAIL_INTERVENED", "texts": texts_out(j["texts"])})
    b = pb.PiiBridge()
    b._http = httpx.AsyncClient(transport=httpx.MockTransport(h))
    b.cache_on = False
    seq = list(maps)

    async def fake_mapping(session):
        return seq.pop(0) if len(seq) > 1 else seq[0]
    b._mapping = fake_mapping
    return b


def run_pre(b, data):
    return asyncio.run(b.async_pre_call_hook(None, None, data, "acompletion"))


@pytest.fixture
def audit(monkeypatch):
    a = FakeAudit()
    monkeypatch.setattr(pb, "AUDIT", a)
    return a


def test_masked_event_has_handles_counts_new_fp_and_no_values(audit):
    after = {"Nikolai Petrov": "<PERSON_1>", "n@e.com": "<EMAIL_ADDRESS_1>"}
    b = bridge([{"n@e.com": "<EMAIL_ADDRESS_1>"}, after], lambda ts: ["hi <PERSON_1> <PERSON_1> <EMAIL_ADDRESS_1>"] * len(ts))
    data = {"litellm_trace_id": "S", "litellm_call_id": "C1", "model": "mock",
            "messages": [{"role": "user", "content": "Nikolai Petrov n@e.com"}]}
    run_pre(b, data)
    (ev,) = audit.events
    assert ev["event"] == "request.masked" and ev["session"] == "S" and ev["call_id"] == "C1"
    assert ev["route"] == "acompletion" and ev["model"] == "mock" and ev["texts"] == 1 and ev["pattern_only"] == 0
    assert ev["entities"] == {"PERSON": 2, "EMAIL_ADDRESS": 1}
    assert ev["new"] == [{"handle": "<PERSON_1>", "type": "PERSON", "fp": audit.fp("PERSON", "Nikolai Petrov")}]
    assert ev["leak_suspects"] == [] and ev["principal"] == "master"
    assert "Nikolai" not in json.dumps(ev)


def test_leak_suspect_logged_and_block_mode(audit):
    m = {"secretvalue": "<SECRET_1>"}
    data = lambda: {"litellm_trace_id": "S", "model": "m", "tools": [{"description": "has SecretValue inside"}],  # noqa
                    "messages": [{"role": "user", "content": "x"}]}
    b = bridge([m, m], lambda ts: ts)
    run_pre(b, data())
    assert audit.events[0]["leak_suspects"] == [{"handle": "<SECRET_1>", "path": "tools[0].description"}]
    audit.events.clear()
    b.leak_action = "block"
    with pytest.raises(HTTPException) as e:
        run_pre(b, data())
    assert e.value.status_code == 400
    assert [x["event"] for x in audit.events] == ["request.masked", "request.blocked"]
    assert audit.events[1]["reason"] == "leak"


@pytest.mark.parametrize("status,reason", [(500, "palena_unreachable")])
def test_palena_failure_writes_blocked(audit, status, reason):
    b = bridge([{}], status=status)
    with pytest.raises(HTTPException):
        run_pre(b, {"litellm_trace_id": "S", "messages": [{"role": "user", "content": "a"}]})
    assert audit.events[-1]["event"] == "request.blocked" and audit.events[-1]["reason"] == reason


def test_unsupported_route_writes_blocked(audit):
    b = bridge([{}])
    with pytest.raises(HTTPException):
        asyncio.run(b.async_pre_call_hook(None, None, {"litellm_trace_id": "S", "input": "x"}, "aembedding"))
    assert audit.events[-1]["reason"] == "unsupported_route"


def test_audit_failure_does_not_break_traffic_unless_required(monkeypatch):
    monkeypatch.setattr(pb, "AUDIT", FakeAudit(fail=True))
    b = bridge([{}, {}], lambda ts: ts)
    data = {"litellm_trace_id": "S", "messages": [{"role": "user", "content": "a"}]}
    assert run_pre(b, data) is data  # error logged, traffic flows
    b.audit_required = True
    with pytest.raises(HTTPException) as e:
        run_pre(b, {"litellm_trace_id": "S", "messages": [{"role": "user", "content": "a"}]})
    assert e.value.status_code == 503


def test_stream_restore_stats_count_restored_kept_and_unknown():
    stats = pii_stream.RestoreStats()
    rev = pii_stream.StatDict({"<PERSON_1>": "Nikolai"}, stats)
    r = pii_stream.Reverser(rev)
    out = r.feed("hi <PERS") + r.feed("ON_1> and <PERSON_1>, <SECRET_1> <PERSON_5>") + r.flush()
    assert out == "hi Nikolai and Nikolai, <SECRET_1> <PERSON_5>"
    assert stats.restored == {"<PERSON_1>": 2} and stats.leftover == {"<SECRET_1>": 1, "<PERSON_5>": 1}


# ---------------------------------------------------------------- word-boundary matching
BOUND = {"Ivan": "<PERSON_1>", "contact": "<ORGANIZATION_1>", "a@b.com": "<EMAIL_ADDRESS_1>",
         "+7 999 123-45-67": "<PHONE_NUMBER_1>", "Иван": "<PERSON_2>"}


@pytest.mark.parametrize("text", ["Ivanov", "ivanova", "contacts", "xa@b.com", "a@b.comx", "a@b.com.au",
                                  "x+7 999 123-45-67", "Иванов", "recontact"])
def test_leak_boundary_no_substring_hits(text):
    assert pii_leak.find_leaks({"c": text}, BOUND) == []


@pytest.mark.parametrize("text,handle", [
    ("hi IVAN, ok", "<PERSON_1>"), ("Who is the Contact?", "<ORGANIZATION_1>"), ("mail A@B.com.", "<EMAIL_ADDRESS_1>"),
    ("call +7 999 123-45-67!", "<PHONE_NUMBER_1>"), ("(иван)", "<PERSON_2>"), ("Ivan_ and Ivan", "<PERSON_1>"),
])
def test_leak_boundary_whole_token_hits(text, handle):
    assert [h["handle"] for h in pii_leak.find_leaks({"c": text}, BOUND)] == [handle]


def test_leak_boundary_perf_many_prefilter_hits():
    words = [f"w{i:04d}x" for i in range(5000)]
    mapping = {w: f"<P_{i}>" for i, w in enumerate(words[:300])}
    body = {"messages": [{"role": "user", "content": (" ".join(words) + " ")[:200_000]}]}
    t = time.perf_counter()
    assert len(pii_leak.find_leaks(body, mapping)) == 300
    assert time.perf_counter() - t < 0.5
