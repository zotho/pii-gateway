# e2e tests

Black-box tests: HTTP against LiteLLM (:14000) and the mock upstream control API (:19000), plus Redis
inspection and `scripts/*.sh` for restarts. The stack must be up (`bash scripts/up.sh`); the suite starts it if
the mock is unreachable. The master key is read from `LITELLM_MASTER_KEY` or `.run/master.key`.

    uv run pytest tests/e2e -m "not slow"     # fast, no service restarts
    uv run pytest tests/e2e                   # full: restarts Palena/Redis, stops Palena (fail-closed)
    uv run pytest tests/e2e -k anthropic -x   # one protocol

Files: 01 masking per field, 02 restore (non-stream/stream/tool args), 03 stability and session isolation,
04 thinking/reasoning passthrough, 05 concurrency, 06 persistence and TTL (slow), 07 fail-closed (slow),
08 unguarded routes, 09 tool output first sighting, 10 pattern-only system text, 11 audit and metrics,
12 common-word filter.

Tests run serially, reset the mock before each test and use a unique session id. `assert_no_canary`
re-serialises JSON-decoded bodies with `ensure_ascii=False` before searching for canary values.
