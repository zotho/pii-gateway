#!/usr/bin/env bash
# Real-provider stack: mitmproxy reverse capture (:18081 anthropic, :18082 chatgpt) + second LiteLLM :14001.
# Requires the base stack (scripts/up.sh). Reads the Codex auth file (CODEX_AUTH_FILE, default ~/.codex/auth.json) READ-ONLY for the ChatGPT
# token at runtime; values are only exported to the LiteLLM process, never printed or written.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
cd "$ROOT"
PORT_LITELLM_REAL=14001
mkdir -p "$RUN/real-upstream"
MITM="$RUN/mitm-venv/bin/mitmdump"
[[ -x $MITM ]] || { echo "missing $MITM (uv venv .run/mitm-venv && uv pip install --python .run/mitm-venv/bin/python mitmproxy)"; exit 1; }
echo "[mitm]"
start_bg mitm "$ROOT" "$MITM" --set confdir="$RUN/mitm-conf" --set termlog_verbosity=warn --set flow_detail=0 \
  --mode "reverse:https://api.anthropic.com@18081" --mode "reverse:https://chatgpt.com@18082" \
  -s "$ROOT/scripts/capture_upstream.py"
wait_http_any() { local i; for ((i=0;i<30;i++)); do (exec 3<>/dev/tcp/127.0.0.1/$1) 2>/dev/null && { echo "  mitm :$1 ready"; return 0; }; sleep 1; done; return 1; }
wait_http_any 18081; wait_http_any 18082
echo "[litellm real]"
AUTH="${CODEX_AUTH_FILE:-$HOME/.codex/auth.json}"
if [[ -f $AUTH ]]; then
  export CHATGPT_ACCESS_TOKEN="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["tokens"]["access_token"])' "$AUTH")"
  export CHATGPT_ACCOUNT_ID="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["tokens"]["account_id"])' "$AUTH")"
else echo "  WARN: no Codex auth file ($AUTH; set CODEX_AUTH_FILE); the ChatGPT route will not authenticate"; export CHATGPT_ACCESS_TOKEN=missing CHATGPT_ACCOUNT_ID=missing; fi
export LITELLM_LOG="${LITELLM_LOG:-INFO}"
export PII_BRIDGE_METRICS_PORT=14091
start_bg litellm-real "$ROOT" "$ROOT/.venv/bin/litellm" --config litellm/config.real.yaml --host 127.0.0.1 --port $PORT_LITELLM_REAL
wait_http "http://127.0.0.1:$PORT_LITELLM_REAL/health/liveliness" litellm-real 180
echo "real stack up"
