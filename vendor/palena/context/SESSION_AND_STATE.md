# SESSION_AND_STATE.md — session id + Redis storage

Session state is what makes the service actually work: pseudonyms must
be stable across the pre-call and post-call phases of a single request,
AND across the many turns of a multi-turn conversation. The **only**
thing tying those calls together is the session id we pick.

## Why a session id at all

- **Within a single LLM call** — pre-call substitutes `Novartis` →
  `Acme Corp` and stores the mapping. Post-call needs to reverse
  `Acme Corp` → `Novartis` in the LLM's response. Both hooks receive
  the same LiteLLM request, so LiteLLM's `litellm_call_id` is trivially
  stable across the pre/post pair — this is already enough for the
  single-request case.

- **Across turns of a conversation** — turn 2 sees `Novartis` again in
  the previously-pseudonymized user message plus fresh new names in the
  new message. To keep `Novartis → Acme Corp` stable so the LLM doesn't
  see two different pseudonyms for the same client, we must use a
  session id that is **the same across every turn of the conversation**.
  `litellm_call_id` changes per request, so it doesn't cut it — we need
  a client-supplied identifier.

## Derivation chain

Implement in `internal/session/derive.go`. Priority, highest-first:

1. **`additional_provider_specific_params.session_id_metadata_key`**
   points at a field to read. If the operator sets
   `session_id_metadata_key: "chat_session_id"`, we look for
   `structured_messages[*].metadata.chat_session_id` (walk from newest
   to oldest, first non-empty string wins). This is the intended
   production path — the operator picks the field name that matches how
   their client passes conversation ids.

2. **`request_data.user_api_key_end_user_id`** — the OpenAI `user`
   field surfaces here after LiteLLM's metadata plumbing. Stable per
   end-user, so it works when there's exactly one active conversation
   per user.

3. **`litellm_trace_id`** — when the client passes W3C `traceparent`,
   LiteLLM propagates the trace id. Reasonable heuristic for a
   conversation identifier when the client uses distributed tracing.

4. **`litellm_call_id`** — always present, but changes per request.
   Fallback that guarantees pre-call/post-call of the same LLM call
   stay stitched together. **Multi-turn will not work** with this
   fallback — each turn gets its own session.

5. **Anonymous synthetic** — if none of the above are present (very
   unusual for real LiteLLM requests), generate `anon-{uuid}`. Log
   `WARN` with the reason so operators can diagnose bad wiring. Do NOT
   fall back to a global default that would collide sessions.

Emit a metric per derivation source: `session_id_source{source=…}`.
Operators can alert if `litellm_call_id` or `synthetic` is the dominant
source — that's a wiring mistake, not intended behavior.

**No PII in the session id.** If step 1's resolved value looks like an
email or a phone number, hash it (see [FAILURE_MODES.md](FAILURE_MODES.md#no-pii-invariant))
before use — session ids leak into Redis keys and logs.

## Redis schema

Every session gets one Redis hash keyed by:

```
{PALENA_PSEUDONYMIZER_REDIS_KEY_PREFIX}:{session_id}
```

Default prefix: `palena:pseudonymizer`.

The hash has two fields:

| Field | Value | Notes |
| --- | --- | --- |
| `mapping` | JSON `{"real": "pseudo", …}` | Real names are keys; pseudonyms are values. Empty when no mapping yet. |
| `processed_indices` | JSON `[0, 2, 3]` | Message indices already pseudonymized in a prior turn. Present but often unused in the HTTP model — see "Processed indices" below. |

TTL: `PALENA_PSEUDONYMIZER_REDIS_SESSION_TTL` (default 3600s). Refreshed
on every read AND every write — active conversations keep the state
alive; abandoned sessions expire.

## Operations

### `GetMapping(session_id) (map[string]string, error)`

```
HGET {prefix}:{session_id} mapping
EXPIRE {prefix}:{session_id} {ttl}
```

Returns empty map when the hash doesn't exist. JSON decode errors are
logged and treated as "no mapping" (start fresh) — corrupt Redis payload
should never crash the service.

### `AddMappings(session_id, new_mappings) (merged map[string]string, error)`

Read-modify-write with existing entries winning:

```
current  = GetMapping(session_id)
merged   = current
for k, v in new_mappings:
    if k not in merged: merged[k] = v   # existing wins!
if merged == current:
    return current   # no-op, skip write
HSET {prefix}:{session_id} mapping {JSON(merged)}
EXPIRE {prefix}:{session_id} {ttl}
return merged
```

Existing-wins is critical for multi-turn stability: if turn 1 assigned
`Novartis → Acme Corp` and turn 2 re-processes the same name (due to a
retry or a race), we do not overwrite with a different pool entry.

**Race note.** The read-modify-write above is not atomic. Two
concurrent pre-calls with the same session id can each try to assign a
different pseudonym to the same real name. Solutions in preference
order:

1. Use a Redis Lua script (`EVAL`) to do the merge atomically. Ship
   this in M1 — the script is 10 lines and handles all races.
2. Alternatively, use `WATCH`/`MULTI`/`EXEC` for optimistic locking with
   retry.

Do NOT rely on "conversations are serial" — the client can send
concurrent turns for latency reasons or via chatbots that fire tool
loops.

### `GetReverseMapping(session_id) (map[string]string, error)`

```
mapping = GetMapping(session_id)
return { v: k for k, v in mapping }
```

The reverse map is used by the post-call phase to replace pseudonyms
with real names. It's derived on the fly rather than stored — the
storage cost of a second field isn't worth the sync overhead, and
sessions are small.

### `GetProcessedIndices(session_id) (map[int]struct{}, error)`

Reads the JSON array, returns as a set. Empty set when absent or
undecodable.

### `MarkProcessed(session_id, indices) error`

Union with existing, sort for determinism, write back.

## Processed indices — HTTP model quirk

In the Python service, `processed_indices` prevents re-pseudonymizing
already-processed messages when the client sends the full conversation
history each turn. In the HTTP model **this bookkeeping is largely
redundant** because:

- Pseudonymization is idempotent (see
  [PSEUDONYMIZATION_ALGORITHM.md](PSEUDONYMIZATION_ALGORITHM.md#multi-turn-de-duplication)):
  applying the mapping to text that's already substituted is a no-op.
- LiteLLM sends us `texts[]` post-assembly; we don't have an obvious
  "message index" to key on.

**Recommendation for v1:** implement `processed_indices` in
`internal/mapping` for parity with the Python schema (same Redis
key/field), but do NOT read/write it from the HTTP handlers. Leave it
as a hook we can wire in later if we discover a real need. This lets
us share Redis state with the Python service if we ever run both in
parallel during a migration.

## Session id and Redis key hygiene

- Session ids are used verbatim in Redis keys. **Reject** ids
  containing `\n`, `\r`, `\0`, or non-printable bytes — those could be
  used to smuggle a colon and hijack another session's key. Regex:
  `^[\x20-\x7E]{1,256}$`. Longer than 256 → hash to 64 hex chars.
- Log the derived session id at DEBUG only, never at INFO or above.
- Never include a session id in an error message returned to the LLM
  caller — those propagate to end users.

## Startup + shutdown

- On startup, connect lazily (first Redis call from a request).
  Readiness probe (`GET /readyz`) triggers the first connect.
- On `SIGTERM`, drain in-flight requests first, then close the Redis
  client. Set a shutdown deadline of 30s.
- The Redis client should be a single `*redis.Client` (or
  `*redis.ClusterClient` if `PALENA_PSEUDONYMIZER_REDIS_CLUSTER=true`)
  shared across the service — do not open a new client per request.

## Cross-service coexistence

Nothing in this doc prevents the Python `palena-guardrail-pseudonymizer`
and this Go service from sharing the same Redis:

- The key prefix is the same by default.
- The hash schema (`mapping` and `processed_indices` fields, JSON
  encoding) is identical.

That gives us a clean migration path: shift a fraction of traffic to
the Go service, watch the metrics, roll back by pointing LiteLLM at the
Python `CustomGuardrail` again. Sessions persist across the switch.
