// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplacer_EmptyInputs(t *testing.T) {
	r := NewReplacer()
	require.Equal(t, "", r.Replace("", map[string]string{"a": "b"}))
	require.Equal(t, "hi", r.Replace("hi", nil))
	require.Equal(t, "hi", r.Replace("hi", map[string]string{}))
}

func TestReplacer_LongestFirst(t *testing.T) {
	r := NewReplacer()
	mapping := map[string]string{
		"John":         "Alpha",
		"John Mueller": "Beta Gamma",
	}
	got := r.Replace("Ping John Mueller and John today.", mapping)
	require.Equal(t, "Ping Beta Gamma and Alpha today.", got)
}

func TestReplacer_CaseInsensitivePreserving(t *testing.T) {
	r := NewReplacer()
	m := map[string]string{"Novartis": "Acme Corp"}
	cases := []struct{ in, want string }{
		{"Novartis reports profit.", "Acme Corp reports profit."},
		{"novartis reports profit.", "acme corp reports profit."},
		{"NOVARTIS reports profit.", "ACME CORP reports profit."},
		{"NoVarTis reports profit.", "Acme Corp reports profit."}, // mixed → as-is
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, r.Replace(tc.in, m), tc.in)
	}
}

func TestReplacer_Possessives(t *testing.T) {
	r := NewReplacer()
	m := map[string]string{"Novartis": "Acme Corp"}
	cases := []struct{ in, want string }{
		{"Novartis's report.", "Acme Corp's report."},
		{"Novartis’s report.", "Acme Corp’s report."}, // unicode apostrophe
		{"Novartis' report.", "Acme Corp' report."},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, r.Replace(tc.in, m), tc.in)
	}
}

func TestReplacer_WordBoundaryRejection(t *testing.T) {
	r := NewReplacer()
	m := map[string]string{"Acme": "Zenith"}
	// "Acmeologist" starts with Acme but isn't a word boundary — must not replace.
	require.Equal(t, "Acmeologist visited today.", r.Replace("Acmeologist visited today.", m))
	// Preceded by apostrophe: also blocked.
	require.Equal(t, "l'Acme is a thing.", r.Replace("l'Acme is a thing.", m))
	// Preceded by underscore: also blocked.
	require.Equal(t, "prefix_Acme item.", r.Replace("prefix_Acme item.", m))
	// Genuine boundary: replaced.
	require.Equal(t, "See Zenith Corp.", r.Replace("See Acme Corp.", m))
}

func TestReplacer_PrefixCollisionNoDoubleProcess(t *testing.T) {
	r := NewReplacer()
	m := map[string]string{
		"John Mueller": "Alpha",
		"John":         "Beta",
	}
	got := r.Replace("John Mueller Jr. asked John.", m)
	require.Equal(t, "Alpha Jr. asked Beta.", got)
}

func TestReplacer_MultipleOccurrences(t *testing.T) {
	r := NewReplacer()
	m := map[string]string{"Novartis": "Acme Corp"}
	got := r.Replace("Novartis and Novartis and NOVARTIS.", m)
	require.Equal(t, "Acme Corp and Acme Corp and ACME CORP.", got)
}

func TestReplacer_NoMatches(t *testing.T) {
	r := NewReplacer()
	m := map[string]string{"Zebra": "Yak"}
	require.Equal(t, "quiet Sunday morning", r.Replace("quiet Sunday morning", m))
}

func TestReplacer_ReverseDirection(t *testing.T) {
	// The replacer is direction-agnostic — same code path for pre / post.
	r := NewReplacer()
	m := map[string]string{"Acme Corp": "Novartis"}
	require.Equal(t, "Novartis will hire.", r.Replace("Acme Corp will hire.", m))
}

func TestReplacer_ApostropheAfterMatchIsPossessive(t *testing.T) {
	// Ensures we don't consume the possessive INTO the replacement lookup.
	r := NewReplacer()
	m := map[string]string{"Anna": "Sarah"}
	got := r.Replace("Anna's team met Anna.", m)
	require.Equal(t, "Sarah's team met Sarah.", got)
}

func TestLeftBoundary_LiteralEscape(t *testing.T) {
	text := `role: reviewer.\nMargaret Holloway and\tNikolai Petrov`
	i := strings.Index(text, "Margaret")
	if !leftBoundaryClear(text, i) {
		t.Fatal(`\n before name must be a boundary`)
	}
	if !leftBoundaryClear(text, strings.Index(text, "Nikolai")) {
		t.Fatal("tab before name must be a boundary")
	}
	if leftBoundaryClear("xMargaret", 1) {
		t.Fatal("letter before name is not a boundary")
	}
}
