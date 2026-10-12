#!/usr/bin/env bash
# (Re)create the fake-PII canary repo at .run/canary-repo from fixtures/canary-repo/
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
D="$RUN/canary-repo"
rm -rf "$D"; mkdir -p "$D"
cp -R "$ROOT/fixtures/canary-repo/." "$D/"
cd "$D"
git init -q
git add -A && git -c user.email=a@b.c -c user.name=t commit -qm init
echo "canary repo at $D"
