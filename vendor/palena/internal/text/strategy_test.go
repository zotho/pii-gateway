// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func newStrategizer(overrides map[string]string, def string) *Strategizer {
	return NewStrategizer(StrategizerConfig{
		Pools:     NewPools(map[string][]string{"PERSON": {"Alpha", "Beta"}, "ORGANIZATION": {"OrgOne"}}),
		Overrides: overrides,
		Default:   def,
	})
}

func TestStrategyFor_Defaults(t *testing.T) {
	s := newStrategizer(nil, "token")
	require.Equal(t, StrategyPool, s.StrategyFor("PERSON"))
	require.Equal(t, StrategyPool, s.StrategyFor("ORGANIZATION"))
	require.Equal(t, StrategyPool, s.StrategyFor("LOCATION"))
	require.Equal(t, StrategyToken, s.StrategyFor("CREDIT_CARD"))
	require.Equal(t, StrategyToken, s.StrategyFor("US_SSN"))
}

func TestStrategyFor_Overrides(t *testing.T) {
	s := newStrategizer(map[string]string{"PERSON": "token", "CREDIT_CARD": "pool"}, "token")
	require.Equal(t, StrategyToken, s.StrategyFor("PERSON"))     // override beats nominal default
	require.Equal(t, StrategyPool, s.StrategyFor("CREDIT_CARD")) // override beats token default
}

func TestStrategyFor_DefaultPool(t *testing.T) {
	s := newStrategizer(nil, "pool")
	require.Equal(t, StrategyPool, s.StrategyFor("CREDIT_CARD"))
}

func TestAssign_PoolForPerson(t *testing.T) {
	s := newStrategizer(nil, "token")
	got := s.Assign("PERSON", "Alice Johnson", map[string]string{}, "")
	require.Equal(t, "Alpha", got)
}

func TestAssign_TokenForStructured(t *testing.T) {
	s := newStrategizer(nil, "token")
	scratch := map[string]string{}
	c1 := s.Assign("CREDIT_CARD", "4111111111111111", scratch, "")
	require.Equal(t, "<CREDIT_CARD_1>", c1)
	scratch["4111111111111111"] = c1
	c2 := s.Assign("CREDIT_CARD", "5555444433332222", scratch, "")
	require.Equal(t, "<CREDIT_CARD_2>", c2)
	// A different type gets its own counter.
	scratch["5555444433332222"] = c2
	ssn := s.Assign("US_SSN", "078-05-1120", scratch, "")
	require.Equal(t, "<US_SSN_1>", ssn)
}

func TestAssign_TokenSkipsReserved(t *testing.T) {
	s := newStrategizer(nil, "token")
	// The input already contains "<CREDIT_CARD_1>"; a fresh card must skip it.
	got := s.Assign("CREDIT_CARD", "4111111111111111", map[string]string{}, "ref <credit_card_1> here")
	require.Equal(t, "<CREDIT_CARD_2>", got)
}

func TestStrategizer_Deterministic_StableAcrossScratch(t *testing.T) {
	s := NewStrategizer(StrategizerConfig{
		Pools:               NewPools(map[string][]string{"PERSON": {"Alpha", "Beta", "Gamma", "Delta"}}),
		Default:             "pool",
		DeterministicSecret: "key",
	})
	a := s.Assign("PERSON", "Alice Johnson", map[string]string{}, "")
	b := s.Assign("PERSON", "Alice Johnson", map[string]string{}, "")
	require.Equal(t, a, b) // deterministic: same real → same pseudonym
}

func TestTokenPrefix_NormalizesCustomTypes(t *testing.T) {
	require.Equal(t, "CREDIT_CARD", tokenPrefix("CREDIT_CARD"))
	require.Equal(t, "INSURANCE_ID", tokenPrefix("insurance id"))
	require.Equal(t, "ID_CARD", tokenPrefix("id-card"))
	require.Equal(t, "PII", tokenPrefix("!!!"))
}

func TestIsToken(t *testing.T) {
	require.True(t, isToken("<CREDIT_CARD_1>"))
	require.True(t, isToken("<US_SSN_42>"))
	require.False(t, isToken("Acme Corp"))
	require.False(t, isToken("<not a token>"))
	require.False(t, isToken("CREDIT_CARD_1"))
}

// Token reversal must NOT case-transform the real value (the regression
// this guards: an all-uppercase token uppercasing a lowercase email).
func TestReplacer_TokenReverseKeepsRealValueVerbatim(t *testing.T) {
	r := NewReplacer()
	reverse := map[string]string{
		"<EMAIL_ADDRESS_1>": "alice@acme.com",
		"<CREDIT_CARD_1>":   "4111111111111111",
	}
	got := r.Replace("Send it to <EMAIL_ADDRESS_1> and charge <CREDIT_CARD_1>.", reverse)
	require.Equal(t, "Send it to alice@acme.com and charge 4111111111111111.", got)
}

func TestReplacer_TokenForwardAndReverseRoundTrip(t *testing.T) {
	r := NewReplacer()
	forward := map[string]string{"4111111111111111": "<CREDIT_CARD_1>"}
	masked := r.Replace("Charge card 4111111111111111 now.", forward)
	require.Equal(t, "Charge card <CREDIT_CARD_1> now.", masked)

	reverse := map[string]string{"<CREDIT_CARD_1>": "4111111111111111"}
	require.Equal(t, "Charge card 4111111111111111 now.", r.Replace(masked, reverse))
}

// Regression: forward-replacing a lowercase real value with a token must
// emit the token VERBATIM (uppercase), not lowercased to match the source.
func TestReplacer_TokenForwardNotCaseFolded(t *testing.T) {
	r := NewReplacer()
	forward := map[string]string{"alice@acme.com": "<EMAIL_ADDRESS_1>"}
	got := r.Replace("Email alice@acme.com please.", forward)
	require.Equal(t, "Email <EMAIL_ADDRESS_1> please.", got)
}
