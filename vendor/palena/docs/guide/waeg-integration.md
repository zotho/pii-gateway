# Wire into Wäg

The service exposes a second endpoint, `POST /v1/waeg/check`, speaking the Wäg
gateway's HTTP guardrail protocol (`allow` / `mask` / `block`). It runs
alongside the LiteLLM endpoint on the
same deployment, sharing one Redis mapping store and one Presidio pair — the
two gateways can be wired to the same instance.

## Minimal config

```yaml
guardrails:
  items:
    - name: palena-pseudonymizer
      kind: http
      role: transform
      mode: [pre_call, during_call, post_call]
      url: http://pseudonymizer.guardrails.svc:8080/v1/waeg/check
      api_key_env: PALENA_PSEUDONYMIZER_API_KEY
      on_error: closed
      forward_identity: true
      forward_session_id: true
      timeout_ms: 2000
```

Three of these are load-bearing. Getting them wrong fails quietly.

### `role: transform` — required

Wäg only applies a returned rewrite to the pending stream window when the item
is a transform item. Configured as a policy item, the guardrail's masking is
treated as a redact decision and reverse-pseudonymization silently does not
happen mid-stream.

### `on_error: closed` — required

Wäg's default failure mode is **fail-open**. This service deliberately fails
closed: if Presidio or Redis is unreachable it answers `502` rather than let an
unpseudonymized prompt through. Wäg does not read that as a block — it treats a
non-action response as a host error and, under the default, forwards the
original prompt to the provider.

`on_error: closed` (or a global `fail_mode: closed`) is what makes the
service's fail-closed guarantee actually hold end to end.

### `api_key_env`

Wäg authenticates with `Authorization: Bearer <key>`. The endpoint accepts
either that or the `x-api-key` header used by the LiteLLM path; both compare
against the same `API_KEY` value. When `API_KEY` is unset the service runs open
(dev mode only).

## Phase mapping

| Wäg phase | Service behaviour |
| --- | --- |
| `pre_call` | Pseudonymize the prompt — real names out, pseudonyms in |
| `during_call` | Reverse the pending stream window, return `hold_chars` for an incomplete trailing name |
| `post_call` | Reverse the complete assistant text |

`during_call` reads `pending` (the uncommitted window) in preference to the
cumulative text, because committed SSE bytes can never be rewritten. Incomplete
names are held back rather than emitted half-substituted: streaming
`Thomas We…` when `Thomas Weber` is a known pseudonym holds those characters
until the next window resolves them.

Note that `hold_chars: 0` from this service means "nothing to hold", but Wäg
raises it to `stream_default_hold_chars` (default 48) regardless. That costs a
little latency, never correctness — the hold is flushed at `eof`.

## Session id and multi-turn consistency

Pseudonyms stay stable across turns only when Wäg forwards a session id. The
service resolves one in this order:

1. `identity.session_id` — needs `forward_identity` **and**
   `forward_session_id`; both default to `false` in Wäg, and the fingerprint is
   only known for Playground / console traffic
2. `identity.user_id` — needs `forward_identity`
3. `request_id` — always present, but scoped to a single request

The `request_id` fallback is enough to stitch one request's `pre_call` to its
`during_call` / `post_call` phases, so reversal always works. It is *not*
enough to give the same person the same pseudonym on the next turn.

For plain API traffic where no session fingerprint exists, set
[`DETERMINISTIC_SECRET`](/reference/environment) instead: pseudonyms are then
derived by keyed HMAC and stay stable across sessions without any session id at
all.

## Known limitation: multi-turn `pre_call`

When a guardrail returns a mask on `pre_call`, Wäg joins every message into one
string, applies the rewrite, and writes the result into the **last user
message** without clearing the earlier ones. A multi-turn conversation that
gets pseudonymized therefore reaches the provider with its history duplicated.

This is gateway-side behaviour affecting any masking guardrail in Wäg, not
something this service can correct from its side. Until it is addressed
upstream, prefer `mode: [during_call, post_call]` with single-turn requests, or
accept the duplication for short conversations.

## Verifying

```bash
curl -sS http://localhost:8080/v1/waeg/check \
  -H 'Authorization: Bearer '"$API_KEY" \
  -H 'content-type: application/json' \
  -d '{
        "guardrail": "palena-pseudonymizer",
        "phase": "pre_call",
        "request_id": "req-1",
        "text": "Email Thomas Weber at ACME about the Zurich office."
      }'
```

A response of `{"action":"mask","text":"Email <pseudonym> at …"}` confirms the
wiring. Then repeat with `"phase":"post_call"` and the same `request_id`,
passing the pseudonymized text as `output_text`, to see it reversed.
