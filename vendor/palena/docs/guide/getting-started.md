# Getting started

This walks through running the guardrail locally with Docker and pointing a
LiteLLM proxy at it.

## Prerequisites

- Docker + Docker Compose
- A running [LiteLLM proxy](https://docs.litellm.ai/docs/proxy/quick_start)
  (v1.65+ for streaming transformation support)

## 1. Start the stack

The repository ships a Compose file with Redis and both Presidio services
(pre-configured for organization detection):

```bash
docker compose up -d          # redis + presidio-analyzer + presidio-image-redactor
make run                      # builds and starts the guardrail on :8080
```

Check it is healthy:

```bash
curl localhost:8080/healthz   # -> ok
curl localhost:8080/readyz    # -> {"ready":true,"deps":{...}}
```

## 2. Point LiteLLM at it

Add the guardrail to your `proxy_server_config.yaml`:

```yaml
guardrails:
  - guardrail_name: palena-pseudonymizer
    litellm_params:
      guardrail: generic_guardrail_api
      api_base: http://localhost:8080
      mode: [pre_call, post_call]
      default_on: true
      unreachable_fallback: fail_closed
      streaming_transform_mode: incremental_diff
      additional_provider_specific_params:
        streaming_transform_mode: incremental_diff
```

Restart the proxy and send a request:

```bash
curl http://localhost:4000/chat/completions \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user",
       "content":"Draft an email for Alice Johnson at Novartis."}],
       "user":"customer-42"}'
```

The model receives fictional names; your response shows the real ones. Watch it
happen:

```bash
docker exec <redis> redis-cli HGETALL palena:pseudonymizer:customer-42
```

## 3. Next steps

- [Configuration](/guide/configuration) — tune entities, pools, and behaviour.
- [Organization detection](/guide/organization-detection) — add your company
  names for high-precision masking.
- [Deploy with Helm](/guide/deployment) — take it to Kubernetes.

::: tip Session id
Pseudonyms stay consistent across a conversation only if each turn carries a
stable session id. The service derives one from `metadata.session_id`, the
OpenAI `user` field, the trace id, or the call id — in that order. Pass a
stable `user` or `metadata.session_id` per conversation.
:::
