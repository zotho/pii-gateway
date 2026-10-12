"""11. Audit log + metrics (docs/OBSERVABILITY.md): events carry handles / counts / fingerprints, never values."""
import subprocess
import sys
import time

import httpx
import pytest

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parents[2] / "litellm"))
from pii_audit import AuditLog  # noqa: E402

from helpers import (ANTHROPIC, CHAT, KEY, RESPONSES, EMAIL, NAME_EN, NAME_EN2, PII_CANARIES, ROOT, TEXT, new_sid, restart_palena,
                     sh, stop_service, upstream, wait_ready)

AUDIT = AuditLog()


def events(sid, kind=None, want=1, timeout=8):
    """events of a session (polls: the stream-end event is written just after the last byte)."""
    end = time.time() + timeout
    while True:
        evs = [e for e in AUDIT.read(session=sid) if kind is None or e["event"] == kind]
        if len(evs) >= want or time.time() > end:
            return evs
        time.sleep(0.2)


def test_masked_event_has_handles_not_values(sid):
    r = ANTHROPIC.send(sid, ANTHROPIC.body([("user", TEXT)]))
    assert r.status == 200, r.raw
    (ev,) = events(sid, "request.masked")
    assert ev["route"] == "anthropic_messages" and ev["model"] == "mock-claude" and ev["texts"] >= 1
    assert ev["call_id"] and ev["principal"] and ev["palena_ms"] >= 0 and ev["leak_suspects"] == []
    assert ev["entities"].get("PERSON", 0) >= 1 and ev["entities"].get("EMAIL_ADDRESS", 0) >= 1, ev["entities"]
    assert {n["type"] for n in ev["new"]} >= {"PERSON", "EMAIL_ADDRESS"}
    assert all(set(n) == {"handle", "type", "fp"} and n["handle"].startswith("<") for n in ev["new"])
    raw = " ".join(l for l in open(AUDIT.path, encoding="utf-8") if sid in l)
    leaks = [c for c in PII_CANARIES if c.lower() in raw.lower()]
    assert not leaks, f"real values in audit log: {leaks}"


def test_new_mappings_logged_once_and_fp_lookup_finds_session(sid):
    for _ in range(2):
        assert ANTHROPIC.send(sid, ANTHROPIC.body([("user", TEXT)])).status == 200
    masked = events(sid, "request.masked", want=2)
    assert len(masked) == 2 and masked[0]["new"] and masked[1]["new"] == []
    handles = [n["handle"] for e in masked for n in e["new"]]
    assert len(handles) == len(set(handles))
    fp = AUDIT.fp("EMAIL_ADDRESS", EMAIL)
    assert fp in {n["fp"] for n in masked[0]["new"]}
    p = subprocess.run([sys.executable, str(ROOT / "scripts/audit_verify.py"), "--fp", "EMAIL_ADDRESS", EMAIL],
                       capture_output=True, text=True, cwd=ROOT)
    assert p.returncode == 0 and f"session={sid}" in p.stdout, p.stdout + p.stderr
    assert EMAIL not in p.stdout.replace(f"fp(EMAIL_ADDRESS)", "")  # value is only echoed via the CLI arg, not the log


def test_chain_verifies():
    ok, n, msg = AUDIT.verify()
    assert ok, msg
    assert n > 0


def test_stream_writes_response_restored_with_counts(sid):
    r = ANTHROPIC.send(sid, ANTHROPIC.body([("user", TEXT)]), stream=True)
    assert r.status == 200
    (ev,) = events(sid, "response.restored")
    assert ev["stream"] is True and ev["upstream_status"] == 200
    assert ev["restored"].get("PERSON", 0) >= 1 and ev["restored"].get("EMAIL_ADDRESS", 0) >= 1, ev
    assert ev["unknown_handles"] == 0 and ev["upstream_ms"] is not None


def test_nonstream_writes_response_restored(sid):
    assert CHAT.send(sid, CHAT.body([("user", TEXT)])).status == 200
    (ev,) = events(sid, "response.restored")
    assert ev["stream"] is False and ev["restored"].get("PERSON", 0) >= 1 and ev["upstream_status"] == 200


def leaky_body(extra_desc):
    return {"max_tokens": 64, "messages": [{"role": "user", "content": TEXT}],
            "tools": [{"name": "lookup", "description": extra_desc, "input_schema": {"type": "object"}}]}


def test_leak_suspect_detected_in_field_walker_skips(sid):
    # first request establishes the mapping; second smuggles the real name via tools[].description
    assert ANTHROPIC.send(sid, ANTHROPIC.body([("user", TEXT)])).status == 200
    r = ANTHROPIC.send(sid, leaky_body(f"look up {NAME_EN.upper()} in the directory"))
    assert r.status == 200, r.raw  # default action = log
    sus = events(sid, "request.masked", want=2)[-1]["leak_suspects"]
    assert sus and sus[0]["path"] == "tools[0].description" and sus[0]["handle"].startswith("<PERSON_"), sus
    assert NAME_EN.lower() not in str(sus).lower()
    m = httpx.get("http://127.0.0.1:14090/metrics", timeout=5).text
    assert "pii_leak_suspects_total" in m and "pii_requests_total" in m and "pii_palena_seconds_bucket" in m


@pytest.mark.slow
def test_leak_action_block(sid):
    def restart(env):
        p = sh("bash scripts/restart-litellm.sh", env)
        assert p.returncode == 0, p.stdout + p.stderr
        wait_ready()
    try:
        restart({"PII_BRIDGE_LEAK_ACTION": "block"})
        assert ANTHROPIC.send(sid, ANTHROPIC.body([("user", TEXT)])).status == 200
        from helpers import mock_reset
        mock_reset()
        r = ANTHROPIC.send(sid, leaky_body(f"look up {NAME_EN2}"))
        assert r.status == 400 and "leak" in r.raw.lower(), r.raw
        assert upstream("/v1/messages") == []
        blocked = events(sid, "request.blocked")
        assert blocked and blocked[-1]["reason"] == "leak" and blocked[-1]["leak_suspects"]
    finally:
        restart({"PII_BRIDGE_LEAK_ACTION": "log"})


@pytest.mark.slow
def test_palena_down_writes_request_blocked(sid):
    stop_service("palena")
    try:
        time.sleep(1)
        r = ANTHROPIC.send(sid, ANTHROPIC.body([("user", TEXT)]))
        assert r.status >= 400
        (ev,) = events(sid, "request.blocked")
        assert ev["reason"] == "palena_unreachable" and ev["route"] == "anthropic_messages"
        assert upstream() == []
    finally:
        restart_palena()
        wait_ready()


def test_unsupported_route_writes_request_blocked(sid):
    r = httpx.post("http://127.0.0.1:14000/v1/embeddings", timeout=30,
                   headers={"authorization": f"Bearer {KEY}", "x-session-id": sid},
                   json={"model": "mock-gpt", "input": TEXT})
    assert r.status_code >= 400
    ev = events(sid, "request.blocked")
    assert ev and ev[0]["reason"] == "unsupported_route"


@pytest.mark.parametrize("proto,stream,prefix", [
    (ANTHROPIC, False, "req_mock_"), (ANTHROPIC, True, "req_mock_"),
    (RESPONSES, False, "mock-"), (RESPONSES, True, "mock-"),
    (CHAT, False, "mock-"), (CHAT, True, "mock-"),
])
def test_response_restored_has_provider_request_id(sid, proto, stream, prefix):
    """the mock upstream answers with `request-id` (Anthropic) / `x-request-id` (OpenAI); the bridge logs it."""
    assert proto.send(sid, proto.body([("user", TEXT)]), stream=stream).status == 200
    (ev,) = events(sid, "response.restored")
    assert ev["stream"] is stream
    assert (ev["provider_request_id"] or "").startswith(prefix), ev
