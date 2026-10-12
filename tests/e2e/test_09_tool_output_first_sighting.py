"""9. A person name seen for the FIRST time inside tool output (JSON-escaped, git log, code comment,
CSV, grep -n, Python repr ...) must be masked in every protocol's tool-result field."""
import json

import pytest

from helpers import ANTHROPIC, CHAT, RESPONSES, dumps, find, http_post, new_sid, upstream

NAMES = {
    "ru": ("Святослав Иванов", ["Святослав", "Иванов"]),
    "en": ("Sviatoslav Ivanov", ["Sviatoslav", "Ivanov"]),
    "en2": ("Nikolai Petrov", ["Nikolai", "Petrov"]),
}

SHAPES = {
    "json_escaped": lambda n: json.dumps({"chunk_id": "0e4afc", "exit_code": 0,
                                          "output": f"1:# Maintainer: {n}\n"}, ensure_ascii=False),
    "git_log": lambda n: f"commit 1a2b3c\nAuthor: {n} <nikolai.petrov@example.com>\nDate:   Mon Oct 12\n\n    fix\n",
    "todo_comment": lambda n: f"src/app.py:12:    // TODO({n}): handle errors\n",
    "py_repr": lambda n: "{'author': '" + n + "', 'note': \"he said \\\"hi\\\"\\n\"}",
    "csv": lambda n: f"id,name,role\n1,{n},maintainer\n2,Jane,dev\n",
    "grep_n": lambda n: f"1:# Maintainer: {n}\n7:def main():\n",
    "maintainer_en": lambda n: f"Maintainer: {n}",
}

PROTO_FIELDS = ["anthropic_str", "anthropic_blocks", "chat_str", "chat_parts",
                "resp_fco_str", "resp_fco_list", "resp_ctco_str", "resp_ctco_list"]


def _build(field: str, text: str):
    if field.startswith("anthropic"):
        content = text if field == "anthropic_str" else [{"type": "text", "text": text}]
        return ANTHROPIC, {"max_tokens": 64, "messages": [
            {"role": "user", "content": "grep maintainer"},
            {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_01", "name": "bash",
                                               "input": {"command": "grep -n Maintainer src/app.py"}}]},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_01", "content": content}]}]}
    if field.startswith("chat"):
        content = text if field == "chat_str" else [{"type": "text", "text": text}]
        return CHAT, {"max_tokens": 64, "messages": [
            {"role": "user", "content": "grep maintainer"},
            {"role": "assistant", "content": None, "tool_calls": [
                {"id": "call_1", "type": "function",
                 "function": {"name": "bash", "arguments": json.dumps({"cmd": "grep -n Maintainer src/app.py"})}}]},
            {"role": "tool", "tool_call_id": "call_1", "content": content}]}
    kind = "function_call_output" if field.startswith("resp_fco") else "custom_tool_call_output"
    call = ({"type": "function_call", "call_id": "c1", "name": "exec",
             "arguments": json.dumps({"cmd": "grep -n Maintainer src/app.py"})}
            if kind == "function_call_output" else
            {"type": "custom_tool_call", "call_id": "c1", "name": "exec", "input": "grep -n Maintainer src/app.py"})
    if field.endswith("_list"):
        out = [{"type": "input_text", "text": "Script completed\nOutput:\n"},
               {"type": "input_text", "text": "{}"},
               {"type": "input_text", "text": text}]
    else:
        out = text
    return RESPONSES, {"store": False, "input": [RESPONSES.msg("user", "grep maintainer"), call,
                                                 {"type": "custom_tool_call_output" if kind.startswith("custom") else kind,
                                                  "call_id": "c1", "output": out}]}


@pytest.mark.parametrize("shape", list(SHAPES))
@pytest.mark.parametrize("lang", list(NAMES))
@pytest.mark.parametrize("field", PROTO_FIELDS)
def test_first_sighting_in_tool_output_is_masked(field, lang, shape):
    name, stems = NAMES[lang]
    proto, body = _build(field, SHAPES[shape](name))
    sid = new_sid("fs")
    r = proto.send(sid, body)
    assert r.status == 200, r.raw[:500]
    up = proto.last_up()
    blob = dumps(up)
    for stem in stems + [name]:
        assert stem.lower() not in blob.lower(), f"{stem!r} leaked ({field}/{lang}/{shape}): {blob[:1200]}"


# Known names must stay masked when a tool output echoes a file JSON-escaped: a literal "\n" before the
# name ("...py.\nMargaret Holloway") must not defeat the replacer's word boundary.
ECHOED_FILE = ("Yaroslav Kuznetsov - maintainer, src/billing.py.\nMargaret Holloway - reviewer.\n"
               "Николай Петров - contact.\n")


@pytest.mark.parametrize("field", PROTO_FIELDS)
def test_known_names_after_literal_escape_are_masked(field):
    sid = new_sid("esc")
    prime_proto, prime_body = _build(field, "Maintainer: Margaret Holloway\nКонтакт: Николай Петров\n")
    assert prime_proto.send(sid, prime_body).status == 200
    text = json.dumps({"exit_code": 0, "output": ECHOED_FILE}, ensure_ascii=False)
    assert "\\nMargaret" in text
    proto, body = _build(field, text)
    r = proto.send(sid, body)
    assert r.status == 200, r.raw[:500]
    blob = dumps(proto.last_up())
    for stem in ["Margaret", "Holloway", "Николай", "Петров"]:
        assert stem.lower() not in blob.lower(), f"{stem!r} leaked ({field}): {blob[:1200]}"
