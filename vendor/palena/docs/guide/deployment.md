# Deploy with Helm

The chart deploys the guardrail service and, optionally, its Redis and Presidio
dependencies.

## Install

```bash
helm install pseudonymizer \
  ./deploy/helm/palena-litellm-pseudonymizer \
  --namespace guardrails --create-namespace
```

By default this brings up the guardrail plus a single-instance Redis and both
Presidio services — a self-contained stack for a proof of concept. Helm prints
the in-cluster URL and a ready-to-paste LiteLLM guardrail block on install.

## Production: bring your own dependencies

Disable the bundled Redis and Presidio and point at managed instances:

```yaml
redis:
  enabled: false
  url: redis://my-managed-redis:6379/0

presidio:
  analyzer:
    enabled: false
    url: http://my-presidio-analyzer:3000
  imageRedactor:
    enabled: false
    url: http://my-image-redactor:3000
```

## Organization names

Add your known company names for high-precision masking (see
[Organization detection](/guide/organization-detection)):

```yaml
presidio:
  analyzer:
    organizations:
      - Novartis
      - Roche
```

## Common values

| Key | Default | Purpose |
| --- | --- | --- |
| `replicaCount` | `2` | Guardrail replicas. |
| `image.tag` | chart appVersion | Service image tag. |
| `config.entities` | `PERSON,ORGANIZATION` | Detected entity types. |
| `config.languages` | `[]` (= `config.language`, `en`) | Languages to analyze in; several switch to the multilingual analyzer image. |
| `config.entityScoreThreshold` | `PHONE_NUMBER:0.4` | Per-type minimum score. |
| `config.allowList` | `""` | Terms never pseudonymized. |
| `apiKey.enabled` | `false` | Require an `x-api-key` header. |
| `redis.enabled` | `true` | Bundle Redis. |
| `presidio.analyzer.nerOrganization` | `true` | spaCy org detection. |
| `presidio.analyzer.organizations` | `[]` | Exact-match org deny-list. |
| `presidio.analyzer.denyLists` | `[]` | Exact-match deny-lists for any entity type. |
| `presidio.analyzer.multilingualImage` | `ghcr.io/palenaai/palena-presidio-analyzer:<appVersion>` | Analyzer used for anything but English alone. |
| `autoscaling.enabled` | `false` | HPA on CPU. |
| `serviceMonitor.enabled` | `false` | Prometheus Operator scrape. |

The full list lives in the chart's `values.yaml`.

## Security

Set an API key before exposing the service beyond the proxy:

```yaml
apiKey:
  enabled: true
  value: "a-long-random-secret"       # or reference an existing Secret
  # existingSecret: my-secret
  # existingSecretKey: api-key
```

## Uninstall

```bash
helm uninstall pseudonymizer --namespace guardrails
```
