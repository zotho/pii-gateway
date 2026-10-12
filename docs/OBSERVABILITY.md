# Observability and audit

## Principles

1. **The audit log never holds a real value.** Only handles (`<PERSON_3>`), entity types, counts,
   field paths and keyed fingerprints. It can be shipped to a SIEM and read by the security team
   without becoming a second PII store.
2. **Two stores, two trust levels.**
   | Store | Contents | Who reads |
   |---|---|---|
   | Mapping vault (Redis) | real value ↔ handle per session | the gateway; humans only via *reveal* |
   | Audit log (append-only JSONL → SIEM) | events below | security / compliance |
3. **Re-identification is a privileged, audited action.** Reading a session's mapping goes
   through `reveal`, which writes an audit event (actor, session, reason) *before* returning data.
   The demo UI's mapping panel uses this path.
4. **Fingerprints, not values.** Each new mapping logs `fp = HMAC-SHA256(audit_key, type|value)[:16]`.
   An investigator holding the key can answer "did value X ever go through the gateway, in which
   sessions?" by computing `fp(X)`, without the log storing X. Rotating or destroying the key ends
   that ability.
5. **Tamper evidence.** Every event carries `prev` and `hash = SHA256(prev || canonical_json(event))`.
   A deleted or edited line breaks the chain (`verify()`).
6. **Deletion = crypto-shredding.** `DELETE /sessions/{id}` drops the vault entry; audit events stay
   but can no longer be re-identified.

## Events

Common fields: `ts` (RFC3339 ms), `event`, `session`, `principal` (LiteLLM key alias / user id),
`call_id` (litellm_call_id), `prev`, `hash`.

| event | extra fields | written by |
|---|---|---|
| `request.masked` | `route`, `model`, `texts`, `pattern_only`, `entities` {type: n}, `new` [{handle, type, fp}], `palena_ms`, `leak_suspects` [{handle, path}] | bridge pre-call |
| `request.blocked` | `reason` (`palena_unreachable` \| `palena_blocked` \| `unsupported_route` \| `leak`), `route` | bridge pre-call |
| `response.restored` | `stream`, `restored` {type: n}, `kept_masked` {type: n} (no-restore, e.g. SECRET), `unknown_handles` n, `upstream_status`, `upstream_ms` | bridge post-call / end of stream |
| `mapping.revealed` | `actor`, `reason`, `handles` (list or `"*"`) | reveal API |
| `session.deleted` | `actor` | sessions API |

`leak_suspects`: after masking, the bridge scans the **outgoing** body for every real value already in
the session mapping (case-insensitive, **on word boundaries**, Unicode-aware: "Ivan" does not hit inside "Ivanov", e-mails and phones are whole tokens; the demo's leak badge uses the same rule). A hit means some field escaped the walker (e.g. tool
definitions). Default action: log + metric; `PII_BRIDGE_LEAK_ACTION=block` turns it into
`request.blocked{reason: leak}`.

## Metrics (Prometheus; Palena exports its own)

| metric | labels |
|---|---|
| `pii_requests_total` | `route`, `outcome` (forwarded / blocked) |
| `pii_entities_total` | `type` |
| `pii_new_mappings_total` | `type` |
| `pii_leak_suspects_total` | `route` |
| `pii_fail_closed_total` | `reason` |
| `pii_palena_seconds` | histogram |
| `pii_restore_total` | `type`, `result` (restored / kept_masked / unknown) |

Suggested alerts: any `pii_leak_suspects_total` increase; `pii_fail_closed_total` rate > 0 for 5 min;
`session_id_source="api_key_fallback"` share > 1% (clients not sending a session header; multi-turn
stability is broken for them).

## Correlation

`session` (agent session) → `call_id` (one LLM call) → provider request id (`request-id` /
`x-request-id` from the upstream response, logged in `response.restored`). The provider-side abuse /
support ticket can be traced back to a session and principal without anyone seeing the content.

## What is deliberately not logged

Prompt or completion bodies (LiteLLM `turn_off_message_logging: true` in production), real values,
raw API keys, OAuth tokens, `chatgpt-account-id`.

## Operations

| What | How |
|---|---|
| Audit log | `PII_AUDIT_LOG` (default `.run/audit/audit.jsonl`), fingerprint key `PII_AUDIT_KEY` (default: a key file generated at `.run/audit/fp.key`; set your own in production). Both LiteLLM instances append to the same file (flock-serialised). |
| Fail-closed audit | An audit write failure is logged and counted (`pii_audit_errors_total`) but does not break traffic; `PII_AUDIT_REQUIRED=1` makes the request fail with 503 instead. |
| Leak guard | `PII_BRIDGE_LEAK_ACTION=log` (default) or `block`. Scan cost is ~11 ms for a 200 KB body with 300 mapped values (~36 ms with 1000). Fields excluded from the scan: LiteLLM internals (`proxy_server_request`, `metadata`, ...). |
| Metrics | Prometheus text on `http://127.0.0.1:$PII_BRIDGE_METRICS_PORT/metrics` (mock stack 14090, real stack 14091; set in `scripts/_env.sh` / `scripts/real-up.sh`; unset = off). |
| Verify chain, counts | `python scripts/audit_verify.py` (exit 1 if the chain is broken) |
| One session | `python scripts/audit_verify.py --session SID` |
| "Did value X go through the gateway?" | `python scripts/audit_verify.py --fp EMAIL_ADDRESS 'a@b.com'` lists the sessions/handles whose `new[].fp` matches. Needs the audit key; the log itself never holds X. |

`response.restored` also carries `provider_request_id` (best effort; `null` when not available). Source by route:
Responses/chat non-stream and every stream: `_hidden_params['additional_headers']` of the response / of the stream
wrapper handed to the streaming hook (not of its items); Anthropic `/v1/messages` non-stream (plain dict response):
`litellm_logging_obj.model_call_details['httpx_response'].headers`. Header names tried: `x-request-id`, `request-id`,
`openai-request-id`, `x-oai-request-id` (chatgpt.com sends `x-oai-request-id`, plus `cf-ray`).
