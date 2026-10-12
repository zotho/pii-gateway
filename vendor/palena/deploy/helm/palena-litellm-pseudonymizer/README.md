# palena-litellm-pseudonymizer Helm chart

Deploys the pseudonymization guardrail into Kubernetes, optionally bundling
its Redis and Presidio (Analyzer + Image Redactor) dependencies.

## Install

```bash
helm install pseudonymizer ./deploy/helm/palena-litellm-pseudonymizer \
  --namespace guardrails --create-namespace
```

By default this brings up the guardrail service plus a single-instance
Redis and both Presidio services — a self-contained stack good for a PoC.
For production, disable the bundled dependencies and point at managed ones
(see below).

## Organization detection

`ORGANIZATION` masking has two layers, both configured here:

| | Default | What it does |
| --- | --- | --- |
| **NER** (`presidio.analyzer.nerOrganization`) | `true` | spaCy detects org-looking names automatically. Baseline defense with zero configuration — but somewhat noisy. |
| **Deny-list** (`presidio.analyzer.organizations`) | empty | Exact-match list of known company names. Zero false positives. Add your real clients here for high precision. |

NER is on by default so orgs are masked even before you curate a list.
Add known names for precision:

```yaml
presidio:
  analyzer:
    organizations:
      - Novartis
      - Roche
      - Contoso Ltd
```

To rely solely on the deny-list (quietest, no false positives), set
`presidio.analyzer.nerOrganization: false` and populate `organizations`.

## Production: external dependencies

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

## Wiring into LiteLLM

After install, `helm` prints the service URL and a ready-to-paste
`generic_guardrail_api` guardrail block. The service exposes
`/beta/litellm_basic_guardrail_api` on `service.port` (8080).

## Key values

| Key | Default | Notes |
| --- | --- | --- |
| `replicaCount` | `2` | Guardrail service replicas. |
| `image.repository` / `image.tag` | GHCR / appVersion | Service image. |
| `config.entities` | `PERSON,ORGANIZATION` | LOCATION excluded (geography drift). |
| `config.decomposePersonNames` | `true` | First/last-name sub-mappings. |
| `apiKey.enabled` | `false` | Enable + set `value`/`existingSecret` to require `x-api-key`. |
| `redis.enabled` | `true` | Bundle Redis; disable + set `url` for external. |
| `presidio.analyzer.nerOrganization` | `true` | spaCy ORG detection. |
| `presidio.analyzer.organizations` | `[]` | Deny-list of known org names. |
| `presidio.analyzer.denyLists` | `[]` | Deny-lists for any entity type (`- entity: PERSON` / `terms: [...]`). |
| `config.languages` | `[]` | Several languages (`en`, `de`, `fr`, `it`): each message is analyzed in its own; switches to `presidio.analyzer.multilingualImage`. ~800Mi analyzer memory per extra language. |
| `config.entityScoreThreshold` | `PHONE_NUMBER:0.4` | Per-type minimum score. |
| `autoscaling.enabled` | `false` | HPA on CPU. |
| `serviceMonitor.enabled` | `false` | Prometheus Operator scrape of `/metrics`. |

Full list in [values.yaml](values.yaml). Service config semantics are
documented in the project docs.

## Uninstall

```bash
helm uninstall pseudonymizer --namespace guardrails
```
