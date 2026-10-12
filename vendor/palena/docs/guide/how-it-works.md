# How it works

LiteLLM calls the guardrail twice per request — once before the model
(`input_type: request`) and once after (`input_type: response`) — at the same
HTTP endpoint.

## Pre-call (request)

```text
Client:  "Please book Alice Johnson a trip."
                      │
                      ▼  detect + substitute
LLM sees: "Please book Jordan Avery a trip."
```

1. **Detect** — the text is sent to Presidio Analyzer, which returns the spans
   it recognises as `PERSON` / `ORGANIZATION` (or any other
   [enabled entity type](/guide/configuration#entities)).
2. **Assign** — each new value gets a stand-in. Nominal identities (person /
   organization / location) draw a fictional name from a configured pool;
   structured PII (credit cards, SSNs, emails, IDs) gets a reversible token like
   `<CREDIT_CARD_1>` — see [Substitution strategy](/guide/configuration#substitution-strategy).
   Existing assignments are reused, so the same real value always maps to the
   same stand-in within a session.
3. **Persist** — the `real → pseudonym` mapping is stored in Redis, scoped to
   the conversation's session id.
4. **Rewrite** — the text is rewritten and forwarded to the model.

If detection or Redis is unreachable here, the request is **blocked** — the
model never receives un-masked text.

## Post-call (response)

```text
LLM says: "Jordan Avery is booked."
                      │
                      ▼  reverse
Client:   "Alice Johnson is booked."
```

The session's mapping is loaded and inverted, and every pseudonym in the
response is rewritten back to the real name. If reversal fails here, the
response is passed through unchanged (a stray pseudonym is annoying but never a
data leak).

## Streaming

Streamed responses are reversed incrementally. The tricky part is not emitting
half a pseudonym — if the stream so far ends with `"Jordan Av"`, the guardrail
tells LiteLLM to hold those trailing characters back until the rest arrives.
The result is a fluid stream that shows real names, token by token.

## Consistency guarantees

- **Within a request** — pre-call and post-call share the same session, so a
  name masked going in is always restored coming out.
- **Across turns** — because the mapping persists in Redis, turn 5 uses the
  same pseudonyms as turn 1.
- **First/last names** — mapping "Alice Johnson → Jordan Avery" also records
  "Alice → Jordan" and "Johnson → Avery", so a model that shortens to just
  "Jordan" still reverses correctly.

See [Organization detection](/guide/organization-detection) for how company
names are handled, and the [HTTP protocol](/reference/http-protocol) reference
for the exact wire contract.
