# PROTOCOL.md — HTTP contract

This service implements the LiteLLM
[`generic_guardrail_api`](https://github.com/BerriAI/litellm/blob/main/litellm/proxy/guardrails/guardrail_hooks/generic_guardrail_api/generic_guardrail_api.py)
provider. All fields, semantics, and status codes below match what
LiteLLM sends and expects — do not invent new ones.

## Endpoint

```
POST /beta/litellm_basic_guardrail_api
Content-Type: application/json
```

This suffix is fixed. LiteLLM's `GenericGuardrailAPI.__init__` appends it
automatically to the configured `api_base`. If the request path is
anything else, respond with `404 Not Found`.

## Auth

Optional bearer/apikey. If configured, clients set:

```
x-api-key: <shared-secret>
```

We compare against `PALENA_PSEUDONYMIZER_API_KEY` in constant time. When
the env var is empty, we accept any request (dev mode) — log a WARN at
startup so operators can see the state.

## Health / metrics

- `GET /healthz` — always 200 unless the process is shutting down.
- `GET /readyz` — 200 iff we can reach Redis + Presidio Analyzer +
  Presidio Image Redactor within `presidio.readiness_timeout`. 503
  otherwise, with a JSON body naming the failing dependency.
- `GET /metrics` — Prometheus format via `promhttp.Handler()`.

## Request schema

```json
{
  "input_type": "request" | "response",
  "litellm_call_id": "…",
  "litellm_trace_id": "…",
  "litellm_version": "1.65.0",
  "model": "gpt-4o",
  "texts": ["…"],
  "structured_messages": [
    { "role": "user", "content": "…" },
    { "role": "user", "content": [{"type": "text", "text": "…"},
                                   {"type": "image_url", "image_url": {"url": "data:image/png;base64,…"}}]}
  ],
  "images": ["data:image/png;base64,…"],
  "tools": [ /* ChatCompletionToolParam */ ],
  "tool_calls": [ /* ChatCompletionMessageToolCall */ ],
  "request_data": {
    "user_api_key_hash": "…",
    "user_api_key_alias": "…",
    "user_api_key_user_id": "…",
    "user_api_key_user_email": "…",
    "user_api_key_team_id": "…",
    "user_api_key_team_alias": "…",
    "user_api_key_end_user_id": "…",
    "user_api_key_org_id": "…"
  },
  "request_headers": {"x-request-id": "abc", "authorization": "[present]"},
  "additional_provider_specific_params": {
    "session_id_metadata_key": "chat_session_id",
    "streaming_transform_mode": "incremental_diff",
    "score_threshold": 0.8
  }
}
```

Field-by-field semantics:

| Field | Notes |
| --- | --- |
| `input_type` | `"request"` before the LLM call, `"response"` after. Same endpoint serves both — the field is the only phase indicator. |
| `litellm_call_id` | Unique per LLM call. **Stable across the pre-call and post-call of the same request** — that's what makes single-request pseudonymize+reverse work without any client cooperation. |
| `litellm_trace_id` | Optional. Set when the client passes W3C `traceparent`. Meant to be stable across multiple LLM calls for the same user turn. |
| `model` | Target model. We don't gate on it, but include in audit metadata. |
| `texts` | Extracted plain-text bodies. On `input_type=request`: one entry per string-content message. On `input_type=response`: one combined text per choice. **This is the field we care about for pseudonymization.** |
| `structured_messages` | Full OpenAI-shape messages including multimodal content blocks. Currently informational for us — we operate on `texts` / `images` and let LiteLLM zip results back. |
| `images` | Base64 data URLs OR remote URLs. We MUST accept both. On response, we return same-length list with redacted counterparts. |
| `tools` / `tool_calls` | Passed through verbatim. Never modified. |
| `request_data` | LiteLLM's user/team identifiers (TypedDict). Consumed by session-id derivation — see [SESSION_AND_STATE.md](SESSION_AND_STATE.md#derivation). |
| `request_headers` | Sanitized inbound headers (values on the allowlist; others as `[present]`). We don't currently use them, but must not error on their presence. |
| `additional_provider_specific_params` | Free-form config bag set by the operator OR overridden per request via LiteLLM's `guardrails: [{name: {extra_body: {…}}}]` syntax. Reserved keys documented in [CONFIG.md](CONFIG.md#request-overrides). |

**Content-Length limit.** Reject bodies larger than
`PALENA_PSEUDONYMIZER_MAX_REQUEST_BYTES` (default 32 MiB) with HTTP 413.
Images alone can push a request past several MiB; the limit is high but
finite to protect against runaway payloads.

## Response schema

```json
{
  "action": "BLOCKED" | "NONE" | "GUARDRAIL_INTERVENED",
  "blocked_reason": "…",
  "texts": ["…"],
  "images": ["data:image/png;base64,…"],
  "tools": [ /* unchanged unless we transformed */ ],
  "stream_holdback_chars": [12, 0]
}
```

Rules for each action:

### `NONE`

- Nothing detected, nothing changed.
- `texts`, `images`, `tools`, `stream_holdback_chars` MAY be omitted.
- LiteLLM emits inputs unchanged.

### `GUARDRAIL_INTERVENED`

- We modified at least one entry in `texts` OR `images` OR `tools`.
- Every field that was modified must be present in the response — LiteLLM
  substitutes by index. **Length MUST match the request** (v1 1:1
  contract). If a transform would require length change (e.g. a
  document expanding into N page images), return `BLOCKED` with reason
  `length_change_not_supported`.
- Order MUST match the request.

### `BLOCKED`

- `blocked_reason` is required — human-readable message returned to the
  end user via LiteLLM's `GuardrailRaisedException`.
- `texts`, `images` MAY be omitted.
- The end user sees this text verbatim — no PII, no mapping details.

## Streaming

The streaming path uses the **same endpoint** with `input_type: "response"`.
Framework logistics (chunk accumulation, sampling, diff emission) are on
LiteLLM's side (see the upstream streaming-transform PR referenced in
[IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md#dependencies)).

Our contribution to make streaming reversal safe:

```json
{
  "action": "GUARDRAIL_INTERVENED",
  "texts": ["The user Marcus Weiss asked about the "],
  "stream_holdback_chars": [12]
}
```

- `stream_holdback_chars[i]` — number of trailing characters in `texts[i]`
  the framework should hold back until the next sample. Required when
  the trailing text could be the start of an in-progress pseudonym
  (algorithm in [PSEUDONYMIZATION_ALGORITHM.md](PSEUDONYMIZATION_ALGORITHM.md#streaming-holdback)).
- Omitted or 0 → framework may emit the full delta.
- Guardrails opting out of transform-streaming should never set this
  field; LiteLLM will run in `block_only` mode.

## Error responses

The HTTP layer maps failures to LiteLLM's expectations:

| Situation | HTTP status | Body |
| --- | --- | --- |
| Bad JSON / missing required field | 400 | `{"error": "…"}` — LiteLLM treats as terminal, non-retriable |
| Presidio unreachable, pre-call, fail-closed | 502 | `{"error": "presidio_unreachable"}` — LiteLLM treats as unreachable (retries or fails per `unreachable_fallback`) |
| Presidio unreachable, post-call | 200 | `{"action": "NONE"}` — best-effort, don't lose the response |
| Redis unreachable, pre-call | 502 | `{"error": "mapping_store_unreachable"}` |
| Redis unreachable, post-call | 200 | `{"action": "NONE"}` |
| Internal error, unclassified | 500 | `{"error": "internal"}` — logged with stack, no PII |

Note that LiteLLM specifically treats **502/503/504** as "unreachable"
for its `unreachable_fallback: fail_open` semantics. Any other 5xx is
treated as a terminal failure. Use 502 (not 500) whenever the failure
is a downstream dependency the operator can fix (Presidio down, Redis
down); reserve 500 for our own bugs.

## Non-blocking design

The handler must return within the `presidio.timeout_seconds +
mapping.timeout_seconds + margin` budget. Both external calls run
under `context.WithTimeout`, propagated from the request's context.
Cancellation on the LiteLLM side (client hang-up) MUST tear down our
Redis/Presidio calls too — never let goroutines linger past the request.

## Idempotency

Because LiteLLM may retry on `unreachable_fallback: fail_open` failures
or transiently timeout and re-issue, all `input_type: "request"` calls
must be idempotent w.r.t. Redis state:

- Adding an existing `real → pseudonym` mapping is a no-op (existing
  entries win; see [SESSION_AND_STATE.md](SESSION_AND_STATE.md#add_mappings)).
- `mark_processed(session_id, indices)` is a set union — reprocessing
  the same indices is a no-op.

## Example: pre-call

Request:

```json
{
  "input_type": "request",
  "litellm_call_id": "chatcmpl-abc123",
  "model": "gpt-4o",
  "texts": ["Please review the Novartis contract for Thomas Weber."],
  "structured_messages": [
    {"role": "user", "content": "Please review the Novartis contract for Thomas Weber."}
  ],
  "request_data": {
    "user_api_key_end_user_id": "customer-42",
    "user_api_key_team_id": "legal"
  },
  "additional_provider_specific_params": {}
}
```

Response:

```json
{
  "action": "GUARDRAIL_INTERVENED",
  "texts": ["Please review the Acme Corp contract for Anna Schmidt."]
}
```

## Example: post-call

Request:

```json
{
  "input_type": "response",
  "litellm_call_id": "chatcmpl-abc123",
  "texts": ["The Acme Corp filing is due by Friday. Anna Schmidt should sign off."],
  "request_data": {"user_api_key_end_user_id": "customer-42"}
}
```

Response:

```json
{
  "action": "GUARDRAIL_INTERVENED",
  "texts": ["The Novartis filing is due by Friday. Thomas Weber should sign off."]
}
```

## Example: pre-call, blocking image

Request contains an image with unpseudonymizable PII and the operator
configured `non_text_pii_action: block`:

```json
{
  "action": "BLOCKED",
  "blocked_reason": "This attachment contains personal data (PERSON, LOCATION) that cannot be pseudonymized. Please remove sensitive information and try again."
}
```

## Example: streaming sample with holdback

The framework has emitted 40 chars so far. It samples a new batch and
calls us with the full accumulated text (85 chars). The tail could be
the start of "Thomas Weber":

Request:

```json
{
  "input_type": "response",
  "litellm_call_id": "chatcmpl-abc123",
  "texts": ["The report was signed by Anna Schmidt and delivered to Thomas We"],
  "additional_provider_specific_params": {"streaming_transform_mode": "incremental_diff"}
}
```

Response:

```json
{
  "action": "GUARDRAIL_INTERVENED",
  "texts": ["The report was signed by Novartis and delivered to Thomas We"],
  "stream_holdback_chars": [15]
}
```

The framework emits chars 40..len-15 as a delta and keeps the last 15
chars ("delivered to Th…") buffered for the next sample, when they can
be safely resolved.
