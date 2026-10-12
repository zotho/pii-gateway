# Wire into LiteLLM

The service implements LiteLLM's
[`generic_guardrail_api`](https://docs.litellm.ai/docs/proxy/guardrails/)
contract, so integration is pure proxy configuration — no custom code, no proxy
image rebuild.

## Minimal config

```yaml
guardrails:
  - guardrail_name: palena-pseudonymizer
    litellm_params:
      guardrail: generic_guardrail_api
      api_base: http://pseudonymizer.guardrails.svc:8080
      mode: [pre_call, post_call]
      default_on: true
      unreachable_fallback: fail_closed
```

- **`mode: [pre_call, post_call]`** — a single guardrail runs on both phases:
  masking before the model, restoring after.
- **`default_on: true`** — applies to every request. Drop it to opt in
  per-key/per-team (a LiteLLM Enterprise feature).
- **`unreachable_fallback: fail_closed`** — if the guardrail is unreachable,
  block rather than leak. Recommended.

## Enabling streaming reversal

To restore names in streamed responses, opt into transform-mode streaming:

```yaml
      streaming_transform_mode: incremental_diff
      streaming_sampling_rate: 1
      additional_provider_specific_params:
        streaming_transform_mode: incremental_diff
```

`streaming_transform_mode` is read by LiteLLM in two places — the top level and
the provider params — so set it in both. Requires LiteLLM v1.65+.

Without this, streamed responses reach the client with pseudonyms intact
(non-streaming requests are unaffected).

## Session id

Pseudonyms stay consistent across a conversation only when each turn carries a
stable session id. The service resolves one in this order:

1. `metadata.session_id` (or a key you name via `session_id_metadata_key`)
2. the OpenAI `user` field
3. `litellm_trace_id`
4. `litellm_call_id` (per-request — only stitches one request's pre/post)

Pass a stable `user` or `metadata.session_id` per conversation for multi-turn
consistency.

## Verifying

Send a request with a name and inspect what the model received versus what the
client sees. With the guardrail active, the two differ — the model gets
fictional names, the client gets real ones. The service also emits one
structured audit log line per call (entity counts only, never the names
themselves).
