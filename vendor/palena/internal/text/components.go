// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ComponentMappings derives first-name and last-name sub-mappings for a
// full person name so that later references to just the first or last
// name stay consistent with the full-name pseudonym.
//
// "Alice Johnson" -> "Jordan Avery" yields:
//
//	Alice   -> Jordan
//	Johnson -> Avery
//
// This lets a model that shortens "Jordan Avery" to "Jordan" reverse
// back to "Alice", and a later user message that says just "Alice" map
// to the same "Jordan" rather than getting a fresh, conflicting
// pseudonym (which would make the model think two different people are
// involved).
//
// Returns nil when either side has fewer than two whitespace tokens (a
// single-token real name, or a synthetic single-token pseudonym like
// "Person-1"). Only the first and last tokens are aligned; middle tokens
// are left unmapped because name shortening in practice uses the first
// or last name, not a middle name.
//
// CAVEAT: a component that is also a common noun ("Baker", "Green",
// "Cook") will over-match that noun on a case-insensitive basis. The
// result is garbled but never leaks real data (a pseudonym or an
// already-public word is what gets rewritten, not a hidden identity).
// Callers can disable decomposition entirely — see the
// DecomposePersonNames handler option.
func ComponentMappings(real, pseudo string) map[string]string {
	rTokens := strings.Fields(real)
	pTokens := strings.Fields(pseudo)
	if len(rTokens) < 2 || len(pTokens) < 2 {
		return nil
	}

	out := map[string]string{}
	addPair := func(r, p string) {
		if p == "" || !isSafeComponent(r) {
			return
		}
		if strings.EqualFold(r, p) {
			return
		}
		out[r] = p
	}

	// First name.
	addPair(rTokens[0], pTokens[0])

	// Last name. Skip when it equals the first token (degenerate
	// "John John") to avoid a redundant/conflicting entry.
	rLast := rTokens[len(rTokens)-1]
	pLast := pTokens[len(pTokens)-1]
	if !strings.EqualFold(rLast, rTokens[0]) {
		addPair(rLast, pLast)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// isSafeComponent rejects tokens that would over-match if used as a
// standalone replacement key: single initials ("J."), punctuation-only
// fragments, or anything with fewer than two letters.
func isSafeComponent(token string) bool {
	if utf8.RuneCountInString(token) < 2 {
		return false
	}
	letters := 0
	for _, r := range token {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters >= 2
}
