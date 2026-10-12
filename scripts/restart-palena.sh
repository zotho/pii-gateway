#!/usr/bin/env bash
# Rebuild (unless NOBUILD=1) and restart Palena. Env overrides: export PALENA_PSEUDONYMIZER_* first.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
stop_pid palena
if [[ -z "${NOBUILD:-}" ]]; then (cd "$ROOT/vendor/palena" && CGO_ENABLED=0 go build -o bin/server ./cmd/server); fi
start_palena
wait_http "http://127.0.0.1:$PORT_PALENA/readyz" palena 30
