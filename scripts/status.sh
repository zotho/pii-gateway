#!/usr/bin/env bash
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
for p in "redis:$PORT_REDIS" "mock:$PORT_MOCK" "presidio:$PORT_PRESIDIO" "palena:$PORT_PALENA" "litellm:$PORT_LITELLM"; do
  n=${p%%:*}; port=${p##*:}
  if is_running $n; then s="running pid $(cat "$(pidfile $n)")"; else s="DOWN"; fi
  printf '%-9s :%-6s %s\n' $n $port "$s"
done
curl -s -m 3 "http://127.0.0.1:$PORT_PALENA/readyz"; echo
