# CONFIG.md — configuration reference

All settings are environment variables, prefixed `PALENA_PSEUDONYMIZER_`.
The Go binary reads them at startup via `envconfig` and validates the
whole thing before serving any requests. Startup fails hard on invalid
config — never boot into a half-configured state.

## Runtime

| Env var | Default | Notes |
| --- | --- | --- |
| `PALENA_PSEUDONYMIZER_HTTP_ADDR` | `:8080` | Bind address. `:8080` binds all interfaces. |
| `PALENA_PSEUDONYMIZER_HTTP_READ_TIMEOUT` | `30s` | Applied to `http.Server.ReadTimeout`. |
| `PALENA_PSEUDONYMIZER_HTTP_WRITE_TIMEOUT` | `30s` | Applied to `http.Server.WriteTimeout`. |
| `PALENA_PSEUDONYMIZER_HTTP_IDLE_TIMEOUT` | `120s` | `http.Server.IdleTimeout`. |
| `PALENA_PSEUDONYMIZER_HTTP_MAX_HEADER_BYTES` | `1048576` (1 MiB) | Blocks header-smuggling attempts. |
| `PALENA_PSEUDONYMIZER_MAX_REQUEST_BYTES` | `33554432` (32 MiB) | See [PROTOCOL.md](PROTOCOL.md#request-schema). |
| `PALENA_PSEUDONYMIZER_SHUTDOWN_TIMEOUT` | `30s` | Grace period for in-flight requests on SIGTERM. |
| `PALENA_PSEUDONYMIZER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `PALENA_PSEUDONYMIZER_LOG_FORMAT` | `json` | `json` or `text`. |
| `PALENA_PSEUDONYMIZER_API_KEY` | (empty = dev mode) | Value clients must send in `x-api-key`. Empty ⇒ accept anyone (log WARN). |

## Presidio

| Env var | Default | Notes |
| --- | --- | --- |
| `PALENA_PSEUDONYMIZER_PRESIDIO_ANALYZER_URL` | `http://presidio-analyzer:5001` | HTTP-only, no auth. |
| `PALENA_PSEUDONYMIZER_PRESIDIO_IMAGE_REDACTOR_URL` | `http://presidio-image-redactor:5003` | HTTP-only, no auth. |
| `PALENA_PSEUDONYMIZER_PRESIDIO_TIMEOUT_SECONDS` | `10` | Per-request timeout, applies to both services. |
| `PALENA_PSEUDONYMIZER_PRESIDIO_READINESS_TIMEOUT_SECONDS` | `2` | `/readyz` probe timeout. Shorter than request timeout on purpose. |
| `PALENA_PSEUDONYMIZER_PRESIDIO_SCORE_THRESHOLD` | `0.7` | Minimum confidence to treat as PII. `[0, 1]`. |
| `PALENA_PSEUDONYMIZER_PRESIDIO_ENTITIES` | `PERSON,ORGANIZATION` | Comma-separated. **Any entity type Presidio supports** — add `CREDIT_CARD`, `US_SSN`, `IBAN_CODE`, `EMAIL_ADDRESS`, `PHONE_NUMBER`, custom IDs, etc. **`LOCATION` is intentionally excluded** — see note below. |
| `PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGE` | `en` | Forwarded verbatim to Presidio's `language` field. |
| `PALENA_PSEUDONYMIZER_ENTITY_STRATEGY` | *(empty)* | Per-type substitution overrides: `CREDIT_CARD:token,PHONE_NUMBER:pool`. See "Substitution strategy". |
| `PALENA_PSEUDONYMIZER_ENTITY_STRATEGY_DEFAULT` | `token` | Strategy for enabled types that are neither nominal (PERSON/ORG/LOCATION) nor explicitly overridden. `pool` \| `token`. |

> **Why LOCATION is off by default.** Substituting a city (`Paris` → `Riverside`)
> replaces one real place with a *different* real-world-plausible place, and the
> model then reasons from the wrong geography — asking "which Riverside do you
> mean?", or giving weather/currency/timezone answers for the wrong location.
> Unlike a person's name (pure identity), a place name carries semantics the
> model needs intact. Add `LOCATION` back to `PRESIDIO_ENTITIES` only if your
> threat model requires masking geography and you accept that drift.

### Substitution strategy

Different PII needs different substitution. There are two strategies:

| Strategy | Output | Right for |
| --- | --- | --- |
| `pool` | a realistic fictional value from a pool (`Alice Johnson` → `Jordan Avery`) | **nominal identities** the model reasons about: PERSON, ORGANIZATION, LOCATION |
| `token` | a consistent reversible placeholder (`4111…` → `<CREDIT_CARD_1>`) | **structured PII**: CREDIT_CARD, US_SSN, IBAN_CODE, EMAIL_ADDRESS, phone numbers, insurance/ID numbers, custom types |

A fake *name* for a credit card is meaningless, and the model rarely needs the
actual digits — a stable token (`<CREDIT_CARD_1>`) both hides the value and
signals to the model that it was redacted, and reverses back to the exact
original in the response.

**Resolution order** for an entity type: an explicit `ENTITY_STRATEGY` override →
`pool` for PERSON/ORGANIZATION/LOCATION → `ENTITY_STRATEGY_DEFAULT` (`token`).
So simply adding `CREDIT_CARD` to `PRESIDIO_ENTITIES` tokenizes it automatically;
no strategy config needed for the common case.

Tokens are `<TYPE_N>` where `TYPE` is the (normalized, uppercased) entity type and
`N` is a per-type, per-session counter, so the same value maps to the same token
across a conversation and reverses exactly (case and all).

> **Detecting structured / country-specific PII.** Presidio detects many types
> out of the box (CREDIT_CARD, US_SSN, IBAN_CODE, EMAIL_ADDRESS, PHONE_NUMBER,
> US_PASSPORT, UK_NHS, MEDICAL_LICENSE, …). Country-specific IDs (national
> identity cards, insurance numbers) are usually **not** built in — add a custom
> recognizer (regex/deny-list) via the Presidio recognizer registry, exactly like
> the organization deny-list in `deploy/presidio/recognizers.yaml`. Also mind
> detection noise: enabling the generic spaCy NER can tag common words as
> ORGANIZATION; scope it off if you only want structured detection.

## Redis

| Env var | Default | Notes |
| --- | --- | --- |
| `PALENA_PSEUDONYMIZER_REDIS_URL` | `redis://redis:6379/0` | `go-redis/v9` URL. Includes DB number. |
| `PALENA_PSEUDONYMIZER_REDIS_SESSION_TTL_SECONDS` | `3600` | Refreshed on every read + write. |
| `PALENA_PSEUDONYMIZER_REDIS_KEY_PREFIX` | `palena:pseudonymizer` | Change only if you're sharing Redis with something that might collide. |
| `PALENA_PSEUDONYMIZER_REDIS_TIMEOUT_SECONDS` | `2` | Applies to each Redis command. |
| `PALENA_PSEUDONYMIZER_REDIS_POOL_SIZE` | `10` | Per-server connection pool. |
| `PALENA_PSEUDONYMIZER_REDIS_CLUSTER` | `false` | Set true to use `*redis.ClusterClient` instead of `*redis.Client`. |

## Non-text content (images)

| Env var | Default | Notes |
| --- | --- | --- |
| `PALENA_PSEUDONYMIZER_NON_TEXT_PII_ACTION` | `redact` | `redact` \| `block` \| `passthrough`. |
| `PALENA_PSEUDONYMIZER_NON_TEXT_BLOCK_MESSAGE` | (see [IMAGE_PIPELINE.md](IMAGE_PIPELINE.md#config-knobs-per-image-handler)) | `{entity_types}` placeholder. |
| `PALENA_PSEUDONYMIZER_MAX_IMAGE_BYTES` | `20971520` (20 MiB) | Per-image guard against OOMing Presidio. |
| `PALENA_PSEUDONYMIZER_MAX_IMAGES_PER_REQUEST` | `20` | Request-level limit. |

## Session

| Env var | Default | Notes |
| --- | --- | --- |
| `PALENA_PSEUDONYMIZER_SESSION_METADATA_KEY` | `session_id` | Default field name to look for in `structured_messages[*].metadata`. Overridable per-guardrail via `additional_provider_specific_params.session_id_metadata_key`. |

## Text handling

| Env var | Default | Notes |
| --- | --- | --- |
| `PALENA_PSEUDONYMIZER_DECOMPOSE_PERSON_NAMES` | `true` | Register first/last-name sub-mappings for multi-token PERSON names — see note below. |
| `PALENA_PSEUDONYMIZER_ALLOW_LIST` | *(empty)* | Comma-separated terms never pseudonymized even when detected (brand/product names, public figures, words Presidio over-tags). Case-insensitive; also excluded from name decomposition. Symmetric to the org deny-list. |
| `PALENA_PSEUDONYMIZER_DETERMINISTIC_SECRET` | *(empty)* | When set, POOL assignment uses `HMAC-SHA256(secret, name)` over the pool so the same real value maps to the same pseudonym across sessions (stable/portable pseudonyms; probes forward on within-session collision). Reversal still uses the store. Token entities unaffected. Secret-grade. |

**Collision hardening.** A freshly-minted pseudonym is never allowed to be a
substring of the text it is inserted into (checked case-insensitively). This
stops a user- or model-supplied lookalike — e.g. someone literally typing
`<CREDIT_CARD_1>`, or a pool name that already appears in the text — from
aliasing a real value and making the reverse pass ambiguous. Token indices and
pool candidates are skipped until a non-colliding one is found.

> **Why person-name decomposition.** We map a full name as a unit
> (`Alice Johnson` → `Jordan Avery`), but a model — and later user turns —
> routinely reference just the first or last name. Without decomposition, a
> model that shortens `Jordan Avery` to `Jordan` leaves a raw pseudonym in the
> reply (no reversal), and a later user message saying just `Alice` gets a
> *fresh, conflicting* pseudonym so the model thinks two different people are
> involved. With decomposition on, assigning `Alice Johnson → Jordan Avery`
> also stores `Alice → Jordan` and `Johnson → Avery` (aligned by position), so
> both directions stay consistent. Longest-first matching still rewrites the
> full name before the components.
>
> **Caveat:** a real surname that is also a common noun (`Baker`, `Green`,
> `Cook`) will over-match that noun (case-insensitively) — producing garbled
> but never data-leaking text (a pseudonym or a public word is rewritten, never
> a hidden identity). Single initials (`J.`) are excluded. Set this to `false`
> to disable decomposition if your inputs contain many common-noun surnames.

## Pools

Pseudonym pools are configured via env vars — each entity type gets a
comma-separated list. Whitespace around commas is trimmed. Blank
entries are ignored.

| Env var | Default |
| --- | --- |
| `PALENA_PSEUDONYMIZER_POOL_PERSON` | `Jordan Avery,Taylor Morgan,Alex Rivera,Riley Quinn,Casey Bennett,Sam Ellis,Jamie Brooks,Cameron Hayes` |
| `PALENA_PSEUDONYMIZER_POOL_ORGANIZATION` | `Acme Corp,Birch Industries,Cedar Systems,Delta Group,Elm Partners,Fern Solutions,Grove Holdings,Hazel Dynamics` |
| `PALENA_PSEUDONYMIZER_POOL_LOCATION` | `Riverside,Lakewood,Hillcrest,Meadowbrook,Stonefield,Clearwater` (unused unless `LOCATION` is added to `PRESIDIO_ENTITIES`) |

> **Why the PERSON pool is gender-neutral.** A pseudonym carries an *implied
> gender* the model reasons from: map a female real name to "Thomas" and the
> model writes "he", leaking a wrong attribute. Gender-*matching* would require
> a gender lookup for both the real name and every pool entry — a hardcoded
> gender list we deliberately avoid. Instead the default pool uses unisex given
> names (Jordan, Taylor, Alex, Riley, …) so the model has no strong prior: it
> says "they" or asks, and the wrong gender never leaks. This is pure pool
> curation — **no gender logic exists in the code**. Operators who want gendered
> pseudonyms can override `POOL_PERSON`, accepting the drift risk.

**Pool custom entity types** are added via
`PALENA_PSEUDONYMIZER_POOL_<TYPE>=…` — the config loader parses any env
var matching this prefix and adds it to the pool map. See
[PSEUDONYMIZATION_ALGORITHM.md](PSEUDONYMIZATION_ALGORITHM.md#pool-assignment)
for the synthetic-fallback rules when the pool is empty or exhausted.

## Streaming

Streaming behaviour is set on the LiteLLM side (`streaming_transform_mode`,
`streaming_sampling_rate`, `streaming_end_of_stream_only`) — the service
does not have a global toggle. See
[PROTOCOL.md](PROTOCOL.md#streaming).

## Request-time overrides

LiteLLM lets clients override guardrail params per request via:

```json
"guardrails": [
  {"pseudonymizer": {"extra_body": {"session_id_metadata_key": "chat_id"}}}
]
```

These end up in `additional_provider_specific_params`. Reserved keys:

| Key | Type | Use |
| --- | --- | --- |
| `session_id_metadata_key` | string | Overrides the env default for THIS request. |
| `streaming_transform_mode` | string | Whether to enable transform-mode streaming for this request. |
| `score_threshold` | float | Per-request Presidio threshold. Overrides env default. |
| `entities` | list[string] | Per-request entity types. Overrides env default. |
| `pii_action` | string | Per-request non-text PII action. Overrides env default. |

Unknown keys are logged at DEBUG and ignored — never error on them. The
client may set anything they like via `extra_body`, and we're not the
police for a schema they didn't publish.

## Validation rules

Startup validation runs before the HTTP server binds. If any of these
fail, log ERROR with the offending field and exit non-zero:

- `PRESIDIO_ANALYZER_URL`, `PRESIDIO_IMAGE_REDACTOR_URL`, `REDIS_URL`
  must parse as URLs.
- `PRESIDIO_SCORE_THRESHOLD` ∈ [0.0, 1.0].
- `PRESIDIO_ENTITIES` non-empty.
- `NON_TEXT_PII_ACTION` ∈ {redact, block, passthrough}.
- `LOG_LEVEL` ∈ {debug, info, warn, error}.
- `SESSION_TTL_SECONDS > 0`.
- All timeout values > 0.
- All byte / count limits > 0.
- `REDIS_POOL_SIZE >= 1`.
- If `NON_TEXT_BLOCK_MESSAGE` uses `{entity_types}`, verify the string
  contains exactly one occurrence.

Log the parsed config at INFO on successful startup — WITHOUT the
`API_KEY` value. The API key must be redacted or omitted.
