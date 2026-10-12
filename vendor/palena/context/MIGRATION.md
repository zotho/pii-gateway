# MIGRATION.md — Python module → Go package mapping

Use this as a lookup table when porting a piece of logic from
[`palena-guardrail-pseudonymizer`](../../palena-guardrail-pseudonymizer/)
(Python) to this repo (Go). The Python code is ground truth for
edge-case behavior — copy the test cases verbatim.

## Package layout mapping

| Python module | Go package | Notes |
| --- | --- | --- |
| `guardrail.py` — `PseudonymizerGuardrail.execute` | `internal/pseudonymizer/orchestrator.go` — `Orchestrator.Handle(ctx, req) (*Response, error)` | Same responsibility: dispatch to text / image handlers, aggregate results. The Go version returns a `Response` DTO consumed by `internal/httpapi`. |
| `text_handler.py` — `TextHandler` | `internal/text/handler.go` — `Handler.Pseudonymize` / `Handler.Reverse` | Same public API. |
| `replacer.py` — `TextReplacer` | `internal/text/replacer.go` — `Replacer.Replace(text, mapping) string` | RE2 caveats in [PSEUDONYMIZATION_ALGORITHM.md](PSEUDONYMIZATION_ALGORITHM.md#replacer-rules). |
| `streaming.py` — `StreamingReplacer` | `internal/text/holdback.go` — `ComputeHoldback(buf, keys, maxKeyLen) int` | The Go version is stateless; LiteLLM buffers on our behalf. |
| `pools.py` — `PoolManager` | `internal/text/pools.go` — `Pools.Assign(entityType, used) string` | Config-driven, matches Python signature. |
| `presidio_client.py` — `PresidioAnalyzerClient` | `internal/presidio/analyzer.go` — `Analyzer.Analyze(ctx, text, entities, threshold, lang) ([]Detection, error)` | Add context.Context first-arg — everything in Go takes ctx. |
| `image_handler.py` — `ImageHandler` | `internal/image/handler.go` — `Handler.Process(ctx, image, sessionID) (*Result, error)` | Same. |
| `image_redactor_client.py` — `PresidioImageRedactorClient` | `internal/presidio/image_redactor.go` — `ImageRedactor.RedactAndAnalyze(ctx, bytes) (redactedBytes []byte, detections []ImageDetection, err error)` | Multipart handling in Go is a bit more manual (`multipart.Writer`). |
| `document_handler.py` — `DocumentHandler` | (v2 only) | Not ported for v1. |
| `mapping.py` — `MappingManager` | `internal/mapping/redis.go` — `Store` interface + `RedisStore` impl | Interface so tests can substitute miniredis or in-memory. |
| `config.py` — `PseudonymizerConfig` | `internal/config/config.go` — `Config` struct | Populated via `envconfig`, validated in `Config.Validate`. |
| `litellm_adapter.py` — `PseudonymizerLiteLLMGuardrail` | (retired) | The HTTP protocol IS the adapter; no equivalent needed. |
| `exceptions.py` — `PseudonymizerError` and subclasses | `internal/errs/errs.go` — sentinel `error` values + typed wrappers | Go uses `errors.Is` / `errors.As`; keep the same categories. |

## Redis schema (unchanged)

The two services can share Redis:

- Key: `{prefix}:{session_id}` — same.
- Hash field `mapping`: JSON `{real: pseudo}` — same.
- Hash field `processed_indices`: JSON `[…]` — same schema, but the Go
  service does not read/write it in v1 (see
  [SESSION_AND_STATE.md](SESSION_AND_STATE.md#processed-indices-http-model-quirk)).

## Config schema (transposed)

Python `PseudonymizerConfig` was a nested Pydantic model. The Go
version uses flat env vars, one per leaf. Correspondence:

| Python path | Env var |
| --- | --- |
| `PseudonymizerConfig.enabled` | (n/a — handled by LiteLLM's `default_on`) |
| `PseudonymizerConfig.presidio.analyzer_url` | `PALENA_PSEUDONYMIZER_PRESIDIO_ANALYZER_URL` |
| `PseudonymizerConfig.presidio.image_redactor_url` | `PALENA_PSEUDONYMIZER_PRESIDIO_IMAGE_REDACTOR_URL` |
| `PseudonymizerConfig.presidio.timeout_seconds` | `PALENA_PSEUDONYMIZER_PRESIDIO_TIMEOUT_SECONDS` |
| `PseudonymizerConfig.presidio.score_threshold` | `PALENA_PSEUDONYMIZER_PRESIDIO_SCORE_THRESHOLD` |
| `PseudonymizerConfig.redis.url` | `PALENA_PSEUDONYMIZER_REDIS_URL` |
| `PseudonymizerConfig.redis.session_ttl` | `PALENA_PSEUDONYMIZER_REDIS_SESSION_TTL_SECONDS` |
| `PseudonymizerConfig.redis.key_prefix` | `PALENA_PSEUDONYMIZER_REDIS_KEY_PREFIX` |
| `PseudonymizerConfig.entities` | `PALENA_PSEUDONYMIZER_PRESIDIO_ENTITIES` (comma-separated) |
| `PseudonymizerConfig.language` | `PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGE` |
| `PseudonymizerConfig.pools.PERSON` | `PALENA_PSEUDONYMIZER_POOL_PERSON` (comma-separated) |
| `PseudonymizerConfig.non_text_content.pii_action` | `PALENA_PSEUDONYMIZER_NON_TEXT_PII_ACTION` |
| `PseudonymizerConfig.non_text_content.block_message` | `PALENA_PSEUDONYMIZER_NON_TEXT_BLOCK_MESSAGE` |
| `PseudonymizerConfig.non_text_content.max_image_bytes` | `PALENA_PSEUDONYMIZER_MAX_IMAGE_BYTES` |

## Semantic differences (worth calling out)

### 1. No `execute()` — request/response are the API boundary

Python's `execute(ShieldContext)` was called once per phase. The Go
service exposes a single HTTP endpoint whose `input_type` distinguishes
phases. There's no `ShieldContext` — the request body IS the context.

### 2. Session id derivation is different

Python derived a `session_id` from LiteLLM's own metadata inside the
adapter. In this Go service, `session_id` is derived at the HTTP layer
from `additional_provider_specific_params.session_id_metadata_key` +
`request_data.user_api_key_end_user_id` + trace/call id. See the full
chain in [SESSION_AND_STATE.md](SESSION_AND_STATE.md#derivation-chain).

### 3. `processed_indices` is written but not read (v1)

Python used it to skip re-processing of message indices across turns.
The HTTP model gives us `texts[]`, not indexed messages, so we can't
easily map it. Idempotency of the replacer covers the practical case.

### 4. Streaming is protocol-level, not hook-level

Python implemented `on_stream_start / on_stream_chunk / on_stream_end`
and buffered inside the process. The Go service returns
`stream_holdback_chars` on the HTTP response and LiteLLM buffers on our
behalf. No in-process buffering.

### 5. Failure mapping is HTTP status codes, not exceptions

`PresidioUnavailableError` → HTTP 502. `MappingStoreError` → 502.
`DocumentTooLargeError` → 413. See
[FAILURE_MODES.md](FAILURE_MODES.md#http-status--litellm-interpretation).

### 6. No BaseGuardrail

There is no `BaseGuardrail` / `ShieldContext` / `Phase.BOTH`
abstraction — those were Palena-runtime concepts. The Go service is
LiteLLM-specific.

## Porting the Python tests

Copy the Python test cases as fixtures where the surface matches:

| Python test file | Go test | Notes |
| --- | --- | --- |
| `tests/test_replacer.py` | `internal/text/replacer_test.go` | 1:1 — feed the same inputs, assert the same outputs. |
| `tests/test_text_handler.py` | `internal/text/handler_test.go` | Same, but requires a fake analyzer + fake mapping store. |
| `tests/test_image_handler.py` | `internal/image/handler_test.go` | Same, with a fake image redactor. |
| `tests/test_mapping.py` | `internal/mapping/redis_test.go` | Use miniredis as the fake. |
| `tests/test_streaming.py` | `internal/text/holdback_test.go` | Same fixtures; the Go version is a pure function so easier to test. |
| `tests/test_guardrail.py` | `internal/pseudonymizer/orchestrator_test.go` | Same, but the assertion is on the returned `Response` DTO. |
| `tests/test_litellm_adapter.py` | `internal/httpapi/handler_test.go` | Rewrite as HTTP handler tests using `httptest.NewRecorder`. |

## What's NOT ported

- `litellm_adapter.py` — the whole point of the migration is to drop
  the in-process adapter.
- `pyproject.toml`, `MEMORY.md`, `.claude/` — Go equivalents live in
  `go.mod`, `Makefile`, and the CI config.
- Anything under `_pytest/` / `.venv/` — obviously.

## What we intentionally do the same

- Pool defaults verbatim.
- Regex-based replacer with the same behavioral rules.
- Redis hash schema.
- Presidio HTTP contract (we're just a different client).
- Session TTL default (3600s).
- Fail-closed pre-call posture.
- No-PII-in-audit invariant.
