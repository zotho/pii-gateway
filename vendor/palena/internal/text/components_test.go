// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComponentMappings_TwoTokenName(t *testing.T) {
	got := ComponentMappings("Alice Johnson", "Jordan Avery")
	require.Equal(t, map[string]string{"Alice": "Jordan", "Johnson": "Avery"}, got)
}

func TestComponentMappings_ThreeTokenRealAlignsFirstLast(t *testing.T) {
	// Middle token ("Jane") is intentionally left unmapped.
	got := ComponentMappings("Mary Jane Watson", "Jordan Avery")
	require.Equal(t, map[string]string{"Mary": "Jordan", "Watson": "Avery"}, got)
}

func TestComponentMappings_SingleTokenRealReturnsNil(t *testing.T) {
	require.Nil(t, ComponentMappings("Alice", "Jordan Avery"))
}

func TestComponentMappings_SyntheticSingleTokenPseudoReturnsNil(t *testing.T) {
	// Synthetic fallbacks like "Person-1" have no last name to align.
	require.Nil(t, ComponentMappings("Alice Johnson", "Person-1"))
}

func TestComponentMappings_RejectsInitials(t *testing.T) {
	// "J." is a single-letter initial — must not become a match key.
	got := ComponentMappings("J. Johnson", "Jordan Avery")
	require.Equal(t, map[string]string{"Johnson": "Avery"}, got)
}

func TestComponentMappings_SkipsWhenComponentEqualsPseudo(t *testing.T) {
	// If a real component already equals its pseudonym, no self-mapping.
	got := ComponentMappings("Jordan Smith", "Jordan Avery")
	require.Equal(t, map[string]string{"Smith": "Avery"}, got)
}

func TestComponentMappings_DegenerateRepeatedNameSkipsLast(t *testing.T) {
	got := ComponentMappings("John John", "Jordan Avery")
	require.Equal(t, map[string]string{"John": "Jordan"}, got)
}
