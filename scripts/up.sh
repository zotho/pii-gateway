#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
cd "$ROOT"
echo "[redis]";    start_bg redis "$RUN/redis" /opt/homebrew/bin/redis-server "$ROOT/redis/redis.conf"
for i in $(seq 1 20); do /opt/homebrew/bin/redis-cli -p $PORT_REDIS ping 2>/dev/null | grep -q PONG && { echo "  redis ready"; break; }; sleep 0.5; done
echo "[mock upstream]"; start_bg mock "$ROOT" "$ROOT/.venv/bin/python" mock_upstream/server.py
wait_http "http://127.0.0.1:$PORT_MOCK/_health" mock 30
echo "[presidio analyzer]"; start_bg presidio "$ROOT" env PORT=$PORT_PRESIDIO "$ROOT/.venv/bin/python" presidio/app.py
wait_http "http://127.0.0.1:$PORT_PRESIDIO/health" presidio 180
echo "[palena]"
if [[ ! -x vendor/palena/bin/server ]]; then (cd vendor/palena && CGO_ENABLED=0 go build -o bin/server ./cmd/server); fi
start_palena
wait_http "http://127.0.0.1:$PORT_PALENA/readyz" palena 30
echo "[litellm]"
start_bg litellm "$ROOT" "$ROOT/.venv/bin/litellm" --config litellm/config.yaml --host 127.0.0.1 --port $PORT_LITELLM
wait_http "http://127.0.0.1:$PORT_LITELLM/health/liveliness" litellm 180
echo "all up"
