#!/usr/bin/env bash
# Smoke test: shows what the mock upstream received vs what the client got back.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
L="http://127.0.0.1:$PORT_LITELLM"; M="http://127.0.0.1:$PORT_MOCK"; K="Authorization: Bearer $LITELLM_MASTER_KEY"
PY="$ROOT/.venv/bin/python"
fail=0

show_upstream() { # path
  curl -s "$M/_requests?path=$1" | "$PY" -c '
import json,sys
r=json.load(sys.stdin)[-1]["body"]
m=r["messages"] if "messages" in r else r.get("input")
print("  upstream received:", json.dumps(m, ensure_ascii=False))'
}
check() { # name text must_not_contain...
  local name="$1" text="$2"; shift 2
  for bad in "$@"; do
    if grep -qF -- "$bad" <<<"$UP"; then echo "  FAIL($name): upstream saw real value: $bad"; fail=1; fi
  done
}

run() { # label endpoint model sid text stream(0/1) extra_json
  local label="$1" ep="$2" model="$3" sid="$4" text="$5" stream="$6"
  curl -s -X POST "$M/_reset" >/dev/null
  local body
  body=$("$PY" -c '
import json,sys
ep,model,sid,text,stream=sys.argv[1:6]
b={"model":model,"metadata":{"session_id":sid},"stream":stream=="1"}
if ep=="/v1/messages":
    b.update(max_tokens=200,messages=[{"role":"user","content":text}])
elif ep=="/v1/responses":
    b.update(input=text)
else:
    b.update(messages=[{"role":"user","content":text}])
print(json.dumps(b))' "$ep" "$model" "$sid" "$text" "$stream")
  echo "== $label  ($ep, model=$model, stream=$stream)"
  echo "  client sent:       $text"
  local out; out=$(curl -s -m 60 -X POST "$L$ep" -H "$K" -H 'content-type: application/json' -d "$body")
  UP=$(curl -s "$M/_requests?path=$ep" | "$PY" -c 'import json,sys;r=json.load(sys.stdin);print(json.dumps(r[-1]["body"],ensure_ascii=False) if r else "NONE")')
  show_upstream "$ep"
  if [[ "$stream" == "1" ]]; then
    CL=$("$PY" -c '
import sys,json
t=""
for l in sys.stdin.read().splitlines():
    if not l.startswith("data:"): continue
    d=l[5:].strip()
    if d=="[DONE]": continue
    try: j=json.loads(d)
    except Exception: continue
    if not isinstance(j,dict): continue
    dl=j.get("delta")
    if isinstance(dl,dict): t+=dl.get("text","") or ""          # anthropic text_delta
    elif j.get("type")=="response.output_text.delta": t+=dl or ""  # responses
    for c in j.get("choices",[]) or []: t+=(c.get("delta") or {}).get("content") or ""
print(t)' <<<"$out")
    echo "  client received (stream, text deltas joined): $CL"
  else
    CL="$out"
    echo "  client received:   $out" | cut -c1-400
  fi
}

restored() { # name must_contain...   (client side got the real values back)
  local name="$1"; shift
  for good in "$@"; do
    if ! grep -qF -- "$good" <<<"$CL"; then echo "  FAIL($name): client did not get restored value: $good"; fail=1; fi
  done
  if grep -qE '<(PERSON|EMAIL_ADDRESS)_[0-9]+>' <<<"$CL"; then echo "  FAIL($name): pseudonym leaked to client"; fail=1; fi
}

T1="My name is Nikolai Petrov, email nikolai.petrov@example.com"
T2="Меня зовут Николай Петров"

run "EN chat"            /v1/chat/completions mock-gpt    smoke-1 "$T1" 0
check chat "$T1" "Nikolai" "Petrov" "nikolai.petrov@example.com"
restored chat "Nikolai Petrov" "nikolai.petrov@example.com"
run "EN anthropic"       /v1/messages         mock-claude smoke-2 "$T1" 0
check msg "$T1" "Nikolai" "Petrov" "nikolai.petrov@example.com"
restored msg "Nikolai Petrov" "nikolai.petrov@example.com"
run "RU chat"            /v1/chat/completions mock-gpt    smoke-3 "$T2" 0
check ru "$T2" "Николай" "Петров"
restored ru "Николай Петров"
run "EN chat STREAM"     /v1/chat/completions mock-gpt    smoke-4 "$T1" 1
check chat-s "$T1" "Nikolai" "Petrov" "nikolai.petrov@example.com"
restored chat-s "Nikolai Petrov" "nikolai.petrov@example.com"
run "EN anthropic STREAM" /v1/messages        mock-claude smoke-5 "$T1" 1
check msg-s "$T1" "Nikolai" "Petrov" "nikolai.petrov@example.com"
restored msg-s "Nikolai Petrov" "nikolai.petrov@example.com"
run "EN responses"       /v1/responses        mock-gpt    smoke-6 "$T1" 0
check resp "$T1" "Nikolai" "Petrov" "nikolai.petrov@example.com"
restored resp "Nikolai Petrov" "nikolai.petrov@example.com"
run "EN responses STREAM" /v1/responses       mock-gpt    smoke-7 "$T1" 1
check resp-s "$T1" "Nikolai" "Petrov" "nikolai.petrov@example.com"
restored resp-s "Nikolai Petrov" "nikolai.petrov@example.com"
run "RU anthropic STREAM" /v1/messages        mock-claude smoke-8 "$T2" 1
check msg-s-ru "$T2" "Николай" "Петров"
restored msg-s-ru "Николай Петров"

echo
[[ $fail == 0 ]] && echo "SMOKE: upstream never saw real PII; client got real values back (incl. streamed)" || echo "SMOKE: FAILURES above"
exit $fail
