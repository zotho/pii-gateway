# IMPLEMENTATION_PLAN.md — milestones + file scaffolding

Sequenced so each milestone is shippable on its own and reveals design
mistakes early. Skip ahead only if you're prototyping — do not merge a
later milestone before the earlier one is green.

## Dependencies

- **LiteLLM streaming-transform PR.** Streaming reversal (M3) depends
  on the upstream LiteLLM PR that adds `streaming_transform_mode:
  incremental_diff` and `stream_holdback_chars` on the response. Until
  that lands, M3 can still be implemented behind the flag but is
  effectively dead code in production.
- **Presidio Analyzer + Image Redactor** running and reachable. Use
  `mcr.microsoft.com/presidio-analyzer` and
  `mcr.microsoft.com/presidio-image-redactor` in docker-compose.
- **Redis 7.** `redis:7-alpine` is fine.
- **Go 1.22+.** We use `slog`, generic type params in a couple of
  places, `for-range int`.

## M0 — Bootstrap (½ day)

Get a hello-world server running under `make` and `docker`. No business
logic yet — this milestone exists to nail down conventions.

**Files to create:**

```
go.mod
go.sum
Makefile
Dockerfile
docker-compose.yml
.golangci.yml
.dockerignore
LICENSE                                 # Apache-2.0
cmd/server/main.go                      # slog init, chi mux, /healthz, /readyz stub, listen+shutdown
internal/config/config.go               # empty Config struct + Load() + Validate()
internal/config/config_test.go
internal/httpapi/router.go              # chi.NewRouter, /healthz, /readyz stubs, /beta/litellm_basic_guardrail_api returning 501
internal/httpapi/router_test.go
```

**Verify:**

- `make lint test build` all pass on a clean checkout.
- `docker build .` produces a scratch image < 20 MiB.
- `curl :8080/healthz` returns 200.

## M1 — Text pipeline (2–3 days)

Full request/response for `input_type=request` and `input_type=response`
covering the `texts[]` field only. No images, no streaming, no
documents.

**New files:**

```
internal/config/config.go               # populate PresidioConfig, RedisConfig, PoolConfig, SessionConfig
internal/httpapi/types.go               # request/response DTOs from PROTOCOL.md
internal/httpapi/handler.go             # POST /beta/litellm_basic_guardrail_api
internal/httpapi/handler_test.go        # httptest-based end-to-end
internal/pseudonymizer/orchestrator.go  # Handle(ctx, req) — dispatches by input_type
internal/pseudonymizer/orchestrator_test.go
internal/text/replacer.go
internal/text/replacer_test.go          # copy Python's replacer test cases verbatim
internal/text/pools.go
internal/text/pools_test.go
internal/text/handler.go                # Pseudonymize / Reverse; wraps Presidio + mapping + pools + replacer
internal/text/handler_test.go
internal/presidio/analyzer.go
internal/presidio/analyzer_test.go      # httptest.NewServer with canned JSON
internal/mapping/store.go               # Store interface
internal/mapping/redis.go               # RedisStore, atomic AddMappings via Lua
internal/mapping/redis_test.go          # miniredis
internal/session/derive.go
internal/session/derive_test.go
internal/errs/errs.go                   # sentinel errors + typed wrappers
internal/audit/audit.go                 # counters + no-PII enforcement
```

**Key milestones inside M1:**

1. Replacer passes 100% of the ported Python test cases.
2. Session id derivation passes tests for every priority level in the
   chain.
3. Round-trip end-to-end test: POST `input_type=request` with
   `"Please review the Novartis contract for Thomas Weber"` returns
   `"Please review the {PoolPersonSomething} contract for {PoolOrgSomething}"`,
   then POST `input_type=response` with `"Hello {PoolPerson}"` gives back
   `"Hello Thomas Weber"` — with a real Redis (miniredis) between.
4. Fail-closed test: shut down presidio httptest server; pre-call
   returns 502, post-call returns 200 with `NONE`.

**Wire the service into a real LiteLLM proxy** at the end of M1. Config:

```yaml
guardrails:
  - guardrail_name: pseudonymizer
    litellm_params:
      guardrail: generic_guardrail_api
      api_base: http://palena-pseudonymizer:8080
      mode: [pre_call, post_call]
      unreachable_fallback: fail_closed
      default_on: true
      additional_provider_specific_params:
        session_id_metadata_key: chat_session_id
```

## M2 — Images (1–2 days)

Add `images[]` handling. Everything in `internal/image/`.

**New files:**

```
internal/image/handler.go
internal/image/handler_test.go
internal/image/dataurl.go               # parse and emit data: URLs
internal/image/dataurl_test.go
internal/image/fetch.go                 # remote URL fetching with size cap
internal/image/fetch_test.go            # httptest server for the fetch case
internal/presidio/image_redactor.go
internal/presidio/image_redactor_test.go
```

**Changes to existing:**

- `internal/pseudonymizer/orchestrator.go` — call image handler after
  text handler, propagate BLOCK.
- `internal/httpapi/handler.go` — accept and return `images[]`.
- `internal/httpapi/types.go` — response `images` field.
- `internal/config/config.go` — non-text-content config.

**Verify:**

- End-to-end: a base64 PNG containing a real name → response image is
  redacted (byte-for-byte different).
- Block mode: `PALENA_PSEUDONYMIZER_NON_TEXT_PII_ACTION=block` returns
  `action: BLOCKED` with `entity_types` interpolated correctly.
- Length contract: request with 2 images returns 2 images (or blocks).
- Reject: `structured_messages[*].content[type=document]` returns
  `BLOCKED` with `documents_not_supported_in_v1`.

## M3 — Streaming (1 day)

Wire up `stream_holdback_chars` in the response.

**New files:**

```
internal/text/holdback.go
internal/text/holdback_test.go          # port Python StreamingReplacer tests as property tests
```

**Changes to existing:**

- `internal/text/handler.go` — expose a `ReverseWithHoldback(text) (out, holdback)`
  that runs the replacer AND computes the holdback in one pass.
- `internal/httpapi/handler.go` — when `input_type=response` AND
  `additional_provider_specific_params.streaming_transform_mode ==
  "incremental_diff"`, include `stream_holdback_chars` on the response.
- `internal/httpapi/types.go` — response `stream_holdback_chars` field.

**Guard rails:**

- If `streaming_transform_mode` is set but the reverse map is empty,
  `stream_holdback_chars` is omitted (nothing to hold back — 0 is
  cheaper as absence than as `[0, 0, …]`).
- If a partial-request text is shorter than what's already known to
  have been emitted (see PROTOCOL.md's `stream_transform_underflow`),
  return `action: BLOCKED`. Note: we don't actually track emitted
  length ourselves — LiteLLM does — so we only emit
  `stream_transform_underflow` when we detect the shorter-than-input
  case (e.g. holdback would go negative).

**Verify:**

- Given a synthetic accumulated text ending with the prefix of a known
  pseudonym, `stream_holdback_chars` protects the boundary.
- Given an accumulated text ending with `"…delivered to "` (space, no
  key prefix), holdback is 0.
- Concat `feed(a) + feed(b) + flush()` equals `Replace(a+b)` for any
  random split.

## M4 — Packaging + observability + operator wiring (1 day)

Ship-quality polish:

**New files:**

```
deploy/helm/palena-litellm-pseudonymizer/…       # chart with values.yaml
deploy/kubernetes/kustomization.yaml                     # raw manifests for kind
deploy/docker-compose.override.yml                       # local dev
.github/workflows/ci.yml                                  # lint + test + build + image
.github/workflows/release.yml                             # tag → push image + GH release
CHANGELOG.md
```

**Changes to existing:**

- `internal/httpapi/router.go` — mount `/metrics` via `promhttp`.
- `cmd/server/main.go` — Prometheus registry init.
- Bump docs where needed.

**Operator side (separate repo — write the prompt for another session
via the same pattern as before):**

- Add `generic_guardrail_api` to the `LiteLLMGuardrail.spec.provider`
  enum in `api/v1alpha1/litellmguardrail_types.go`.
- Update `context/reference/litellmguardrail.md` with an example.
- Land, tag, release.

The operator change makes the guardrail declaratively deployable
alongside a LiteLLMInstance:

```yaml
apiVersion: litellm.palena.ai/v1alpha1
kind: LiteLLMGuardrail
metadata: { name: pseudonymizer }
spec:
  instanceRef: { name: my-gateway }
  guardrailName: pseudonymizer
  provider: generic_guardrail_api
  mode: pre_call            # informational; the service handles both phases
  apiBase: http://palena-pseudonymizer.guardrails:8080
  defaultOn: true
  params:
    unreachable_fallback: fail_closed
```

## Post-v1 backlog (not in scope, don't build unless asked)

- **Documents (v2).** Decide between porting `palena-doc-rasterizer`
  to Go, running it as a sidecar HTTP service, or dropping. See
  [IMAGE_PIPELINE.md](IMAGE_PIPELINE.md#documents-v2).
- **Custom entity types beyond PERSON/ORGANIZATION/LOCATION.** Trivial
  once we accept `POOL_<TYPE>` env vars — but adds real complexity
  around synthetic fallbacks for arbitrary Presidio types.
- **Anonymization vs pseudonymization option.** Some clients want
  `<PERSON>` placeholders, not fictional names. Different config
  branch; not v1.
- **Language other than `en`.** Presidio supports several; we already
  forward the language field, but pools are English-only.
- **HA Redis.** Cluster mode is stubbed in config; test in production
  before enabling.
- **Kubernetes CRD for pool overrides.** Currently pools are env vars;
  a CRD-managed dictionary would be nicer for a large deployment.
- **Full multimodal `structured_messages` writeback.** In v1 we
  operate on `texts[]` / `images[]` and let LiteLLM stitch back. If a
  future guardrail behavior needs to preserve position across multi-
  content messages (e.g. add a new content block), we'll extend the
  response schema — but wait for a real need.

## Test-writing conventions

Follow these to keep the port faithful:

1. **Copy Python fixtures before inventing new ones.** If Python passes
   for `"Novartis's contract" → "Acme Corp's contract"`, our test file
   contains that exact input/output.
2. **Table-driven.** One `TestXxx` per subject, with a `[]struct`
   holding cases + `t.Run(tc.name, ...)`.
3. **Race detector on.** `go test -race -count=1 ./...` is the CI
   command.
4. **No network in unit tests.** Use `httptest` and `miniredis`.
5. **Golden fixtures live in `testdata/`.** For request/response JSON,
   use `.golden.json` files and a `-update` flag that regenerates them.
6. **Assertions from `stretchr/testify/require` in the leaves, plain
   `t.Errorf` at package boundaries where readability matters.**
7. **Property tests for the streaming holdback.** `pgregory.net/rapid`
   is a good pick if we want a real property library; otherwise a
   simple loop over `rand.Intn(len(input))` splits.
8. **80% coverage floor** on `internal/text`, `internal/mapping`,
   `internal/pseudonymizer`, `internal/session`. Enforced in
   `make cover`.

## Definition of done

M1: text end-to-end works against a real LiteLLM proxy pointed at a
docker-compose stack, with a real Redis session shared across two
consecutive requests, and pseudonym mappings are stable across turns.

M2: images redacted end-to-end with a real Presidio Image Redactor.

M3: given the LiteLLM streaming-transform PR merged and deployed, a
streaming completion is reversed at the client, verified by a `curl`
against the proxy.

M4: helm-installable, prometheus-scraped, image published to GHCR, and
the operator CRD accepts `generic_guardrail_api` as a provider.
