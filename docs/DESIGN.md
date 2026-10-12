# Design

pii-gateway is a LiteLLM proxy plus a protocol-aware bridge (`litellm/pii_bridge.py`) that
pseudonymizes personal data in coding-agent traffic and restores it in responses. Detection and the
session mapping live in a modified Palena service (`vendor/palena`) backed by Presidio and Redis.

## Why a bridge instead of `generic_guardrail_api`

LiteLLM's stock guardrail path is not enough for coding agents:

| Gap | Effect for agents |
|---|---|
| Guardrail response has no `tool_calls` field | `tool_use.input` / `function_call.arguments` are never pseudonymized in history and never restored in responses |
| Responses `function_call_output.output` is not in `texts` | Codex tool results (file contents) leak |
| Streaming rewrite only on `/v1/chat/completions` | Claude Code (`/v1/messages`, always streams) and Codex (`/v1/responses`) get pseudonyms back instead of real names |
| `/v1/responses/compact`, WebSocket `/v1/responses`, `/anthropic/*` passthrough are unguarded | bypass routes |
| Returning `structured_messages` rebuilds history | drops `thinking.signature` and `encrypted_content` |

What LiteLLM does provide and the bridge relies on: client session headers
(`x-claude-code-session-id`, Codex `session-id`, `x-session-id`) are mapped to `litellm_trace_id`;
thinking, reasoning and compaction items are not scanned on the default path;
`general_settings.forward_client_headers_to_llm_api: true` forwards Claude subscription OAuth.

## Architecture

```
client (Claude Code / Codex / Pi)
   |  x-claude-code-session-id / session-id / x-session-id
   v
LiteLLM proxy :14000
   +- callbacks: pii_bridge.PiiBridge  (litellm/pii_bridge.py)
        pre_call   : walk request per protocol -> collect strings -> Palena (input_type=request,
                     litellm_trace_id=<session>) -> write back in place. Fail closed.
        post_call  : non-stream response -> walk -> Palena (input_type=response) -> write back
        stream hook: per content block / output item buffer, reverse with holdback so a
                     pseudonym split across SSE events is restored correctly
        route guard: reject unguarded call types (compact, passthrough, ws) with 4xx
   v
Palena :18080  (detection via Presidio :15002, session mapping in Redis :16379)
```

The stock `palena` guardrail is not enabled; the bridge calls Palena's HTTP API directly to avoid
double processing. Modules: `pii_bridge.py` (hooks, Palena client, session, route guard),
`pii_walk.py` (pure request/response walkers), `pii_stream.py` (holdback reverser and per-route
restorers), `pii_leak.py` (post-masking leak scan), `pii_audit.py`, `pii_metrics.py`.

## What the bridge rewrites

| Protocol | Pseudonymize on request | Never touch | Restore on response |
|---|---|---|---|
| Anthropic `/v1/messages` | `system` (string or text blocks), `text` blocks, `tool_result.content`, string leaves of `tool_use.input` | `thinking`, `redacted_thinking`, `signature`, ids, `cache_control`, images | `text` blocks / `text_delta`, `tool_use.input` / `input_json_delta` (reversed on the assembled JSON) |
| OpenAI chat | `messages[].content` (string or text parts), string leaves of `tool_calls[].function.arguments` | `reasoning_content`, ids | `content`, `tool_calls` arguments |
| OpenAI Responses | `instructions`, `input` string, message `content[].text`, `function_call_output.output`, string leaves of `function_call.arguments` | `reasoning` items, `encrypted_content`, compaction items, ids | `output_text`, function_call `arguments` |

String leaves of JSON arguments are pseudonymized individually so the JSON stays valid; an
`arguments` string is only re-serialised if a leaf changed.

Policy notes:
- Thinking text shown to the user keeps pseudonyms: restoring it would break the signature when the
  client sends it back.
- Tool definitions (`tools[].description`) are left untouched (static, no PII expected).

Handled call types: `anthropic_messages`, `acompletion`, `aresponses`, `acompact_responses` (request
is pseudonymized, response is opaque). Any other call type carrying content is rejected with HTTP 400.

### Request side

One Palena call per request. A per-(session, text) LRU skips unchanged history
(`PII_BRIDGE_CACHE=0` disables); the cache key includes the pattern-only mode.

### Streaming

The reverse map is read once per stream from Redis (`HGET <key> mapping`, inverted), then reversal is
local, holding back any trailing proper prefix of a pseudonym.
- `/v1/messages`: bytes are re-framed into SSE events; only `text_delta` / `input_json_delta` events
  that change are re-serialised, everything else (thinking, signature, ping) passes byte-identical.
  A held tail is flushed as an extra delta before `content_block_stop`.
- Chat: `delta.content` and `tool_calls[].function.arguments` per choice/tool index; the tail is
  released on the `finish_reason` chunk.
- Responses: `output_text.delta`, `function_call_arguments.delta`, `custom_tool_call_input.delta`
  incrementally; `*.done`, `content_part.done`, `output_item.*` and `response.completed` carry fully
  reversed texts.
- Tool-argument streams use JSON mode (replacement is JSON-escaped).

## Session key

`litellm_trace_id`, then `litellm_session_id`, body metadata, raw headers, and finally the API-key
alias with a WARN (`session_id_source="api_key_fallback"`). Never `litellm_call_id`: that would
silently break multi-turn stability. The session is sent to Palena as `litellm_trace_id`; Redis key
is `palena:pseudonymizer:<session>`.

## Palena changes (vendor/palena)

See `NOTICE` for the list. In short:
- Typed, non-declinable tokens (`<PERSON_1>`) instead of pool names, because Russian declension
  breaks reversal of realistic names.
- Session TTL in seconds (`REDIS_SESSION_TTL_SECONDS`, accepts `3600`, `24h`, `0` = never expire;
  scripts default to 0). Redis runs with AOF so mappings survive restarts. Merge is atomic (Lua).
- No-restore entity types (`PALENA_PSEUDONYMIZER_NO_RESTORE_ENTITIES`, default `SECRET`): their
  `<TYPE_n>` tokens stay masked in responses.

## Detection (Presidio + Palena)

- **Secrets**: the "Secrets" recognizer in `presidio/recognizers.yaml` (entity `SECRET`, regexes
  adapted from gitleaks: AWS key id and secret, GitHub, OpenAI/Anthropic `sk-`, Slack `xox*`, JWT, PEM
  private keys, generic `password|token|api_key = value`). Strategy `token` gives `<SECRET_n>`.
- **Mixed language**: with several `PRESIDIO_LANGUAGES`, Palena analyzes every text in all of them
  and unions the spans. NER spans in the wrong script for their language are trimmed or dropped;
  overlaps prefer the longer span, and a narrower higher-scoring recognizer hit beats a wider NER span.
- **Russian short names** (Коля, Саша, ...) come from a regex list recognizer and get their own token,
  not linked to "Николай Петров". Names outside that list and spaCy's knowledge are missed.

### Robustness (`vendor/palena/internal/text`)

- NER is format-sensitive: a name inside JSON-escaped tool output, CSV or a `// TODO(..)` comment is
  often missed on first sighting. Every text is also analyzed in two same-length "exploded" views
  (`views.go`: structural punctuation and literal `\n \" ...` escapes neutralised); spans are cut at
  the blanked delimiters and unioned with the plain view.
- Interior foreign-script words do not make a whole NER span "foreign".
- Over-detection filters: an allowlist (`PALENA_PSEUDONYMIZER_ALLOWLIST_FILE`, default
  `config/allowlist.txt`; whole span or every word, case-insensitive) and a structural filter for
  PERSON/ORGANIZATION (filenames such as `CLAUDE.md`, lowercase path components, URLs, snake_case,
  ALLCAPS, CamelCase, letter+digit tokens, stop words).
- **Common-word filter** (`PALENA_PSEUDONYMIZER_COMMON_WORDS_FILE`, default `config/common_words.txt`,
  built by `scripts/build_common_words.py`): an NER-derived, single-word PERSON/ORGANIZATION span
  whose lower-case form is a common dictionary word is dropped ("Client", "Contact", "Billing" are
  typical spaCy false positives on capitalised nouns). It runs before person-span extension, so such
  a word cannot grow into a two-word name. Not affected: pattern-recognizer hits, multi-word spans,
  values already in the session mapping. The list is the top 25k English + 25k Russian wordfreq
  words minus common first names, the 3000 most frequent US surnames and Russian first names and
  surname-suffix words, plus a few dev terms. Trade-off: a lone surname that is also a dictionary
  word and not in the top-3000 list is not masked when seen alone (multi-word "Jane Baker" still is).
- The replacer treats a quote opening a string (`'Name'`) as a word boundary, and a literal escape
  sequence before a value (`...py.\nMargaret`) does not block the boundary.

### Pattern-only system text

Agent system prompts (vendor text plus the installed skills/plugins list) contain many product names
that spaCy tags as PERSON/ORGANIZATION (around 100 false positives per Claude Code session).
Allowlisting is whack-a-mole, so such texts are sent as pattern-only:

- The bridge (`pii_walk.collect_request(..., system_pattern_only=True)`, env
  `PII_BRIDGE_SYSTEM_PATTERN_ONLY`, default 1) marks Anthropic top-level `system` and `messages[]`
  with role `system`/`developer`, OpenAI chat messages with those roles, and Responses `instructions`
  and input messages with those roles. Not marked: user messages (Claude Code's CLAUDE.md arrives in
  a user `<system-reminder>`), assistant text, tool results and arguments. The indices go to Palena in
  `additional_provider_specific_params.pattern_only_indices`.
- Palena drops, for those indices, every Presidio detection whose `recognition_metadata.recognizer_name`
  is an NER-model recognizer (spaCy, Stanza, Transformers, Flair, GLiNER). Pattern recognizers (email,
  phone, SECRET, deny-list orgs, Russian short names) still apply, and values already in the session
  mapping are still replaced. Missing metadata keeps the detection (fail towards masking).
- Trade-off: a person name that first appears only in a system prompt is not detected there; it is
  caught when it shows up in user text or tool output, and then masked everywhere.

## Failure behaviour

If Palena or Redis is unreachable the bridge fails closed: the request is rejected, nothing is
forwarded. Audit write failures are logged and counted but do not break traffic unless
`PII_AUDIT_REQUIRED=1`. See `docs/OBSERVABILITY.md`.

## Not covered

`/v1/messages/count_tokens` (LiteLLM runs no hooks on it), `/anthropic/*` and other passthrough
routes, WebSocket `/v1/responses`, documents / `search_result` / `server_tool_use` blocks, images,
`tools[]` definitions, and entities Presidio misses (for example an e-mail embedded in a path).
