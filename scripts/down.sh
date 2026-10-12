#!/usr/bin/env bash
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/_env.sh"
for n in litellm palena presidio mock redis; do stop_pid $n; done
