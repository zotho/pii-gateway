"""Streaming restore: holdback across split pseudonyms, structure preservation."""
import json
import random
from types import SimpleNamespace as NS

import pii_stream as s

REV = {"<PERSON_1>": "Nikolai Petrov", "<PERSON_10>": "Ann \"Q\" Lee", "<EMAIL_ADDRESS_1>": "n@example.com"}


def split_all(text, rnd):
    out, i = [], 0
    while i < len(text):
        n = rnd.randint(1, 4)
        out.append(text[i:i + n])
        i += n
    return out


def test_reverser_every_split_point():
    text = "Hi <PERSON_1>, <PERSON_10> and <EMAIL_ADDRESS_1> < not <PERSON_ x"
    want = s.Reverser(REV).full(text)
    assert "Nikolai Petrov" in want and "<PERSON_ x" in want
    for i in range(len(text) + 1):
        r = s.Reverser(REV)
        got = r.feed(text[:i]) + r.feed(text[i:]) + r.flush()
        assert got == want, i
    rnd = random.Random(1)
    for _ in range(200):
        r = s.Reverser(REV)
        got = "".join(r.feed(p) for p in split_all(text, rnd)) + r.flush()
        assert got == want


def test_reverser_prefix_token_not_confused():
    r = s.Reverser(REV)
    assert r.feed("<PERSON_1") == "" and r.feed("0>") == 'Ann "Q" Lee'
    r = s.Reverser(REV)
    assert r.feed("<PERSON_1") == "" and r.feed("> x") == "Nikolai Petrov x"


def test_reverser_json_mode_escapes_and_unicode_form():
    r = s.Reverser(REV, json_mode=True)
    out = r.feed('{"a":"<PERSON_10>","b":"\\u003cPERSON_1\\u003e"}')
    assert json.loads(out) == {"a": 'Ann "Q" Lee', "b": "Nikolai Petrov"}


def test_reverser_empty_map_passthrough():
    r = s.Reverser({})
    assert r.feed("<PERSON_1>") == "<PERSON_1>" and r.flush() == ""


# ------------------------------------------------------------------ Anthropic SSE
def sse(ev, name=True):
    return (f"event: {ev['type']}\n" if name else "") + "data: " + json.dumps(ev) + "\n\n"


def anth_events(text_parts, tool_parts=None):
    evs = [{"type": "message_start", "message": {"id": "m", "content": []}},
           {"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": ""}},
           {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "hm <PERSON_1>"}},
           {"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": "SIG=="}},
           {"type": "content_block_stop", "index": 0},
           {"type": "content_block_start", "index": 1, "content_block": {"type": "text", "text": ""}}]
    evs += [{"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": p}} for p in text_parts]
    evs.append({"type": "content_block_stop", "index": 1})
    if tool_parts:
        evs.append({"type": "content_block_start", "index": 2,
                    "content_block": {"type": "tool_use", "id": "t", "name": "f", "input": {}}})
        evs += [{"type": "content_block_delta", "index": 2, "delta": {"type": "input_json_delta", "partial_json": p}}
                for p in tool_parts]
        evs.append({"type": "content_block_stop", "index": 2})
    evs += [{"type": "message_delta", "delta": {"stop_reason": "end_turn"}}, {"type": "message_stop"}]
    return evs


def parse_sse(b):
    out = []
    for blk in b.decode().split("\n\n"):
        if blk.strip():
            data = [l[5:].strip() for l in blk.split("\n") if l.startswith("data:")]
            out.append(json.loads("\n".join(data)))
    return out


def test_anthropic_sse_restore_any_byte_chunking():
    text = "Hello <PERSON_1> / <EMAIL_ADDRESS_1> !"
    args = json.dumps({"who": "<PERSON_10>", "n": 1})
    rnd = random.Random(7)
    evs = anth_events(split_all(text, rnd), split_all(args, rnd))
    raw = "".join(sse(e) for e in evs).encode()
    for trial in range(30):
        r = s.AnthropicSseRestorer(REV)
        out, i = b"", 0
        while i < len(raw):
            n = rnd.randint(1, 120)
            out += r.feed(raw[i:i + n])
            i += n
        out += r.finish()
        got = parse_sse(out)
        assert [e["type"] for e in got if e["type"] != "content_block_delta"] == \
               [e["type"] for e in evs if e["type"] != "content_block_delta"]
        txt = "".join(e["delta"]["text"] for e in got if e.get("delta", {}).get("type") == "text_delta")
        assert txt == "Hello Nikolai Petrov / n@example.com !"
        pj = "".join(e["delta"]["partial_json"] for e in got if e.get("delta", {}).get("type") == "input_json_delta")
        assert json.loads(pj) == {"who": 'Ann "Q" Lee', "n": 1}
        # thinking + signature events byte-identical, still in place
        for e in evs[:5]:
            assert sse(e).encode() in out


def test_anthropic_untouched_events_passthrough_bytes():
    raw = sse({"type": "ping"}).encode() + sse({"type": "content_block_delta", "index": 0,
              "delta": {"type": "text_delta", "text": "plain"}}).encode()
    r = s.AnthropicSseRestorer(REV)
    assert r.feed(raw) == raw


def test_anthropic_tail_flushed_before_block_stop():
    r = s.AnthropicSseRestorer(REV)
    evs = [{"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "a <PERS"}},
           {"type": "content_block_stop", "index": 0}]
    out = parse_sse(r.feed("".join(sse(e) for e in evs).encode()))
    assert [e["type"] for e in out] == ["content_block_delta", "content_block_delta", "content_block_stop"]
    assert out[0]["delta"]["text"] == "a " and out[1]["delta"]["text"] == "<PERS"


# ------------------------------------------------------------------ chat
def chunk(content=None, args=None, finish=None, idx=0):
    tcs = [NS(index=0, function=NS(arguments=args))] if args is not None else None
    return NS(choices=[NS(index=idx, finish_reason=finish, delta=NS(content=content, tool_calls=tcs, role=None))])


def test_chat_restore_split_and_finish_flush():
    r = s.ChatRestorer(REV)
    chunks = [chunk("Hi <PER"), chunk("SON_1> and <EM"), chunk("AIL_ADDRESS_1>."), chunk("<PERS", finish="stop")]
    out = []
    for c in chunks:
        out += r.process(c)
    text = "".join(c.choices[0].delta.content or "" for c in out)
    assert text == "Hi Nikolai Petrov and n@example.com.<PERS"
    assert out[-1].choices[0].finish_reason == "stop" and len(out) == 4 and r.finish() == []


def test_chat_tool_args():
    r = s.ChatRestorer(REV)
    parts = ['{"who":"<PER', 'SON_1>"}']
    out = [r.process(chunk(args=p))[0] for p in parts]
    assert json.loads("".join(c.choices[0].delta.tool_calls[0].function.arguments for c in out)) == {"who": "Nikolai Petrov"}


# ------------------------------------------------------------------ responses
def ev(t, **kw):
    return NS(type=t, **kw)


def test_responses_restore():
    r = s.ResponsesRestorer(REV)
    out = []
    for e in [ev("response.output_text.delta", item_id="i", content_index=0, delta="Hi <PERSON"),
              ev("response.output_text.delta", item_id="i", content_index=0, delta="_1>! <PE"),
              ev("response.output_text.done", item_id="i", content_index=0, text="Hi <PERSON_1>! <PE"),
              ev("response.reasoning_summary_text.delta", delta="<PERSON_1> thinks"),
              ev("response.function_call_arguments.delta", item_id="f", delta='{"w":"<PERS'),
              ev("response.function_call_arguments.delta", item_id="f", delta='ON_1>"}'),
              ev("response.function_call_arguments.done", item_id="f", arguments='{"w":"<PERSON_1>"}'),
              ev("response.output_item.done", item=NS(type="message", content=[NS(type="output_text", text="<PERSON_1>")])),
              ev("response.completed", response=NS(output=[NS(type="function_call", arguments='{"w":"<PERSON_1>"}'),
                                                           NS(type="reasoning", summary=[])]))]:
        out += r.process(e)
    deltas = "".join(e.delta for e in out if e.type == "response.output_text.delta")
    assert deltas == "Hi Nikolai Petrov! <PE"          # tail released before .done
    done = [e for e in out if e.type == "response.output_text.done"][0]
    assert done.text == "Hi Nikolai Petrov! <PE"
    assert [e for e in out if e.type == "response.reasoning_summary_text.delta"][0].delta == "<PERSON_1> thinks"
    args = "".join(e.delta for e in out if e.type == "response.function_call_arguments.delta")
    assert json.loads(args) == {"w": "Nikolai Petrov"}
    assert json.loads([e for e in out if e.type == "response.function_call_arguments.done"][0].arguments) == {"w": "Nikolai Petrov"}
    assert out[-2].item.content[0].text == "Nikolai Petrov"
    assert json.loads(out[-1].response.output[0].arguments) == {"w": "Nikolai Petrov"}
