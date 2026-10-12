// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Replacer performs word-boundary-aware, case-insensitive,
// case-preserving find-and-replace over a mapping. See
// context/PSEUDONYMIZATION_ALGORITHM.md § "Replacer rules" for the
// design and edge cases covered.
//
// The mapping is passed to Replace each call; there is no
// pre-compilation cache. The regex build is cheap relative to a
// Presidio round-trip, and per-call construction keeps the API
// stateless.
type Replacer struct{}

// NewReplacer constructs a Replacer.
func NewReplacer() *Replacer { return &Replacer{} }

// Replace rewrites every key of `mapping` found in `text` with its
// corresponding value.
//
//   - Case-insensitive match, case-preserving output: "novartis" → "acme corp",
//     "NOVARTIS" → "ACME CORP", "Novartis" → "Acme Corp".
//   - Longest-first: "John Mueller" is matched before "John".
//   - Word-boundary aware: "Acmeologist" does NOT match "Acme".
//   - Possessive-safe: "Novartis's" → "Acme Corp's" (both ASCII "'s" and
//     the Unicode "'s" apostrophe are preserved).
//
// Returns the original text unchanged if `text` or `mapping` is empty.
func (r *Replacer) Replace(text string, mapping map[string]string) string {
	if text == "" || len(mapping) == 0 {
		return text
	}

	// Sort keys longest-first for correct alternation matching under
	// RE2 (leftmost-longest across the alternation).
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return text
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})

	escaped := make([]string, len(keys))
	for i, k := range keys {
		escaped[i] = regexp.QuoteMeta(k)
	}
	// Group 1: the entity itself. RE2 has no lookbehinds so we do the
	// left-boundary check manually below.
	pattern := "(?i)(" + strings.Join(escaped, "|") + `)('s|` + "’" + `s|')?`
	re := regexp.MustCompile(pattern)

	// Case-insensitive lookup table: lowered key → original value.
	lowered := make(map[string]string, len(mapping))
	for k, v := range mapping {
		lowered[strings.ToLower(k)] = v
	}

	var b strings.Builder
	b.Grow(len(text))
	pos := 0

	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		// m[0]/m[1] = full match, m[2]/m[3] = key group,
		// m[4]/m[5] = optional possessive group (may be -1/-1).
		start, end := m[0], m[1]
		keyStart, keyEnd := m[2], m[3]

		// Left + right word-boundary checks. RE2 has no lookaround so
		// we do them manually. Mirrors the Python (?<![\w']) lookbehind
		// and (?=\W|$|'s|’s|') lookahead.
		if !leftBoundaryClear(text, start) || !rightBoundaryClear(text, end) {
			// Skip: leave the whole match verbatim.
			b.WriteString(text[pos:end])
			pos = end
			continue
		}

		source := text[keyStart:keyEnd]
		replacement, ok := lowered[strings.ToLower(source)]
		if !ok {
			// Should not happen — the alternation is built from keys.
			b.WriteString(text[pos:end])
			pos = end
			continue
		}

		// Emit anything before the match verbatim.
		b.WriteString(text[pos:start])
		b.WriteString(matchCase(source, replacement))
		// Preserve any possessive tail captured after the key.
		if m[4] != -1 {
			b.WriteString(text[m[4]:m[5]])
		}
		pos = end
	}
	b.WriteString(text[pos:])
	return b.String()
}

// leftBoundaryClear returns true if the position `start` in `text` is a
// left word boundary: either at the start of the string, or preceded
// by a rune that isn't a letter, digit, underscore, or apostrophe.
//
// Mirrors the Python (?<![\w']) lookbehind.
func leftBoundaryClear(text string, start int) bool {
	if start == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:start])
	if r == utf8.RuneError {
		return true
	}
	if start >= 2 && text[start-2] == '\\' && strings.IndexByte("ntrbf", text[start-1]) >= 0 {
		// literal escape sequence before the name ("...py.\nMargaret" in JSON-escaped
		// tool output): the escape letter is not part of the word
		return true
	}
	if r == '\'' || r == '’' {
		// an apostrophe is only part of a word ("O'Brien", "l'Ivanov"); a quote
		// opening a string ('Name' in a Python repr / SQL / CSV) is a boundary
		if start-1 <= 0 {
			return true
		}
		p, _ := utf8.DecodeLastRuneInString(text[:start-utf8.RuneLen(r)])
		return !(unicode.IsLetter(p) || unicode.IsDigit(p) || p == '_')
	}
	return !isWordContextRune(r)
}

// rightBoundaryClear returns true if the position `end` in `text` is a
// right word boundary: either at the end of the string, or followed by
// a rune that isn't a letter, digit, or underscore. Apostrophes are
// allowed because "'s" is a valid trailing possessive already captured
// by the regex.
func rightBoundaryClear(text string, end int) bool {
	if end >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[end:])
	if r == utf8.RuneError {
		return true
	}
	if r == '_' {
		return false
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

func isWordContextRune(r rune) bool {
	if r == '_' || r == '\'' || r == '’' {
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// matchCase returns `replacement` with the casing of `source` applied.
// Three-way rule (matches Python _match_case):
//   - all letters upper → UPPER
//   - all letters lower → lower
//   - anything else     → replacement as-is
//
// Non-letter characters in `source` are ignored when judging the pattern.
func matchCase(source, replacement string) string {
	// Token pseudonyms (<CREDIT_CARD_1>) are exact stand-ins — never
	// case-transform when either side is a token:
	//   - forward (real -> token): emit the token verbatim, don't lowercase
	//     it just because the real value (e.g. an email) was lowercase;
	//   - reverse (token -> real): emit the real value verbatim, don't
	//     uppercase it to match the token's case.
	if isToken(source) || isToken(replacement) {
		return replacement
	}
	hasLetter := false
	allUpper := true
	allLower := true
	for _, r := range source {
		if !unicode.IsLetter(r) {
			continue
		}
		hasLetter = true
		if !unicode.IsUpper(r) {
			allUpper = false
		}
		if !unicode.IsLower(r) {
			allLower = false
		}
	}
	if !hasLetter {
		return replacement
	}
	switch {
	case allUpper:
		return strings.ToUpper(replacement)
	case allLower:
		return strings.ToLower(replacement)
	}
	return replacement
}
