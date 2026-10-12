# shared helpers; sourced by other scripts
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN="$ROOT/.run"
mkdir -p "$RUN/logs" "$RUN/redis" "$RUN/upstream"
export PATH="/opt/homebrew/bin:$PATH"

# LiteLLM master key: from env, else generated once into .run/master.key (chmod 600)
if [[ -z "${LITELLM_MASTER_KEY:-}" ]]; then
  if [[ ! -s "$RUN/master.key" ]]; then
    ( umask 077; echo "sk-$(openssl rand -hex 24)" > "$RUN/master.key" )
  fi
  chmod 600 "$RUN/master.key"
  LITELLM_MASTER_KEY="$(cat "$RUN/master.key")"
fi
export LITELLM_MASTER_KEY

PORT_LITELLM=14000; PORT_PALENA=18080; PORT_PRESIDIO=15002; PORT_REDIS=16379; PORT_MOCK=19000

# palena env (override by exporting before calling up.sh / restart-palena.sh)
palena_env() {
  export PALENA_PSEUDONYMIZER_HTTP_ADDR="127.0.0.1:$PORT_PALENA"
  export PALENA_PSEUDONYMIZER_LOG_FORMAT="${PALENA_PSEUDONYMIZER_LOG_FORMAT:-json}"
  export PALENA_PSEUDONYMIZER_LOG_LEVEL="${PALENA_PSEUDONYMIZER_LOG_LEVEL:-debug}"
  export PALENA_PSEUDONYMIZER_REDIS_URL="redis://127.0.0.1:$PORT_REDIS/0"
  export PALENA_PSEUDONYMIZER_REDIS_SESSION_TTL_SECONDS="${PALENA_PSEUDONYMIZER_REDIS_SESSION_TTL_SECONDS:-0}"
  export PALENA_PSEUDONYMIZER_PRESIDIO_ANALYZER_URL="http://127.0.0.1:$PORT_PRESIDIO"
  # no image redactor locally: presidio/app.py answers GET /health on the same port so /readyz is green
  export PALENA_PSEUDONYMIZER_PRESIDIO_IMAGE_REDACTOR_URL="http://127.0.0.1:$PORT_PRESIDIO"
  export PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGES="${PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGES:-en,ru}"
  export PALENA_PSEUDONYMIZER_PRESIDIO_ENTITIES="${PALENA_PSEUDONYMIZER_PRESIDIO_ENTITIES:-PERSON,ORGANIZATION,EMAIL_ADDRESS,PHONE_NUMBER,SECRET}"
  export PALENA_PSEUDONYMIZER_ENTITY_STRATEGY="${PALENA_PSEUDONYMIZER_ENTITY_STRATEGY:-PERSON:token,ORGANIZATION:token,SECRET:token}"
  # entity types whose <TYPE_n> tokens are NOT restored in responses (secrets stay masked)
  export PALENA_PSEUDONYMIZER_NO_RESTORE_ENTITIES="${PALENA_PSEUDONYMIZER_NO_RESTORE_ENTITIES:-SECRET}"
  export PALENA_PSEUDONYMIZER_ALLOWLIST_FILE="${PALENA_PSEUDONYMIZER_ALLOWLIST_FILE:-$ROOT/config/allowlist.txt}"
  # NER single-word PERSON/ORGANIZATION hits on these dictionary words ("Client", "Contact") are dropped; regenerate: scripts/build_common_words.py
  export PALENA_PSEUDONYMIZER_COMMON_WORDS_FILE="${PALENA_PSEUDONYMIZER_COMMON_WORDS_FILE:-$ROOT/config/common_words.txt}"
  export PALENA_PSEUDONYMIZER_ENTITY_STRATEGY_DEFAULT="${PALENA_PSEUDONYMIZER_ENTITY_STRATEGY_DEFAULT:-token}"
}

pidfile() { echo "$RUN/$1.pid"; }
is_running() { local f; f="$(pidfile "$1")"; [[ -f "$f" ]] && kill -0 "$(cat "$f")" 2>/dev/null; }

wait_http() { # url name timeout_s
  local i
  for ((i=0;i<${3:-60};i++)); do
    if curl -fsS -o /dev/null -m 2 "$1" 2>/dev/null; then echo "  $2 ready"; return 0; fi
    sleep 1
  done
  echo "  $2 NOT ready after ${3:-60}s (see $RUN/logs)"; return 1
}

start_bg() { # name  cwd  cmd...
  local name="$1" cwd="$2"; shift 2
  if is_running "$name"; then echo "  $name already running (pid $(cat "$(pidfile "$name")"))"; return 0; fi
  ( cd "$cwd" || exit 1; nohup "$@" >> "$RUN/logs/$name.log" 2>&1 </dev/null & echo $! > "$(pidfile "$name")" )
}

stop_pid() { # name : kill only the recorded pid
  local f; f="$(pidfile "$1")"
  [[ -f "$f" ]] || { echo "  $1: no pidfile"; return 0; }
  local pid; pid="$(cat "$f")"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"; local i
    for ((i=0;i<20;i++)); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid"
    echo "  $1 stopped (pid $pid)"
  else echo "  $1 not running (stale pid $pid)"; fi
  rm -f "$f"
}

start_palena() {
  palena_env
  start_bg palena "$ROOT/vendor/palena" "$ROOT/vendor/palena/bin/server"
}

# bridge observability (docs/OBSERVABILITY.md): Prometheus port of the mock-stack LiteLLM (real-up.sh overrides to 14091)
export PII_BRIDGE_METRICS_PORT="${PII_BRIDGE_METRICS_PORT:-14090}"
