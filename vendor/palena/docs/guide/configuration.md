# Configuration

All settings are environment variables prefixed `PALENA_PSEUDONYMIZER_`. The
service validates them on startup and refuses to boot on invalid config. The
full list is in the [Environment variables](/reference/environment) reference;
this page covers the choices that matter most.

## Entities

```bash
PALENA_PSEUDONYMIZER_PRESIDIO_ENTITIES=PERSON,ORGANIZATION   # default
```

`LOCATION` is **off by default**. Masking a city (`Paris` → `Riverside`)
replaces one real place with another real-sounding place, and the model then
reasons from the wrong geography — wrong weather, wrong currency, "which
Riverside do you mean?". Person and organization names are pure identity and
safe to swap; place names carry semantics the model needs. Add `LOCATION` back
only if your threat model requires geography masking.

You can enable **any entity type Presidio detects** — add structured PII such
as credit cards, national IDs, emails or phone numbers:

```bash
PALENA_PSEUDONYMIZER_PRESIDIO_ENTITIES=PERSON,ORGANIZATION,CREDIT_CARD,US_SSN,EMAIL_ADDRESS
```

How each type is substituted depends on its [strategy](#substitution-strategy).

## Substitution strategy

Not all PII should be masked the same way. A fake *name* for a credit card is
meaningless, and the model rarely needs the real digits — it just needs to know
a value was there. So there are two strategies:

| Strategy | Output | Right for |
| --- | --- | --- |
| `pool` | a realistic fictional value (`Alice Johnson` → `Jordan Avery`) | **nominal identities** the model reasons about: `PERSON`, `ORGANIZATION`, `LOCATION` |
| `token` | a consistent reversible placeholder (`4111…` → `<CREDIT_CARD_1>`) | **structured PII**: `CREDIT_CARD`, `US_SSN`, `IBAN_CODE`, `EMAIL_ADDRESS`, phone numbers, insurance / ID numbers, custom types |

```bash
# Default strategy for enabled types that aren't nominal identities:
PALENA_PSEUDONYMIZER_ENTITY_STRATEGY_DEFAULT=token          # token | pool

# Per-type overrides (comma-separated TYPE:strategy):
PALENA_PSEUDONYMIZER_ENTITY_STRATEGY=PHONE_NUMBER:pool,CREDIT_CARD:token
```

**Resolution order** for a given entity type:

1. an explicit `ENTITY_STRATEGY` override, else
2. `pool` if the type is `PERSON` / `ORGANIZATION` / `LOCATION`, else
3. `ENTITY_STRATEGY_DEFAULT` (`token`).

So simply adding `CREDIT_CARD` to `PRESIDIO_ENTITIES` tokenizes it
automatically — no strategy config needed for the common case. Tokens are
`<TYPE_N>` where `N` is a per-type, per-session counter, so the same value maps
to the same token across a conversation and reverses back to the exact original
(case and all).

::: tip Detecting structured / country-specific PII
Presidio recognises many types out of the box (`CREDIT_CARD`, `US_SSN`,
`IBAN_CODE`, `EMAIL_ADDRESS`, `PHONE_NUMBER`, `US_PASSPORT`, `UK_NHS`, …).
Country-specific IDs (national identity cards, insurance numbers) usually need a
**custom recognizer** — add a regex or deny-list recognizer the same way you add
the [organization deny-list](/guide/organization-detection).
:::

## Languages

Presidio's name detection is per language: German text analyzed as English
misses most German names and tags ordinary words ("Bitte") as organizations. List
the languages your users write in:

```bash
PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGES=en,de,fr,it
```

Each message is classified among the listed languages (offline, with
[lingua-go](https://github.com/pemistahl/lingua-go)) and analyzed in its own
language; text too short or ambiguous to classify uses the first one. With one
language nothing is classified.

Presidio needs an NLP model for every listed language. The stock
`presidio-analyzer` image ships English only; the chart switches to
`ghcr.io/palenaai/palena-presidio-analyzer` (Presidio plus the German, French and
Italian models) as soon as `config.languages` holds anything but English, and
renders the analyzer's NLP, recognizer and language config to match. Each extra
language needs roughly 800Mi more analyzer memory.

## Score thresholds per entity type

`PRESIDIO_SCORE_THRESHOLD` (0.7) applies to every type unless
`ENTITY_SCORE_THRESHOLD` overrides it:

```bash
PALENA_PSEUDONYMIZER_ENTITY_SCORE_THRESHOLD=PHONE_NUMBER:0.4,EMAIL_ADDRESS:0.9
```

Presidio's phone recognizer scores every match 0.4, which is why the default
lowers `PHONE_NUMBER` only: under 0.7 no phone number would ever be masked.

## Allow-list

Terms that should **never** be pseudonymized, even when Presidio detects them:

```bash
PALENA_PSEUDONYMIZER_ALLOW_LIST=Palena,Acme Rockets,Ada Lovelace
```

Matched case-insensitively against the detected span. Use it for:

- **your own brand / product names** — you don't want "Palena" swapped for a
  fake org in every prompt;
- **public figures or well-known entities** that carry no client PII;
- **words Presidio over-tags** — e.g. the generic NER tagging "SSN" as an
  `ORGANIZATION` (a false positive you saw in testing).

It's the symmetric counterpart to the deny-lists below: a deny-list *adds*
detections, the allow-list *suppresses* them. Allow-listed terms are also excluded from
first/last-name decomposition, so an allow-listed surname never leaks a
sub-mapping.

## Deny-lists

Terms that should **always** be pseudonymized, as a given entity type, whether
or not Presidio's NER would find them — client names, project code names,
internal people. Set them in the chart (they become exact-match Presidio
recognizers in every configured language):

```yaml
presidio:
  analyzer:
    denyLists:
      - entity: ORGANIZATION
        terms: [Contoso Ltd, Fabrikam]
      - entity: PERSON
        terms: [Project Falcon]
```

The entity type must also be in `config.entities`. The older
`presidio.analyzer.organizations` list still works and is the same as an
`ORGANIZATION` entry.

## Deterministic pseudonyms

By default a real name gets the next free pool pseudonym per session, so the
same person may map to different pseudonyms in different conversations. Set a
secret to make pool assignment **deterministic** — the same real value maps to
the same pool pseudonym across every session:

```bash
PALENA_PSEUDONYMIZER_DETERMINISTIC_SECRET=<random-secret>
```

The pseudonym is chosen by a keyed `HMAC-SHA256(secret, name)` over the pool,
probing forward on a within-session collision. This gives **stable, portable
pseudonyms** — useful when you correlate pseudonymized data across sessions or
want reproducibility after a Redis flush. Trade-offs:

- Reversal still uses the session store (HMAC is one-way) — this changes
  *which* pseudonym is picked, not whether state is stored.
- Cross-session determinism means a fixed name↔pseudonym relationship, i.e.
  less per-session unlinkability. Only enable it if that's what you want.
- Applies to the **pool** strategy only; token entities keep their per-session
  counter. Treat the secret like any other credential.

## Pseudonym pools

Each entity type draws pseudonyms from a pool:

```bash
PALENA_PSEUDONYMIZER_POOL_PERSON=Jordan Avery,Taylor Morgan,Alex Rivera,...
PALENA_PSEUDONYMIZER_POOL_ORGANIZATION=Acme Corp,Birch Industries,...
```

The default PERSON pool is **gender-neutral** on purpose. A pseudonym carries
an implied gender the model reasons from — map a female name to "Thomas" and
the model writes "he", leaking a wrong attribute. Unisex names (Jordan, Taylor,
Alex, Riley…) give the model no strong prior, so it says "they" or asks. This
is pure pool curation; there is no gender logic in the service.

## Person-name decomposition

```bash
PALENA_PSEUDONYMIZER_DECOMPOSE_PERSON_NAMES=true   # default
```

When on, mapping "Alice Johnson → Jordan Avery" also records "Alice → Jordan"
and "Johnson → Avery", so a model that refers to just the first name still
reverses correctly and later turns stay consistent. A real surname that is also
a common noun (e.g. "Baker") can over-match; set this to `false` if your inputs
contain many such names.

## Images

```bash
PALENA_PSEUDONYMIZER_NON_TEXT_PII_ACTION=redact   # redact | block | passthrough
```

- **redact** (default) — forward a redacted copy of the image.
- **block** — reject the request with a user-facing message.
- **passthrough** — forward the original untouched (not recommended).

## Failure behaviour

The guardrail is **fail-closed on the way to the model**: if Presidio or Redis
is unreachable during the pre-call phase, the request is blocked so real names
cannot leak. On the post-call phase it is best-effort — a reversal failure
never turns a successful LLM response into an error.

Operators control the final policy with LiteLLM's `unreachable_fallback`
(`fail_closed` recommended).

## Authentication

```bash
PALENA_PSEUDONYMIZER_API_KEY=<shared-secret>
```

When set, clients (i.e. the LiteLLM proxy) must send it as an `x-api-key`
header. When empty, the service accepts any request — fine for a private
in-cluster deployment, not for an exposed one.

## Session erasure

Mappings expire on their own via the Redis TTL, but you can erase one on demand
— e.g. to honour a GDPR right-to-erasure request or tear down a finished
conversation:

```bash
curl -X DELETE http://pseudonymizer:8080/sessions/<session_id> \
  -H "x-api-key: $PALENA_PSEUDONYMIZER_API_KEY"
```

Returns `200 {"deleted": true|false}` — `deleted` reports whether a mapping
existed (the call is idempotent). The endpoint is guarded by the same
`API_KEY` shared secret as the guardrail endpoint; leave the key unset only in
a trusted, in-cluster deployment.
