# PSEUDONYMIZATION_ALGORITHM.md — text pipeline

This is the heart of the service. Read this before touching
`internal/text`, `internal/pseudonymizer`, or the streaming holdback
logic. The Python reference implementation is at
[`../../palena-guardrail-pseudonymizer/src/palena/guardrail/pseudonymizer/`](../../palena-guardrail-pseudonymizer/src/palena/guardrail/pseudonymizer/),
specifically `replacer.py`, `text_handler.py`, `pools.py`, and
`streaming.py` — those are ground truth for edge-case behavior.

## Overview

For each `texts[i]` in a pre-call request:

1. **Detect** entities via Presidio Analyzer (`POST /analyze`).
2. **Filter** to entity types listed in config (`PERSON`, `ORGANIZATION`
   by default; `LOCATION` is off by default — see
   [CONFIG.md](CONFIG.md#presidio)) at or above the score threshold.
3. **Load** the existing session mapping from Redis.
4. **Assign** pseudonyms from configured pools for any newly-detected
   entities that don't yet have one.
5. **Persist** any new assignments back to Redis (existing entries win).
6. **Rewrite** the text with the merged mapping (see "Replacer rules"
   below).

For each `texts[i]` in a post-call request: skip detection entirely; load
the reverse mapping and rewrite.

## Presidio Analyzer request

```
POST {analyzer_url}/analyze
Content-Type: application/json

{
  "text": "Please review the Novartis contract for Thomas Weber.",
  "language": "en",
  "entities": ["PERSON", "ORGANIZATION", "LOCATION"],
  "score_threshold": 0.7
}
```

Response is a JSON array:

```json
[
  {"entity_type": "PERSON", "start": 34, "end": 46, "score": 0.85, "text": "Thomas Weber"},
  {"entity_type": "ORGANIZATION", "start": 18, "end": 26, "score": 0.9, "text": "Novartis"}
]
```

If `text` field is missing on any element, extract it via `text[start:end]`.
Overlapping spans are kept as-is; the replacer's longest-first ordering
handles overlaps naturally.

## Pool assignment

Pools are configured (see [CONFIG.md](CONFIG.md#pools)). Defaults:

```
PERSON       = [Jordan Avery, Taylor Morgan, Alex Rivera, Riley Quinn, Casey Bennett, Sam Ellis, Jamie Brooks, Cameron Hayes]
ORGANIZATION = [Acme Corp, Birch Industries, Cedar Systems, Delta Group, Elm Partners, Fern Solutions, Grove Holdings, Hazel Dynamics]
LOCATION     = [Riverside, Lakewood, Hillcrest, Meadowbrook, Stonefield, Clearwater]   # unused unless LOCATION opted in
```

The PERSON names are deliberately **gender-neutral** (unisex given names).
A pseudonym leaks its implied gender into the model's reasoning — map a
female real name to a male pseudonym and the model writes "he". Neutral
names give no strong prior, so the wrong gender never leaks, and this
needs no gender logic in code (it's pure pool curation). See the
[CONFIG.md pools note](CONFIG.md#pools) for the full rationale.

Assignment algorithm (matches Python `PoolManager.assign_pseudonym`):

1. `used` = set of all pseudonyms currently assigned to any real name in
   the session mapping.
2. Iterate the pool in configured order; return the first entry not in
   `used`.
3. If the pool is exhausted, fall back to `{Prefix}-{N}` where
   `Prefix ∈ {Person, Organization, Location}` and `N` is the smallest
   positive integer such that `{Prefix}-{N}` is not in `used`.
4. Unknown entity types fall back to `{TitleCase(EntityType)}-{N}`.

## Multi-entity single-request ordering

When one Presidio call returns multiple detections, we may need to
assign pseudonyms to several new entities inside one request. The
Python implementation sorts them **longest-first, then alphabetical**
before assignment to guarantee:

- Compound names get assigned before their components. If Presidio
  returns both `Thomas Weber` and `Thomas`, `Thomas Weber` is processed
  first, so `Thomas` gets a distinct pseudonym rather than colliding
  on the same assignment.
- Deterministic output for identical inputs — critical for tests and
  reproducibility.

Also within the same request, we must not hand out the same pseudonym
twice. Maintain a scratch mapping (`current ∪ new_so_far`) as you walk
the sorted detections and pass it to `assign_pseudonym` each time.

## Case handling in mapping

The mapping stored in Redis is case-sensitive on the key (`real → pseudo`).
But when checking whether we already have a mapping for a newly-detected
name, do a **case-insensitive** comparison against existing keys — so
`Novartis` and `novartis` share one pseudonym. The replacer's
case-preservation logic (below) handles emitting the pseudonym in the
right case at output time.

## Person-name decomposition

When `DecomposePersonNames` is on (default) and a **PERSON** entity has two
or more tokens, assigning the full name also registers first-name and
last-name sub-mappings, aligned by position:

```text
Alice Johnson  ->  Jordan Avery
  Alice        ->  Jordan            (first token)
  Johnson      ->  Avery             (last token)
```

All three land in the **same session's** Redis hash. Rationale and caveats
are in [CONFIG.md](CONFIG.md#text-handling); the mechanics that matter here:

- **Existing-wins across the session.** Components are added via the same
  atomic `AddMappings` merge, so if `Johnson` is already mapped (from an
  earlier turn or another person in the same session), it is left alone.
  Two people sharing a surname means only the first one's surname component
  is stored; the second degrades to "full name + first name reverse" (safe,
  non-leaking).
- **Scoped to the session, never global.** Component entries live under
  `{prefix}:{session_id}` exactly like full-name entries. `Alice → Jordan`
  in session A has no effect on session B — see
  [SESSION_AND_STATE.md](SESSION_AND_STATE.md#redis-schema). There is no
  shared/global name dictionary.
- **Longest-first keeps the full name winning.** The reverse map holds both
  `Jordan Avery → Alice Johnson` and `Jordan → Alice`; the replacer's
  longest-first ordering rewrites the full name before the bare first name,
  so `Jordan Avery` never degrades to `Alice Avery`.

## Replacer rules

Single regex, longest-first, case-insensitive. Emit the pseudonym with
the source's casing pattern applied. Word-boundary aware. Possessive
safe.

**Regex construction** (Go `regexp` syntax — RE2, not PCRE, so no
lookbehinds; we must use a manual pre-check instead):

```go
// Escape and sort keys longest-first.
keys := sortLongestFirst(keysOf(mapping))
alternation := strings.Join(escapeAll(keys), "|")

// (word|possessive)-boundary aware, case-insensitive.
pattern := "(?i)(?P<match>" + alternation + ")(?:'s|’s)?"
re := regexp.MustCompile(pattern)
```

RE2 has no lookbehind, so we cannot express the Python
`(?<![\w'])` guard directly. Instead, iterate matches and reject any
whose start index is preceded by a "word context" char (alphanumeric,
underscore, or apostrophe). See "Word-boundary check" below for the
loop.

Trailing `'s` / `’s` is matched into an *optional non-capturing* group
so we can strip it before lookup — the pseudonym is emitted plus the
same trailing possessive. This matches Python's lookahead behavior even
though we're doing it inside the pattern rather than after.

**Substitution loop** (pseudocode):

```go
for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
    // m[0]/m[1] = full match, m[2]/m[3] = named group "match"
    start, end := m[0], m[1]
    matchStart, matchEnd := m[2], m[3]

    // Word-boundary check (Python's (?<![\w']) equivalent):
    if start > 0 && isWordContextChar(text[start-1]) {
        keep(text[start:end])
        continue
    }

    source := text[matchStart:matchEnd]
    replacement := mapping[caseInsensitiveLookup(source)]
    replacement = matchCase(source, replacement)
    // Preserve any trailing 's / ’s captured beyond matchEnd
    tail := text[matchEnd:end]

    write(replacement + tail)
}
```

Because RE2 doesn't guarantee non-overlapping matches will honor
longest-first when alternatives share prefixes, the sort step at
alternation construction is what buys us longest-first correctness.

### `isWordContextChar`

Matches the Python `(?<![\w'])` semantics:

```go
func isWordContextChar(b byte) bool {
    return isAlphanumericASCII(b) || b == '_' || b == '\'' || b == 0xE2 // start of U+2019 apostrophe in UTF-8
}
```

Careful: apostrophe check needs to be Unicode-aware, not byte-level.
Iterate runes, not bytes, or use `utf8.DecodeLastRuneInString(text[:start])`.

### `matchCase`

Three-way case preservation (matches Python `_match_case`):

- All letters upper → uppercase the replacement.
- All letters lower → lowercase the replacement.
- Anything else → emit replacement as-is.

Filter non-letters when judging pattern (so `d'Angelo` is judged by
`dAngelo`, not by the presence of `'`).

### Test fixtures

Copy the Python replacer test cases verbatim into
`internal/text/replacer_test.go`. Golden inputs live in
`testdata/replacer/`. The Python suite covers:

- Longest-first: `John Mueller` vs `John` in the same mapping.
- Case-insensitive detection with case-preserving output:
  `Novartis` → `Acme Corp`, `novartis` → `acme corp`, `NOVARTIS` →
  `ACME CORP`.
- Possessives: `Novartis's` → `Acme Corp's`, `Novartis’s` (unicode
  apostrophe) → `Acme Corp’s`.
- Word-boundary rejection: `Acmeologist` must not match `Acme`.
- Prefix-collision safety: `John Mueller Jr.` doesn't get split at
  `John` and re-processed.

## Streaming holdback

Read this only if you're implementing the streaming-response path.

**Goal:** never emit half of a pseudonym. If the accumulated response
so far ends with `"Thomas We"`, we must NOT reverse it to nothing (which
would strand the client mid-word); we must hold back the trailing chars
until the next sample can decide.

**Algorithm** (matches Python `StreamingReplacer._compute_holdback`):

Let `buf` be the mutated accumulated text (post-replacer). Let `K` be
the set of reverse-map keys (the pseudonyms we might see in the LLM's
output, since post-call reverses `pseudo → real`). Let `L = max(len(k)
for k in K)`.

We want the largest integer `h ∈ [0, min(L-1, len(buf))]` such that
`buf[len(buf)-h:]` is a **proper prefix** of at least one key in `K`
AND the position `len(buf)-h` is at a word boundary (position 0 or
preceded by a non-word-context char). If no such `h` exists → return 0.

Pseudocode:

```go
func computeHoldback(buf string, keys []string, maxKeyLen int) int {
    bufLen := len(buf)
    maxCheck := min(bufLen, maxKeyLen-1)

    for h := maxCheck; h > 0; h-- {
        startPos := bufLen - h

        // Word-boundary gate.
        if startPos > 0 && isWordContextChar(buf[startPos-1]) {
            continue
        }

        suffix := strings.ToLower(buf[startPos:])
        for _, k := range keys {
            if strings.HasPrefix(strings.ToLower(k), suffix) && len(suffix) < len(k) {
                return h
            }
        }
    }
    return 0
}
```

**What the framework does with the holdback:**

When we return `stream_holdback_chars[i] = h`, the LiteLLM framework
computes the safe-to-emit portion as `mutated_text[:len(mutated_text)-h]`
and holds the last `h` chars back until the next sample (or end-of-
stream, when we force `h = 0`).

**Idempotency requirement:** we must be safe against LiteLLM calling us
with the same accumulated text twice (retries, or overlapping samples).
Rewriting `T[0:N]` and later `T[0:N+delta]` must produce consistent
prefixes — the mapping's stability guarantees this, because entries
never change after first assignment.

## Multi-turn de-duplication

The Python service tracks `processed_indices` — which message indices
have already been pseudonymized in an earlier turn — to avoid
re-processing the same messages.

**In the HTTP model this becomes moot** for a simple reason: LiteLLM
gives us `texts[]` after already assembling them from `structured_messages`.
On a multi-turn conversation, ALL prior messages are re-sent by the
client, which means we re-see already-pseudonymized text. That's not a
problem because:

1. Already-pseudonymized text contains our pseudonyms, not the original
   names — Presidio may still detect them (fictional names are still
   names), but our replacer is idempotent: applying the same mapping
   twice to text that's already substituted is a no-op (the pseudonym
   IS the pseudonym, no key matches).
2. Any newly-detected entity gets a new mapping entry — additive only.

**Verify with a test:** feed the same 3-turn history twice, ensure the
mapping state is identical after both runs.

## Failure modes for the text pipeline

- **Presidio 5xx / timeout / connect error** on `input_type=request` →
  return HTTP 502 with `{"error": "presidio_unreachable"}` from the
  handler layer. LiteLLM's `unreachable_fallback: fail_closed` will
  block the LLM call, which is exactly what we want.
- **Presidio 5xx** on `input_type=response` → log ERROR, return HTTP 200
  with `{"action": "NONE"}`. Losing a reversal is preferable to blocking
  a successful LLM response.
- **Redis unreachable** on request → same as Presidio unreachable
  (502).
- **Redis unreachable** on response → same as Presidio on response
  (`NONE`).
- **Pool exhausted (all synthetic slots also taken)** → this is
  theoretically impossible (synthetic pool is unbounded) but if it
  happens due to a bug, return HTTP 500 and let the operator investigate.
