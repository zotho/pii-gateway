# Organization detection

Detecting organization names is harder than detecting people. Presidio's
default build **disables** organization detection because generic NER is noisy.
The guardrail turns it on and gives you two layers to combine.

## The two layers

| Layer | Catches | Precision | Configure |
| --- | --- | --- | --- |
| **NER** | any org-looking name | good, but occasionally over-flags | `presidio.analyzer.nerOrganization` |
| **Deny-list** | only names you list | perfect (exact match) | `presidio.analyzer.organizations` |

**NER is on by default**, so organizations are masked even before you curate a
list. For known clients and companies, add a deny-list — it never produces a
false positive and always catches the names you care about.

## Add your organizations (Helm)

```yaml
presidio:
  analyzer:
    nerOrganization: true          # baseline defense (default)
    organizations:                 # high-precision exact matches
      - Novartis
      - Roche
      - Contoso Ltd
```

Names are matched case-insensitively at word boundaries. When the list is
non-empty, the chart mounts a full Presidio recognizer registry with your
deny-list added.

## Choosing a strategy

- **Just getting started / unknown inputs** → leave NER on, no deny-list. You
  get automatic coverage with some noise.
- **Known roster of clients** → add them to the deny-list. This is the
  high-value case for a business or legal workflow.
- **Zero tolerance for false positives** → set `nerOrganization: false` and
  rely solely on the deny-list.

## Local development

The repository's `docker-compose.yml` mounts the same configuration from
`deploy/presidio/` so organization detection works out of the box when you run
the stack locally. Edit `deploy/presidio/recognizers.yaml` to change the local
deny-list.

::: info Detection is Presidio's job
The guardrail service asks Presidio what counts as an organization and
pseudonymizes whatever comes back. Tuning *what* is detected happens in
Presidio configuration (the Helm values above), not in the service.
:::
