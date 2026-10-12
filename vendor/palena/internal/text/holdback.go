// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"strings"
	"unicode/utf8"
)

// ComputeHoldback returns the number of trailing characters in `buf`
// the framework should hold back before emitting to the client. The
// held-back suffix is a proper prefix of at least one key in `keys`
// AND starts at a word boundary — so if we're in the middle of
// streaming "Thomas We" and "Thomas Weber" is a known pseudonym, we
// hold "Thomas We" back for the next sample.
//
// See context/PSEUDONYMIZATION_ALGORITHM.md § "Streaming holdback".
//
// Byte-level: everything is counted in RUNES, not bytes, so the return
// value is safe for LiteLLM to slice off in one step. When callers
// need a byte offset instead, use HoldbackBytes.
func ComputeHoldback(buf string, keys []string) int {
	if buf == "" || len(keys) == 0 {
		return 0
	}
	maxKeyLen := 0
	loweredKeys := make([]string, len(keys))
	for i, k := range keys {
		loweredKeys[i] = strings.ToLower(k)
		if n := utf8.RuneCountInString(k); n > maxKeyLen {
			maxKeyLen = n
		}
	}
	if maxKeyLen < 2 {
		return 0
	}
	bufRunes := []rune(buf)
	maxCheck := len(bufRunes)
	if maxKeyLen-1 < maxCheck {
		maxCheck = maxKeyLen - 1
	}

	for h := maxCheck; h > 0; h-- {
		startPos := len(bufRunes) - h
		if startPos > 0 && isWordContextRune(bufRunes[startPos-1]) {
			// Cannot start a match here — regex would never anchor.
			continue
		}
		suffix := strings.ToLower(string(bufRunes[startPos:]))
		for _, k := range loweredKeys {
			if len(suffix) < len(k) && strings.HasPrefix(k, suffix) {
				return h
			}
		}
	}
	return 0
}
