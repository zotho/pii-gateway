"""Pure walker tests: collect/apply per protocol; protected fields untouched; JSON stays valid."""
import copy
import json

import pytest

import pii_walk as w


def up(plan):
    """Fake pseudonymizer: upper-case every collected string, return as new texts."""
    return [t.upper() for t in plan.texts]


def run(proto, body):
    before = copy.deepcopy(body)
    plan = w.collect_request(proto, body)
    texts = plan.texts
    plan.apply([t.upper() for t in texts])
    return before, texts


# ------------------------------------------------------------------ Anthropic
def anth_body():
    return {
        "model": "claude-x", "max_tokens": 9,
        "system": [{"type": "text", "text": "sys one", "cache_control": {"type": "ephemeral"}},
                   {"type": "text", "text": "sys two"}],
        "tools": [{"name": "Read", "description": "do not touch", "input_schema": {"type": "object"}}],
        "messages": [
            {"role": "user", "content": "plain user"},
            {"role": "assistant", "content": [
                {"type": "thinking", "thinking": "think about bob", "signature": "SIG=="},
                {"type": "redacted_thinking", "data": "opaque"},
                {"type": "text", "text": "assistant text"},
                {"type": "tool_use", "id": "toolu_1", "name": "Read",
                 "input": {"path": "/a/bob.txt", "n": 3, "opts": {"x": ["l1", 7, None]}, "flag": True}}]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "toolu_1", "content": "file says bob"},
                {"type": "tool_result", "tool_use_id": "toolu_2",
                 "content": [{"type": "text", "text": "blk"}, {"type": "image", "source": {"data": "AAA"}}]},
                {"type": "image", "source": {"type": "base64", "data": "AAAA"}},
                {"type": "text", "text": "tail", "cache_control": {"type": "ephemeral"}}]},
        ],
    }


def test_anthropic_collects_exact_fields_in_order():
    plan = w.collect_request(w.ANTHROPIC, anth_body())
    assert plan.texts == ["sys one", "sys two", "plain user", "assistant text", "/a/bob.txt", "l1",
                          "file says bob", "blk", "tail"]


def test_anthropic_apply_rewrites_and_protects():
    before, _ = run(w.ANTHROPIC, anth_body())
    b = anth_body()
    plan = w.collect_request(w.ANTHROPIC, b)
    plan.apply(up(plan))
    assert b["system"][0]["text"] == "SYS ONE" and b["system"][0]["cache_control"] == {"type": "ephemeral"}
    a = b["messages"][1]["content"]
    assert a[0] == before["messages"][1]["content"][0]            # thinking + signature untouched
    assert a[1] == {"type": "redacted_thinking", "data": "opaque"}
    assert a[2]["text"] == "ASSISTANT TEXT"
    assert a[3]["input"] == {"path": "/A/BOB.TXT", "n": 3, "opts": {"x": ["L1", 7, None]}, "flag": True}
    assert a[3]["id"] == "toolu_1" and a[3]["name"] == "Read"
    u = b["messages"][2]["content"]
    assert u[0]["content"] == "FILE SAYS BOB" and u[1]["content"][0]["text"] == "BLK"
    assert u[1]["content"][1] == {"type": "image", "source": {"data": "AAA"}}
    assert u[2] == before["messages"][2]["content"][2]
    assert u[3]["cache_control"] == {"type": "ephemeral"}
    assert b["tools"] == before["tools"]


def test_anthropic_system_string():
    b = {"system": "sys", "messages": []}
    plan = w.collect_request(w.ANTHROPIC, b)
    plan.apply(["S2"])
    assert b["system"] == "S2"


# ------------------------------------------------------------------ OpenAI chat
def test_chat_walk():
    args = json.dumps({"cmd": "cat bob.txt", "n": 1, "k": {"deep": ["x"]}})
    body = {"model": "m", "messages": [
        {"role": "system", "content": "sys"},
        {"role": "user", "content": [{"type": "text", "text": "hi bob"},
                                     {"type": "image_url", "image_url": {"url": "data:..."}}]},
        {"role": "assistant", "content": None, "reasoning_content": "secret thoughts bob",
         "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "sh", "arguments": args}}]},
        {"role": "tool", "tool_call_id": "call_1", "content": "out bob"}]}
    plan = w.collect_request(w.CHAT, body)
    assert plan.texts == ["sys", "hi bob", "cat bob.txt", "x", "out bob"]
    plan.apply(up(plan))
    m = body["messages"]
    assert m[1]["content"][0]["text"] == "HI BOB" and m[1]["content"][1]["type"] == "image_url"
    assert m[2]["reasoning_content"] == "secret thoughts bob"
    new_args = json.loads(m[2]["tool_calls"][0]["function"]["arguments"])
    assert new_args == {"cmd": "CAT BOB.TXT", "n": 1, "k": {"deep": ["X"]}}
    assert m[2]["tool_calls"][0]["id"] == "call_1"
    assert m[3]["content"] == "OUT BOB"


def test_unchanged_arguments_string_is_byte_identical():
    args = '{ "cmd":  "ls"  }'   # odd spacing must survive when nothing changes
    body = {"messages": [{"role": "assistant", "tool_calls": [
        {"id": "1", "type": "function", "function": {"name": "f", "arguments": args}}]}]}
    plan = w.collect_request(w.CHAT, body)
    plan.apply(plan.texts)
    assert body["messages"][0]["tool_calls"][0]["function"]["arguments"] == args


def test_arguments_with_quotes_and_unicode_stay_valid_json():
    args = json.dumps({"s": "a \"q\" \\ b"})
    body = {"messages": [{"role": "assistant", "tool_calls": [
        {"id": "1", "type": "function", "function": {"name": "f", "arguments": args}}]}]}
    plan = w.collect_request(w.CHAT, body)
    plan.apply(['<PERSON_1> said "hi" \\ Николай\n'])
    out = json.loads(body["messages"][0]["tool_calls"][0]["function"]["arguments"])
    assert out == {"s": '<PERSON_1> said "hi" \\ Николай\n'}


def test_non_json_arguments_treated_as_text():
    body = {"messages": [{"role": "assistant", "tool_calls": [
        {"id": "1", "type": "function", "function": {"name": "f", "arguments": "not json bob"}}]}]}
    plan = w.collect_request(w.CHAT, body)
    assert plan.texts == ["not json bob"]


# ------------------------------------------------------------------ Responses
def test_responses_walk():
    args = json.dumps({"cmd": "cat bob"})
    body = {"model": "m", "instructions": "be nice to bob", "input": [
        {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi bob"},
                                                        {"type": "input_image", "image_url": "data:x"}]},
        {"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "bob thoughts"}],
         "encrypted_content": "gAAAA"},
        {"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "sh", "arguments": args},
        {"type": "function_call_output", "call_id": "call_1", "output": "file: bob"},
        {"type": "custom_tool_call", "call_id": "c2", "name": "apply_patch", "input": "*** bob"},
        {"type": "custom_tool_call_output", "call_id": "c2", "output": "patched bob"},
        {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "done bob"}]},
        {"type": "compaction", "id": "cmp_1", "encrypted_content": "gAAAAcmp"},
        {"type": "item_reference", "id": "x"}]}
    before = copy.deepcopy(body)
    plan = w.collect_request(w.RESPONSES, body)
    assert plan.texts == ["be nice to bob", "hi bob", "cat bob", "file: bob", "*** bob", "patched bob", "done bob"]
    plan.apply(up(plan))
    assert body["input"][1] == before["input"][1]
    assert body["input"][7] == before["input"][7] and body["input"][8] == before["input"][8]
    assert json.loads(body["input"][2]["arguments"]) == {"cmd": "CAT BOB"}
    assert body["input"][2]["call_id"] == "call_1"
    assert body["input"][3]["output"] == "FILE: BOB"
    assert body["input"][0]["content"][1] == before["input"][0]["content"][1]


def test_responses_string_input_and_list_output():
    b = {"input": "hello bob"}
    p = w.collect_request(w.RESPONSES, b)
    p.apply(["X"])
    assert b["input"] == "X"
    b = {"input": [{"type": "function_call_output", "call_id": "1",
                    "output": [{"type": "input_text", "text": "t1"}]}]}
    p = w.collect_request(w.RESPONSES, b)
    assert p.texts == ["t1"]


# ------------------------------------------------------------------ responses (restore side)
def test_response_walkers():
    a = {"content": [{"type": "thinking", "thinking": "t", "signature": "s"},
                     {"type": "text", "text": "<PERSON_1> hi"},
                     {"type": "tool_use", "id": "1", "name": "f", "input": {"p": "<PERSON_1>"}}]}
    p = w.collect_response(w.ANTHROPIC, a)
    assert p.texts == ["<PERSON_1> hi", "<PERSON_1>"]
    p.apply(["Bob hi", "Bob"])
    assert a["content"][2]["input"] == {"p": "Bob"} and a["content"][0]["thinking"] == "t"

    class Obj:  # attribute-style (pydantic-like) objects
        def __init__(self, **kw):
            self.__dict__.update(kw)
    fn = Obj(name="f", arguments='{"p":"<PERSON_1>"}')
    chat = Obj(choices=[Obj(message=Obj(content="<PERSON_1>", tool_calls=[Obj(function=fn)]))])
    p = w.collect_response(w.CHAT, chat)
    p.apply(["Bob", "Bob"])
    assert chat.choices[0].message.content == "Bob" and json.loads(fn.arguments) == {"p": "Bob"}

    r = {"output": [{"type": "reasoning", "summary": [], "encrypted_content": "e"},
                    {"type": "message", "content": [{"type": "output_text", "text": "<PERSON_1>"}]},
                    {"type": "function_call", "arguments": '{"a":"<PERSON_1>"}'},
                    {"type": "compaction", "encrypted_content": "zzz"}]}
    p = w.collect_response(w.RESPONSES, r)
    assert p.texts == ["<PERSON_1>", "<PERSON_1>"]


def test_apply_length_mismatch_raises():
    p = w.collect_request(w.CHAT, {"messages": [{"role": "user", "content": "a"}]})
    with pytest.raises(ValueError):
        p.apply([])


def test_protocol_for_call_type():
    assert w.protocol_for_call_type("anthropic_messages") == w.ANTHROPIC
    assert w.protocol_for_call_type("acompletion") == w.CHAT
    assert w.protocol_for_call_type("aresponses") == w.RESPONSES
    assert w.protocol_for_call_type("acompact_responses") == w.RESPONSES
    assert w.protocol_for_call_type("aembedding") is None


# ------------------------------------------------------------------ pattern-only (system) slots
def po_texts(proto, body, spo=True):
    plan = w.collect_request(proto, body, spo)
    return [plan.texts[i] for i in plan.pattern_only], plan


def test_pattern_only_off_by_default():
    plan = w.collect_request(w.ANTHROPIC, {"system": "s", "messages": [{"role": "user", "content": "u"}]})
    assert plan.pattern_only == []


def test_pattern_only_anthropic_system_str_and_blocks_not_user_or_tool_result():
    body = {"system": "sys", "messages": [
        {"role": "user", "content": [{"type": "text", "text": "<system-reminder>CLAUDE.md</system-reminder>"},
                                      {"type": "tool_result", "tool_use_id": "t", "content": "tool out"}]}]}
    assert po_texts(w.ANTHROPIC, body)[0] == ["sys"]
    body["system"] = [{"type": "text", "text": "a"}, {"type": "text", "text": "b"}]
    got, plan = po_texts(w.ANTHROPIC, body)
    assert got == ["a", "b"]
    assert plan.texts[2:] == ["<system-reminder>CLAUDE.md</system-reminder>", "tool out"]


def test_pattern_only_chat_system_and_developer_only():
    body = {"messages": [{"role": "system", "content": "s"},
                         {"role": "developer", "content": [{"type": "text", "text": "d"}]},
                         {"role": "user", "content": "u"},
                         {"role": "tool", "tool_call_id": "c", "content": "t"}]}
    assert po_texts(w.CHAT, body)[0] == ["s", "d"]


def test_pattern_only_responses_instructions_and_system_messages():
    body = {"instructions": "ins", "input": [
        {"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "dev"}]},
        {"type": "message", "role": "system", "content": "sys"},
        {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "usr"}]},
        {"type": "function_call_output", "call_id": "c", "output": "out"}]}
    assert po_texts(w.RESPONSES, body)[0] == ["ins", "dev", "sys"]


def test_pattern_only_function_call_args_in_system_scope_flag_follows_scope():
    plan = w.collect_request(w.CHAT, {"messages": [
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "1", "type": "function", "function": {"name": "f", "arguments": "{\"a\":\"x\"}"}}]}]}, True)
    assert plan.pattern_only == []


def test_pattern_only_anthropic_role_system_message_but_not_user_or_assistant():
    body = {"messages": [{"role": "user", "content": "u"},
                         {"role": "system", "content": [{"type": "text", "text": "skills list"}]},
                         {"role": "system", "content": "sys str"},
                         {"role": "assistant", "content": [{"type": "text", "text": "a"}]}]}
    got, plan = po_texts(w.ANTHROPIC, body)
    assert got == ["skills list", "sys str"]
    assert plan.texts == ["u", "skills list", "sys str", "a"]
