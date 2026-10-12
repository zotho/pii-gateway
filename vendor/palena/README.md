<h1 align="center">Palena Pseudonymizer</h1>

<p align="center">
  Pseudonymization guardrail for the <a href="https://docs.litellm.ai/">LiteLLM proxy</a> —
  swaps real people and company names for consistent fictional ones before the
  LLM sees them, then restores the originals in the response.
</p>

---

The upstream model provider only ever receives fictional names ("Alice Johnson"
→ "Jordan Avery"); your users never notice. Works for chat text, streamed
responses, and image attachments, and stays consistent across every turn of a
conversation.

It speaks LiteLLM's
[`generic_guardrail_api`](https://docs.litellm.ai/docs/proxy/guardrails/) HTTP
contract, so wiring it in is a few lines of proxy config — no custom code, no
proxy image rebuild.

## Quick start

```bash
docker compose up -d          # redis + presidio (analyzer + image redactor)
make run                      # builds and starts the guardrail on :8080
```

Point your LiteLLM proxy at it:

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

Send a request with a name and the model receives a pseudonym while your client
sees the real name back. Full walkthrough in the
[getting-started guide](https://github.com/PalenaAI/palena-litellm-pseudonymizer).

## Deploy to Kubernetes

A Helm chart ships the service plus optional Redis and Presidio, with
organization detection on by default:

```bash
helm install pseudonymizer \
  ./deploy/helm/palena-litellm-pseudonymizer \
  --namespace guardrails --create-namespace
```

See [deploy/helm/palena-litellm-pseudonymizer](deploy/helm/palena-litellm-pseudonymizer/README.md).

## Documentation

Guides and reference live under [`docs/`](docs/) as Markdown.

It covers configuration, organization detection, Helm deployment, LiteLLM
integration, and the HTTP protocol.

## Development

```bash
make test          # go test -race ./...
make lint          # golangci-lint
make build         # CGO_ENABLED=0 build
make image         # docker image
```

## License

[Apache 2.0](LICENSE).
