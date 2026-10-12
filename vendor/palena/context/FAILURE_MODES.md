# FAILURE_MODES.md — what happens when things break

The service is a PII gate on the hot path. Failure semantics matter
because either extreme is dangerous: fail-open leaks PII to the LLM
vendor; fail-closed too eagerly and users can't get responses. This
doc pins down what we do in every failure class, why, and how it
composes with LiteLLM's own `unreachable_fallback` knob.

## Guiding principles

1. **Pre-call: fail closed.** If we cannot guarantee the LLM won't see
   real PII, we block. LiteLLM's `unreachable_fallback` gives operators
   a final "actually let it through" override, but by default we bounce.
2. **Post-call: fail best-effort.** A successful LLM response has
   already been generated. Losing the reversal is annoying (client sees
   pseudonyms) but not a data-leak. Never turn a successful LLM call
   into a failed request just because we can't reverse.
3. **No PII in error messages, logs, metrics, or audit metadata. Ever.**
   This is a hard invariant enforced by code review + the audit
   package.

## HTTP status → LiteLLM interpretation

LiteLLM treats specific status codes specially:

| Status | LiteLLM interpretation | Use for |
| --- | --- | --- |
| 200 | Guardrail responded normally; body's `action` decides | All non-terminal cases |
| 400 | Client (LiteLLM) sent bad data; do not retry | Bad JSON, missing required fields, malformed session id |
| 401 / 403 | Auth failure; do not retry | Missing / wrong `x-api-key` |
| 413 | Payload too large | Request or image exceeds byte cap |
| 429 | Rate limited; LiteLLM may retry with backoff | Presidio rate limits (rare) |
| **502 / 503 / 504** | **Unreachable dependency** — respects `unreachable_fallback` | Presidio / Redis down |
| 500 | Terminal internal error — no retry, blocks the call | Our own bugs |

Reserving 500 for "our own bugs" means operators can page us on any
non-zero 500 rate. Depedency failures show as 502.

## Pre-call failure matrix

| Failure | Symptom | HTTP status | Response body | Effect (default `fail_closed`) |
| --- | --- | --- | --- | --- |
| Presidio Analyzer 5xx / timeout / connect refused | Cannot detect text PII | 502 | `{"error": "presidio_unreachable"}` | LLM call blocked |
| Presidio Image Redactor unreachable | Cannot detect image PII | 502 | `{"error": "image_redactor_unreachable"}` | LLM call blocked |
| Redis unreachable | Cannot persist mapping | 502 | `{"error": "mapping_store_unreachable"}` | LLM call blocked |
| Redis returns corrupt JSON in `mapping` field | Cannot deserialize existing state | 200 | `{"action": "GUARDRAIL_INTERVENED", …}` | Continue with empty mapping (fresh start); log ERROR; user has no cross-turn continuity but request proceeds |
| Presidio returns malformed JSON | Detection broken | 502 | `{"error": "presidio_malformed"}` | LLM call blocked |
| Pseudonym pool empty AND synthetic prefix undefined for entity type | Cannot assign | 500 | `{"error": "internal"}` | LLM call blocked; page on this |
| Session id derives to empty | Wiring bug | 400 | `{"error": "session_id_derivation_failed"}` | LLM call blocked |
| Request > `MAX_REQUEST_BYTES` | Payload too big | 413 | `{"error": "request_too_large"}` | LLM call blocked |
| Image > `MAX_IMAGE_BYTES` | Payload too big | 413 | `{"error": "image_too_large"}` | LLM call blocked |
| More than `MAX_IMAGES_PER_REQUEST` images | Too many | 200 | `{"action": "BLOCKED", "blocked_reason": "too_many_images"}` | User sees explanation |
| Document detected in `structured_messages` or `images` | Not supported in v1 | 200 | `{"action": "BLOCKED", "blocked_reason": "documents_not_supported_in_v1"}` | User sees explanation |
| Request context canceled by LiteLLM (client hang-up) | Client gone | (no response) | (no response) | Downstream Redis / Presidio calls also canceled |

## Post-call failure matrix

Post-call runs against a response the LLM already produced. Guiding
rule: **never lose the response** because our reversal failed.

| Failure | Symptom | HTTP status | Response body | Effect |
| --- | --- | --- | --- | --- |
| Redis unreachable | Cannot load reverse map | 200 | `{"action": "NONE"}` | Client sees pseudonyms in response; log ERROR |
| Redis returns corrupt mapping | Cannot deserialize | 200 | `{"action": "NONE"}` | Same |
| Malformed response body from LLM (empty texts, non-string content) | Nothing to reverse | 200 | `{"action": "NONE"}` | Passthrough |
| Reversal produces identical text (no pseudonyms to reverse) | Common case | 200 | `{"action": "NONE"}` | LiteLLM emits unchanged |
| Request > `MAX_REQUEST_BYTES` | Payload too big | 413 | `{"error": "request_too_large"}` | Post-call skipped (LiteLLM `unreachable_fallback` may fail-open here to preserve the response) |

## Streaming failure matrix

Only relevant when `additional_provider_specific_params.streaming_transform_mode
== "incremental_diff"` AND LiteLLM's streaming-transform PR is deployed.

| Failure | Symptom | HTTP status | Effect |
| --- | --- | --- | --- |
| Redis unreachable during a sample | Cannot look up reverse map | 200 | `{"action": "NONE"}` — chunks flow unchanged for this sample; retry on next |
| Guardrailed text is shorter than what was already emitted | Would need to unsend bytes | 200 | `{"action": "BLOCKED", "blocked_reason": "stream_transform_underflow"}` — LiteLLM emits an in-stream error and terminates |
| Presidio unreachable during a sample | Same as Redis, but Presidio is only used on `input_type=request`, so shouldn't fire | 200 | `{"action": "NONE"}` |

## `unreachable_fallback` and how we interact

The operator sets this in LiteLLM config:

```yaml
guardrails:
  - guardrail_name: pseudonymizer
    litellm_params:
      guardrail: generic_guardrail_api
      unreachable_fallback: fail_closed   # or fail_open
```

- **`fail_closed` (default, our recommendation):** any 5xx from us
  fails the LLM call with a clear error. This is the safe posture for a
  PII gate.
- **`fail_open`:** LiteLLM logs a critical error and lets the LLM call
  through. Use only when the operator has determined that missing PII
  masking is preferable to service outages.

Neither posture is our responsibility to enforce — we just return the
right status code and let LiteLLM policy decide. What we DO enforce is
"never fail-open silently on our own initiative" — if we can't detect
PII, we say so (5xx), we do not pretend detection succeeded.

## No-PII invariant

The audit package (`internal/audit`) is the enforcement point:

- Provides typed counter builders. `RecordEntitiesDetected(sessionID
  string, entityType string, count int)` — the entity TYPE is a fixed
  vocabulary (PERSON, ORGANIZATION, LOCATION, …) so it's safe as a
  metric label; the entity TEXT is never accepted as an argument.
- Rejects any function that would take a `real` or `pseudonym` value
  as a metric or log field (compile-time enforcement via unexported
  types).
- Redacts session ids to a 12-char SHA-256 prefix when included in
  Prometheus labels — cardinality control AND PII protection.

Reviewers reject any PR that logs a variable named `real`, `pseudo`,
`entity_text`, `name`, `person`, or any string returned from Presidio's
`text` field. This is a bright line.

## Audit metadata schema

Successful requests emit at INFO level ONE structured log line, plus
Prometheus counters:

```
{
  "level": "info",
  "msg": "guardrail_applied",
  "input_type": "request",
  "call_id": "chatcmpl-abc123",
  "session_id_hash": "3f7b1a…",
  "session_id_source": "user_api_key_end_user_id",
  "duration_ms": 42,
  "entities_detected": 3,
  "entities_pseudonymized": 3,
  "entity_types": {"PERSON": 2, "ORGANIZATION": 1},
  "session_mapping_size": 8,
  "new_mappings_created": 1,
  "images_processed": 1,
  "images_pii_found": 1,
  "images_redacted": 1,
  "documents_processed": 0,
  "action": "GUARDRAIL_INTERVENED"
}
```

**Explicitly not in the log line:**
- Any real name.
- Any pseudonym.
- The mapping itself.
- The session id in clear (only its SHA-256 prefix).
- The Presidio API URL (operators know it; no reason to log it per request).

## Prometheus metrics

Minimum viable set for M1:

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `pseudonymizer_requests_total` | counter | `input_type`, `action` | Total requests processed. |
| `pseudonymizer_request_duration_seconds` | histogram | `input_type` | End-to-end handler duration. |
| `pseudonymizer_entities_detected_total` | counter | `entity_type` | Entities returned by Presidio. |
| `pseudonymizer_entities_pseudonymized_total` | counter | `entity_type` | Entities we actually rewrote. |
| `pseudonymizer_new_mappings_total` | counter | `entity_type` | New pool assignments. |
| `pseudonymizer_presidio_errors_total` | counter | `service` (`analyzer`/`image_redactor`), `kind` (`5xx`/`timeout`/`connect`/`malformed`) | Presidio failures. |
| `pseudonymizer_mapping_errors_total` | counter | `kind` (`read`/`write`/`corrupt`) | Redis failures. |
| `pseudonymizer_session_id_source_total` | counter | `source` | Where we derived the session id. |
| `pseudonymizer_block_reasons_total` | counter | `reason` | Enumerated block reasons. |
| `pseudonymizer_stream_underflow_total` | counter | (none) | How often streaming underflow forces a block. |

Do **not** label any metric with `session_id`, `user_id`, `team_id`, or
similar high-cardinality fields — Prometheus tears out at ~10k label
combos per metric. If we need to slice by team, use exemplars or push
to another observability backend.

## Alerting hints (for operators, not us)

The alerts belong to the operator's Prometheus rules, but we document
the intended thresholds so they know what "good" looks like:

- `rate(pseudonymizer_presidio_errors_total[5m]) > 0.1` → page on-call.
- `rate(pseudonymizer_mapping_errors_total{kind="read"}[5m]) > 0.5` →
  page on-call.
- `rate(pseudonymizer_session_id_source_total{source="litellm_call_id"}[10m]) >
  rate(pseudonymizer_session_id_source_total{source!="litellm_call_id"}[10m])`
  → wiring is wrong; investigate.
- `histogram_quantile(0.99, pseudonymizer_request_duration_seconds) > 2`
  → latency budget breach.
- `rate(pseudonymizer_stream_underflow_total[5m]) > 0.01` → holdback
  algorithm is producing bad output; investigate.

## Fail-closed audit

Every fail-closed decision (any 5xx or `action: BLOCKED`) is logged at
WARN with a fixed vocabulary of `reason` strings. That gives operators
a searchable log to prove pseudonymization compliance during audits —
"show me every request we blocked and why."
