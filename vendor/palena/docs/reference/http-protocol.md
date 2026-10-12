# HTTP protocol

The service exposes one endpoint that the LiteLLM proxy calls both before and
after each guarded request.

```text
POST /beta/litellm_basic_guardrail_api
Content-Type: application/json
```

The `input_type` field distinguishes the two phases — there is no separate
endpoint for pre- and post-call.

A second endpoint, `POST /v1/waeg/check`, speaks the Wäg gateway's guardrail
protocol against the same pseudonymization core. It is documented separately in
[Wire into Wäg](/guide/waeg-integration); the rest of this page describes the
LiteLLM contract.

## Request

```json
{
  "input_type": "request",
  "litellm_call_id": "chatcmpl-abc",
  "litellm_trace_id": "trace-xyz",
  "model": "gpt-4o",
  "texts": ["Please book Alice Johnson a trip."],
  "images": ["data:image/png;base64,..."],
  "request_data": { "user_api_key_end_user_id": "customer-42" },
  "additional_provider_specific_params": {
    "streaming_transform_mode": "incremental_diff"
  }
}
```

| Field | Meaning |
| --- | --- |
| `input_type` | `"request"` (pre-call) or `"response"` (post-call). |
| `litellm_call_id` | Stable across the pre/post pair of one request. |
| `litellm_trace_id` | Stable across a multi-call conversation, when supplied. |
| `texts` | Text bodies to pseudonymize (request) or reverse (response). |
| `images` | Base64 data URLs or remote URLs (request phase). |
| `request_data` | LiteLLM user/team identifiers; used for session derivation. |
| `additional_provider_specific_params` | Per-request overrides, e.g. streaming mode. |

## Response

```json
{
  "action": "GUARDRAIL_INTERVENED",
  "texts": ["Please book Jordan Avery a trip."],
  "stream_holdback_chars": [0]
}
```

| `action` | Meaning |
| --- | --- |
| `NONE` | Nothing detected or changed; inputs pass through. |
| `GUARDRAIL_INTERVENED` | `texts` / `images` were modified; length and order match the request. |
| `BLOCKED` | Request denied; `blocked_reason` carries a user-facing message. |

`stream_holdback_chars` (streaming only) tells LiteLLM how many trailing
characters of each text to withhold until the next chunk, so a partial
pseudonym is never emitted.

## Status codes

| Situation | Status |
| --- | --- |
| Normal (action decides outcome) | `200` |
| Malformed request / bad field | `400` |
| Missing / wrong `x-api-key` | `401` |
| Payload too large | `413` |
| Presidio or Redis unreachable (pre-call) | `502` |
| Internal bug | `500` |

LiteLLM treats `502/503/504` as "unreachable" for its `unreachable_fallback`
policy — the service returns `502` for downstream dependency failures and
reserves `500` for its own errors.

## Health & metrics

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness — 200 while the process is up. |
| `GET /readyz` | Readiness — checks Redis + Presidio; 503 if a dependency is down. |
| `GET /metrics` | Prometheus metrics. |

## Session administration

| Endpoint | Purpose |
| --- | --- |
| `DELETE /sessions/{session_id}` | Erase a session's real↔pseudonym mapping (GDPR right-to-erasure / explicit teardown). Guarded by the `x-api-key` shared secret. Returns `200 {"deleted": true\|false}`; idempotent. |
