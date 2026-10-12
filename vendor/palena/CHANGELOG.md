# Changelog

All notable changes to the Palena Pseudonymizer are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-10-09

### Added

- **Several languages per deployment.** `PRESIDIO_LANGUAGES` (chart
  `config.languages`) lists the languages users write in; each message is
  classified among them offline (lingua-go) and analyzed in its own language,
  falling back to the first. German text analyzed as English missed most German
  names and tagged ordinary words as organizations. One language behaves as
  before.
- **Multilingual Presidio analyzer image.** `ghcr.io/palenaai/palena-presidio-analyzer`
  (published with each release) is Presidio plus the German, French and Italian
  spaCy models. The chart switches to it, and renders the analyzer's NLP,
  recognizer and language config, as soon as `config.languages` holds anything
  but English. Each extra language needs roughly 800Mi more analyzer memory.
- **Deny-lists for any entity type.** Chart `presidio.analyzer.denyLists`
  (`entity` + `terms`) makes exact-match terms always detected as that type, in
  every configured language. `presidio.analyzer.organizations` still works.
- **Per-type score thresholds.** `ENTITY_SCORE_THRESHOLD` (chart
  `config.entityScoreThreshold`) overrides `PRESIDIO_SCORE_THRESHOLD` per entity
  type and defaults to `PHONE_NUMBER:0.4`: Presidio scores every phone number
  0.4, so under the 0.7 default none was ever masked.

### Changed

- The chart's default memory limit for the service is 512Mi (was 256Mi): the
  language classifier's models take about 140Mi when several languages are set.

### Fixed

- **Entities after a non-ASCII character were left unmasked.** Presidio returns
  character offsets and the service sliced the text by bytes, so any accented
  letter before an entity ("Zürich", "rencontré") shifted the span and the name
  went through to the model unchanged. Offsets are now converted to byte offsets.

## [0.4.1] - 2026-09-24

### Fixed

- **The chart's own Service load-balanced across the bundled dependencies.**
  `pseudonymizer.selectorLabels` is the label set shared by *every* object in the
  release, and the main Service, PodDisruptionBudget and ServiceMonitor selected
  on it alone. The bundled redis and Presidio deployments carry those same labels
  plus their own `app.kubernetes.io/component`, so all three selectors matched
  every pod in the release rather than the pseudonymizer's own.

  In practice the Service sent a share of traffic to presidio-analyzer,
  presidio-image-redactor and redis, none of which serve `/v1/waeg/check` or
  `/beta/litellm_basic_guardrail_api`: Presidio answers `404` (its Flask handler,
  so the body is an HTML-ish JSON error, not ours) and redis is not HTTP at all.
  With the default 2 replicas that is 3 wrong endpoints out of 5 — roughly 40% of
  requests measured against a real deployment. A gateway configured the way the
  docs require (`on_error: closed`) turns every one of those into a blocked
  request, so chat traffic failed intermittently with no clue pointing here.

  The pseudonymizer's own pods now carry `app.kubernetes.io/component:
  pseudonymizer`, and the Service, PDB and ServiceMonitor select on it.

  The Deployment's `spec.selector` is deliberately left as it was: it is
  immutable once created, so narrowing it would break `helm upgrade` from 0.4.0
  with "field is immutable". The overlap is harmless there — the dependencies'
  pods already carry a controller ownerRef and cannot be adopted.

  Upgrading from 0.4.0 replaces the pods (the pod template gains a label) and
  needs no manual step.

- **`helm install` with the chart's own default values never started.** Two
  values were rendered in a form the service rejects at startup, so a plain
  install crash-looped and only a caller that happened to override them (the
  Navique AI Core operator does) ever ran:
  - `config.session.ttlSeconds: 3600` reached the service as `"3600"`, but it is
    parsed as a Go `time.Duration`, which requires a unit —
    `converting '3600' to type time.Duration: missing unit`. The template now
    accepts both a bare number (seconds) and an explicit duration such as `90m`.
    The env var's `_SECONDS` name and its `time.Duration` type still disagree;
    reconciling them is left for a release that can change the contract.
  - `config.maxRequestBytes` and `config.nonText.maxImageBytes` were rendered in
    scientific notation (`3.3554432e+07`), since Helm parses YAML numbers as
    float64 — `strconv.ParseInt: invalid syntax`. Both are now cast with `int64`.

## [0.4.0] - 2026-08-06

### Added

- **Wäg gateway support** — a second endpoint, `POST /v1/waeg/check`, speaking
  the Wäg HTTP guardrail protocol (`allow` / `mask` / `block`) against the same
  pseudonymization core. Maps `pre_call` to pseudonymization and
  `during_call` / `post_call` to reversal, rewriting the uncommitted stream
  window with `hold_chars` so partially-streamed names are never emitted
  half-substituted. Accepts `Authorization: Bearer` in addition to `x-api-key`,
  since that is how Wäg authenticates.
- **Documentation** — [Wire into Wäg](docs/guide/waeg-integration.md), covering
  the required `role: transform` and `on_error: closed` item configuration,
  phase mapping, and session-id behaviour.

### Notes

- The LiteLLM endpoint, its wire contract and its behaviour are unchanged; this
  release is additive and needs no configuration or chart changes to upgrade.
- Wäg defaults to **fail-open**. This service fails closed (`502`) when Presidio
  or Redis is unreachable, but Wäg reads a non-action response as a host error
  and forwards the original prompt unless the item sets `on_error: closed`.
  Configure it.

## [0.3.0] - 2026-07-04

### Added

- **Allow-list of never-pseudonymized terms** (`ALLOW_LIST`) — your brand /
  product names, public figures, or words Presidio over-tags (e.g. "SSN" as an
  organization) are left verbatim even when detected. Case-insensitive; also
  excluded from first/last-name decomposition. Symmetric to the org deny-list.
  Applies to text and OCR'd image entities.
- **Deterministic pseudonyms** (`DETERMINISTIC_SECRET`) — optional keyed-HMAC
  pool assignment so the same real name maps to the same pseudonym across
  sessions (stable, portable pseudonyms). Token entities unaffected. Wired as a
  Kubernetes Secret in the Helm chart.
- **Session erasure endpoint** — `DELETE /sessions/{session_id}` removes a
  session's mapping on demand (GDPR right-to-erasure / explicit teardown),
  guarded by the same `x-api-key` shared secret. Idempotent; returns
  `{"deleted": true|false}`.

### Changed

- **Collision hardening** — a freshly-assigned pseudonym is never allowed to be
  a substring of the text it is inserted into, so a user- or model-supplied
  lookalike (e.g. someone literally typing `<CREDIT_CARD_1>`, or a pool name
  already present in the text) can no longer alias a real value and make the
  reverse pass ambiguous. Token indices and pool candidates are skipped until a
  non-colliding one is found.

## [0.2.0] - 2026-07-04

### Added

- **Optional structured-PII pseudonymization** — any entity type Presidio
  detects (`CREDIT_CARD`, `US_SSN`, `IBAN_CODE`, `EMAIL_ADDRESS`,
  `PHONE_NUMBER`, custom IDs, …) can now be enabled via
  `PRESIDIO_ENTITIES`. Two substitution strategies:
  - **`pool`** — realistic fictional value (unchanged behaviour for nominal
    identities: `PERSON` / `ORGANIZATION` / `LOCATION`).
  - **`token`** — a consistent reversible placeholder (`<CREDIT_CARD_1>`),
    the new default for non-nominal types. Hides the value, signals the
    redaction to the model, and reverses to the exact original.
  - Configurable via `ENTITY_STRATEGY_DEFAULT` and per-type
    `ENTITY_STRATEGY` overrides (`"PHONE_NUMBER:pool,CREDIT_CARD:token"`);
    surfaced in the Helm chart. Nominal types default to `pool` regardless
    of the default, so existing deployments are unaffected.

### Fixed

- Token pseudonyms are no longer case-folded by the case-preserving replacer
  in either direction (forward masking or response reversal), so a token like
  `<EMAIL_ADDRESS_1>` stays verbatim and reverses to the exact original value.

## [0.1.0] - 2026-06-03

Initial release.

### Added

- **LiteLLM `generic_guardrail_api` HTTP service** exposing
  `POST /beta/litellm_basic_guardrail_api`, plus `/healthz`, `/readyz`, and
  `/metrics`. Handles both the pre-call (`input_type: request`) and post-call
  (`input_type: response`) phases at one endpoint.
- **Text pseudonymization** — detects `PERSON` / `ORGANIZATION` entities via
  Presidio Analyzer and substitutes each with a consistent fictional pseudonym
  from configurable pools; reverses the substitution in the response.
  - Case-insensitive matching with case-preserving output, possessive-safe,
    longest-match-first, word-boundary aware (RE2, no lookaround).
  - **Gender-neutral default person pool** so a masked name never leaks a wrong
    gender into the model's reasoning.
  - **`LOCATION` off by default** to avoid geography drift (a masked city makes
    the model reason from the wrong place).
- **Person-name decomposition** — mapping "Alice Johnson → Jordan Avery" also
  records "Alice → Jordan" and "Johnson → Avery", so bare first/last-name
  references stay consistent and reverse correctly. Toggle with
  `PALENA_PSEUDONYMIZER_DECOMPOSE_PERSON_NAMES`.
- **Streaming reversal** via LiteLLM's `streaming_transform_mode:
  incremental_diff`. The service returns `stream_holdback_chars` per choice so
  the framework never emits a partial pseudonym mid-stream.
- **Image PII** — detects and redacts PII in image attachments via Presidio
  Image Redactor (`redact` / `block` / `passthrough`), sharing OCR'd names with
  the text session mapping.
- **Session-scoped Redis mapping** — real↔pseudonym mappings are namespaced by
  a derived `session_id` (metadata → end-user → trace id → call id), stored via
  an atomic Lua merge (existing assignments win), with a refreshing TTL.
- **Fail-closed semantics** — if Presidio or Redis is unreachable on the way to
  the model, the request is blocked; post-call reversal failures degrade to a
  passthrough so a successful response is never lost.
- **No-PII audit invariant** — structured logs and Prometheus metrics carry
  counts and entity-type labels only, never real names or pseudonyms; session
  ids appear only as short hashes.
- **Helm chart** (`deploy/helm/palena-litellm-pseudonymizer`) deploying
  the service with optional bundled Redis and Presidio. Organization detection
  is on by default (spaCy NER) with an optional exact-match deny-list of known
  company names for high precision.
- **VitePress documentation site** under `docs/` (getting started,
  configuration, organization detection, deployment, LiteLLM integration, HTTP
  protocol, environment reference).
- **CI/CD** — lint + race tests, Semgrep, Hadolint, zizmor, govulncheck,
  dependency review, conventional-commit and license-header checks, Trivy image
  scanning with SBOM, cosign keyless signing, SLSA Level 3 provenance, and
  OpenSSF Scorecard.

[Unreleased]: https://github.com/PalenaAI/palena-litellm-pseudonymizer/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/PalenaAI/palena-litellm-pseudonymizer/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/PalenaAI/palena-litellm-pseudonymizer/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/PalenaAI/palena-litellm-pseudonymizer/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/PalenaAI/palena-litellm-pseudonymizer/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/PalenaAI/palena-litellm-pseudonymizer/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/PalenaAI/palena-litellm-pseudonymizer/releases/tag/v0.1.0
