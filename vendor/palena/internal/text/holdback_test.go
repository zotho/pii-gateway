// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComputeHoldback_ProperPrefixAtBoundary(t *testing.T) {
	// "Thomas We" is a proper prefix of "Thomas Weber"; the boundary
	// before "T" is either start-of-string or a space, so it's a
	// valid holdback point.
	keys := []string{"Thomas Weber"}
	got := ComputeHoldback("Hi from Thomas We", keys)
	require.Equal(t, len("Thomas We"), got)
}

func TestComputeHoldback_NoPrefixMatchReturnsZero(t *testing.T) {
	got := ComputeHoldback("Hi from Alice yesterday", []string{"Thomas Weber"})
	require.Equal(t, 0, got)
}

func TestComputeHoldback_FullKeyIsNotHeldBack(t *testing.T) {
	// Exact match should be replaced, not held back.
	got := ComputeHoldback("Hi from Thomas Weber", []string{"Thomas Weber"})
	require.Equal(t, 0, got)
}

func TestComputeHoldback_MidWordSuffixIgnored(t *testing.T) {
	// "ce" is a prefix of "cedar" but appears mid-word ("nice"),
	// which the word-boundary check rejects.
	got := ComputeHoldback("that's nice", []string{"cedar"})
	require.Equal(t, 0, got)
}

func TestComputeHoldback_EmptyInputsReturnZero(t *testing.T) {
	require.Equal(t, 0, ComputeHoldback("", []string{"Alpha"}))
	require.Equal(t, 0, ComputeHoldback("something", nil))
	require.Equal(t, 0, ComputeHoldback("something", []string{}))
}

func TestComputeHoldback_LongestValidSuffixWins(t *testing.T) {
	// Buffer ends with "The Ac". Keys include "Acme" (matches "Ac")
	// and "Acmenator" (also matches "Ac"). Should return 2 either
	// way — the longest suffix that's a proper prefix of ANY key.
	got := ComputeHoldback("The Ac", []string{"Acme", "Acmenator"})
	require.Equal(t, 2, got)
}

func TestComputeHoldback_CaseInsensitive(t *testing.T) {
	got := ComputeHoldback("hi from thomas we", []string{"Thomas Weber"})
	require.Equal(t, len("thomas we"), got)
}

func TestComputeHoldback_TrailingSpaceMeansZero(t *testing.T) {
	// After a space, the space itself isn't a word char and no key
	// starts with a space.
	got := ComputeHoldback("The Acme ", []string{"Acme"})
	require.Equal(t, 0, got)
}

func TestComputeHoldback_ShortKeyIgnored(t *testing.T) {
	// Keys of length 1 have no proper prefix, so no holdback ever.
	require.Equal(t, 0, ComputeHoldback("hello", []string{"h"}))
}
