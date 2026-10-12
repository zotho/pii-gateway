// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPools_AssignFromPool(t *testing.T) {
	p := NewPools(map[string][]string{
		"PERSON": {"Alice", "Bob", "Carol"},
	})
	used := map[string]struct{}{}
	got := p.Assign("PERSON", used, "")
	require.Equal(t, "Alice", got)
	used["Alice"] = struct{}{}
	require.Equal(t, "Bob", p.Assign("PERSON", used, ""))
}

func TestPools_SyntheticFallbackWhenExhausted(t *testing.T) {
	p := NewPools(map[string][]string{"PERSON": {"Alice"}})
	used := map[string]struct{}{"Alice": {}}
	require.Equal(t, "Person-1", p.Assign("PERSON", used, ""))
	used["Person-1"] = struct{}{}
	require.Equal(t, "Person-2", p.Assign("PERSON", used, ""))
}

func TestPools_UnknownEntityTypeUsesTitleCase(t *testing.T) {
	p := NewPools(map[string][]string{})
	used := map[string]struct{}{}
	require.Equal(t, "Nrp-1", p.Assign("NRP", used, ""))
}

func TestPools_WhitespaceEntriesStripped(t *testing.T) {
	p := NewPools(map[string][]string{"PERSON": {"  Alice  ", "", " Bob"}})
	used := map[string]struct{}{}
	require.Equal(t, "Alice", p.Assign("PERSON", used, ""))
	used["Alice"] = struct{}{}
	require.Equal(t, "Bob", p.Assign("PERSON", used, ""))
}

func TestPools_EmptyPoolExhaustsToSynthetic(t *testing.T) {
	p := NewPools(map[string][]string{})
	require.Equal(t, "Person-1", p.Assign("PERSON", map[string]struct{}{}, ""))
}

func TestPools_Assign_SkipsReserved(t *testing.T) {
	p := NewPools(map[string][]string{"PERSON": {"Alice", "Bob"}})
	// "alice" already appears in the surrounding text → skip it to avoid a
	// reverse-mapping collision, pick Bob instead.
	got := p.Assign("PERSON", map[string]struct{}{}, "meeting with alice tomorrow")
	require.Equal(t, "Bob", got)
}

func TestPools_AssignDeterministic_StableForSameReal(t *testing.T) {
	p := NewPools(map[string][]string{"PERSON": {"Alice", "Bob", "Carol", "Dave"}})
	secret := []byte("s3cr3t")
	first := p.AssignDeterministic("PERSON", "Real Person", map[string]struct{}{}, "", secret)
	// Same real (case-insensitive) → same pseudonym, independent of session state.
	again := p.AssignDeterministic("PERSON", "real person", map[string]struct{}{}, "", secret)
	require.Equal(t, first, again)
}

func TestPools_AssignDeterministic_ProbesOnCollision(t *testing.T) {
	p := NewPools(map[string][]string{"PERSON": {"Alice", "Bob"}})
	secret := []byte("k")
	slot := p.AssignDeterministic("PERSON", "X", map[string]struct{}{}, "", secret)
	// Mark that slot taken; the same hash must probe forward to a free name.
	used := map[string]struct{}{slot: {}}
	next := p.AssignDeterministic("PERSON", "X", used, "", secret)
	require.NotEqual(t, slot, next)
}
