#!/usr/bin/env bash
# Restart LiteLLM only (picks up litellm/config.yaml and litellm/pii_*.py changes).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
stop_pid litellm
start_bg litellm "$ROOT" "$ROOT/.venv/bin/litellm" --config litellm/config.yaml --host 127.0.0.1 --port $PORT_LITELLM
wait_http "http://127.0.0.1:$PORT_LITELLM/health/liveliness" litellm 180
