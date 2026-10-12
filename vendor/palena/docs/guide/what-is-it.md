# What is it?

**Palena Pseudonymizer** is an HTTP guardrail for the
[LiteLLM proxy](https://docs.litellm.ai/). It sits between your clients and the
LLM and makes sure the model never sees real identities:

- On the way **in**, it detects entity names (people, organizations) and
  replaces each with a consistent fictional pseudonym.
- On the way **out**, it reverses the substitution so your users see the real
  names again.

The upstream LLM provider only ever receives fictional names. Your users never
notice the guardrail is there.

## Why pseudonymize instead of redact?

Redaction (`[PERSON]`, `████`) destroys the text's readability and the model's
ability to reason. Pseudonymization keeps the prose natural — the model happily
drafts an email for "Jordan Avery" and you get one addressed to "Alice Johnson".

## What it protects

| Content | Behaviour |
| --- | --- |
| **Chat text** | Names detected and swapped pre-call, restored post-call. |
| **Streaming responses** | Restored incrementally as tokens arrive, without emitting half a pseudonym. |
| **Images** | PII detected and redacted before the image reaches the model. |

## How it plugs in

The service speaks LiteLLM's
[`generic_guardrail_api`](https://docs.litellm.ai/docs/proxy/guardrails/) HTTP
contract, so wiring it up is a few lines of proxy config — no custom code, no
proxy image rebuild. See [Wire into LiteLLM](/guide/litellm-integration).

## What it is not

- It is **not** a data-loss-prevention scanner — it masks names, it does not
  block prompts.
- It does **not** run ML models itself. Detection is delegated to
  [Presidio](https://microsoft.github.io/presidio/) over HTTP.
- It does **not** persist real names anywhere except a short-lived,
  session-scoped Redis mapping (default TTL 1 hour).
