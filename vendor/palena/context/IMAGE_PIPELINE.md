# IMAGE_PIPELINE.md — image PII flow

## Scope for v1

- Images arriving as `images: string[]` in the request body
  (base64 data URLs OR remote URLs). Detect PII via Presidio Image
  Redactor and produce a same-length response with redacted images.
- Images embedded inside `structured_messages[*].content` as blocks of
  `type: "image_url"`. These are informational — LiteLLM already
  extracts them into `images[]` for us, so we only act on the flat
  list.
- **1:1 length contract.** `response.images[i]` corresponds to
  `request.images[i]`. Never add or remove entries.

**Not in v1:** documents (PDF, DOCX, XLSX). See "Documents (v2)"
below for the planned approach.

## Data flow (per image)

```
image_bytes (raw or base64-decoded)
    │
    ▼
 Presidio Image Redactor  ──►  {redacted_bytes, [detections]}
    │                            │
    │                            └─►  OCR'd entity text +
    │                                 bounding boxes + score + type
    │
    ▼
 Filter detections by config.entities and score_threshold
    │
    ├── none relevant  ──►  action=NONE for this image
    │
    ▼
 Share OCR'd names with the text session mapping
    │  (so the response can reverse them if the LLM types them)
    │
    ▼
 Config.non_text_pii_action:
    │
    ├── "redact"       ──►  emit redacted_bytes
    ├── "block"        ──►  action=BLOCKED, blocked_reason=template
    └── "passthrough"  ──►  emit original bytes unchanged
```

## Presidio Image Redactor request

Multipart upload. The service exposes at least these endpoints; we use
both in different modes.

### `/analyze` — detections only

```
POST {image_redactor_url}/analyze
Content-Type: multipart/form-data

image: <bytes>
```

Response is a JSON array of detections:

```json
[
  {"entity_type": "PERSON", "text": "Thomas Weber", "score": 0.82,
   "left": 42, "top": 100, "width": 120, "height": 24}
]
```

Note that the OCR'd `text` may be noisy (Tesseract). We accept it as-is
and let it feed the session mapping.

### `/redact` — redacted image + detections

```
POST {image_redactor_url}/redact
Content-Type: multipart/form-data

image: <bytes>
color: "black"   # form field, per Presidio's API
```

Response is binary image bytes (PNG) with a JSON header describing the
detections. Presidio's exact response shape varies by version — the Go
client should accept both:

- A `multipart/mixed` response with one binary part and one JSON part.
- A JSON response with the redacted image as a base64 field
  (`{"image_base64": "…", "detections": […]}`).

Detect by `Content-Type` and branch accordingly. Fixture-test both.

## The `images` field format

LiteLLM sends `images[i]` as either:

1. A `data:` URL:
   `data:image/png;base64,iVBORw0KGgoAAAA…`
2. A remote URL:
   `https://cdn.example.com/foo.jpg`

The service MUST accept both. Handling:

- **Case 1 (data URL):** extract media type + base64 payload from the
  URL. `strings.Cut(url, ",")` after checking the `data:` prefix.
  Decode base64. Pass raw bytes + media type to the image redactor.
- **Case 2 (remote URL):** fetch with `http.Client` under the request's
  context, with a size cap
  (`PALENA_PSEUDONYMIZER_MAX_IMAGE_BYTES`, default 20 MiB) and a
  fetch timeout (`presidio.timeout_seconds`). On fetch failure →
  block the request with reason `image_fetch_failed` if pre-call
  (fail-closed); log and pass-through on post-call.

On the response side we always emit `data:image/png;base64,…` (Presidio
returns PNG). This means the response `images[i]` may be a `data:` URL
even when the request was a remote URL — that's fine, LiteLLM feeds
data URLs to LLMs without issue.

## Size limits

Guard against runaway payloads:

| Limit | Env var | Default | Behavior on breach |
| --- | --- | --- | --- |
| Per-image bytes | `PALENA_PSEUDONYMIZER_MAX_IMAGE_BYTES` | 20 MiB | HTTP 413 (whole request rejected — LiteLLM shouldn't send this much) |
| Total request bytes | `PALENA_PSEUDONYMIZER_MAX_REQUEST_BYTES` | 32 MiB | HTTP 413 (see [PROTOCOL.md](PROTOCOL.md#request-schema)) |
| Images per request | `PALENA_PSEUDONYMIZER_MAX_IMAGES_PER_REQUEST` | 20 | `action: BLOCKED` with reason `too_many_images` |

The per-image limit protects Presidio (which OCRs the whole image and
can OOM on multi-megapixel scans). The images-per-request limit
protects our own memory + Presidio round-trip budget.

## OCR text → session mapping

Every relevant detection is shared with the text session mapping so
downstream reversal works. Algorithm mirrors `image_handler._add_to_mapping`
in the Python code:

```
existing = mapping.GetMapping(session_id)
scratch  = existing            # start from what we know
new_map  = {}

# Deterministic order: longest first, then alphabetical
for det in sorted(relevant, key=(-len(det.text), det.text)):
    real = det.text.strip()
    if real == "": continue
    if real in scratch: continue
    if lowercase(real) in { lowercase(k) for k in scratch }: continue
    pseudo = pool.Assign(det.entity_type, scratch)
    new_map[real] = pseudo
    scratch[real] = pseudo

if len(new_map) > 0:
    mapping.AddMappings(session_id, new_map)
```

Important: we ADD to the mapping even when `pii_action == "block"`.
Because retries and idempotency — the same block message with the same
inputs must return consistently. And the mapping is safe (no real PII
is emitted).

## Config knobs (per image handler)

Full list in [CONFIG.md](CONFIG.md#non-text-content). Highlights:

| Setting | Env var | Default |
| --- | --- | --- |
| PII action | `PALENA_PSEUDONYMIZER_NON_TEXT_PII_ACTION` | `redact` |
| Block message template | `PALENA_PSEUDONYMIZER_NON_TEXT_BLOCK_MESSAGE` | see below |
| Max image bytes | `PALENA_PSEUDONYMIZER_MAX_IMAGE_BYTES` | 20971520 (20 MiB) |
| Max images / request | `PALENA_PSEUDONYMIZER_MAX_IMAGES_PER_REQUEST` | 20 |

Default block message:

```
This attachment contains personal data ({entity_types}) that cannot be
pseudonymized. Please remove sensitive information and try again.
```

`{entity_types}` is replaced with a comma-joined sorted set of the
entity types seen (e.g. `LOCATION, PERSON`).

## Response construction

For each `request.images[i]`:

| Handler decision | Response `images[i]` |
| --- | --- |
| No relevant PII | Copy of input (unchanged) |
| `action=redact` and PII detected | `data:image/png;base64,{redacted}` |
| `action=passthrough` and PII detected | Copy of input (unchanged) |
| `action=block` (any image had PII) | Response body is `{"action": "BLOCKED", "blocked_reason": "…"}`; `images` array is not sent |

For `NONE` at the request level (nothing modified across all images and
all texts), we may omit the `images` array entirely — LiteLLM keeps
the originals when the response field is missing.

## Documents (v2)

Deferred. When we come back to this, the design options are:

1. **Port `palena-doc-rasterizer` to Go.** The Python version uses
   `pypdfium2` (PDF), `python-docx` (DOCX), `openpyxl` (XLSX). Go
   equivalents: `github.com/unidoc/unipdf` (AGPL/commercial — needs the commercial license for an Apache-2.0 project),
   `github.com/nguyenthenguyen/docx`, `github.com/xuri/excelize`.
   Same-language upside, non-trivial porting cost.

2. **Call `palena-doc-rasterizer` as a sidecar HTTP service.** Wrap the
   existing Python library in a thin HTTP shim
   (`POST /rasterize` returns PNG pages), run alongside the Go
   service. Cheapest to build, one extra deploy artifact.

3. **Drop document support.** Documents get `action: BLOCKED` with a
   clear reason. Force clients to rasterize themselves upstream.

We'll pick after M1–M3 land and we see actual production request
shapes. Don't build the abstraction now.

For v1, documents arriving in `structured_messages` as `type: document`
blocks or in `images[]` with a PDF media type → `action: BLOCKED`,
`blocked_reason: documents_not_supported_in_v1`. Log the size and
media type at INFO for demand forecasting.
