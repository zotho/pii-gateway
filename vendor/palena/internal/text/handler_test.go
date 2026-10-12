// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
)

type stubAnalyzer struct {
	detections []presidio.Detection
	err        error
	calls      int
	languages  []string
}

func (s *stubAnalyzer) Analyze(_ context.Context, _ string, _ []string, language string) ([]presidio.Detection, error) {
	s.calls++
	s.languages = append(s.languages, language)
	return s.detections, s.err
}

func newTestHandler(t *testing.T, stub *stubAnalyzer) (*Handler, mapping.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{
		SessionTTL: time.Minute, Timeout: time.Second,
	}, nil)
	h := NewHandler(HandlerConfig{
		Analyzer: stub,
		Store:    store,
		Pools:    NewPools(map[string][]string{"PERSON": {"Alpha", "Beta"}, "ORGANIZATION": {"OrgOne", "OrgTwo"}}),
		Entities: []string{"PERSON", "ORGANIZATION"},
	})
	return h, store
}

// newDecomposingHandler uses two-token pool names so component
// decomposition has something to align against.
func newDecomposingHandler(t *testing.T, stub *stubAnalyzer) (*Handler, mapping.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{
		SessionTTL: time.Minute, Timeout: time.Second,
	}, nil)
	h := NewHandler(HandlerConfig{
		Analyzer:             stub,
		Store:                store,
		Pools:                NewPools(map[string][]string{"PERSON": {"Jordan Avery", "Taylor Morgan"}}),
		Entities:             []string{"PERSON"},
		DecomposePersonNames: true,
	})
	return h, store
}

// newStructuredHandler enables token substitution for structured PII.
func newStructuredHandler(t *testing.T, stub *stubAnalyzer) (*Handler, mapping.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	strat := NewStrategizer(StrategizerConfig{
		Pools:   NewPools(map[string][]string{"PERSON": {"Jordan Avery"}}),
		Default: "token",
	})
	h := NewHandler(HandlerConfig{
		Analyzer:    stub,
		Store:       store,
		Strategizer: strat,
		Entities:    []string{"PERSON", "CREDIT_CARD", "US_SSN"},
	})
	return h, store
}

func TestHandler_StructuredPII_TokenizedAndReversed(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "PERSON", Text: "Alice Johnson", Score: 0.9},
		{EntityType: "CREDIT_CARD", Text: "4111111111111111", Score: 1.0},
		{EntityType: "US_SSN", Text: "078-05-1120", Score: 0.95},
	}}
	h, _ := newStructuredHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(),
		"Charge Alice Johnson card 4111111111111111 SSN 078-05-1120.", "s1")
	require.NoError(t, err)
	// Person -> pool name; structured PII -> tokens.
	require.Equal(t, "Charge Jordan Avery card <CREDIT_CARD_1> SSN <US_SSN_1>.", res.Text)

	// Reversal restores all of them verbatim.
	got, err := h.Reverse(context.Background(),
		"Confirmed <CREDIT_CARD_1> for Jordan Avery, SSN <US_SSN_1>.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Confirmed 4111111111111111 for Alice Johnson, SSN 078-05-1120.", got)
}

// newAllowListHandler enables an allow-list of never-pseudonymized terms.
func newAllowListHandler(t *testing.T, stub *stubAnalyzer, allow map[string]struct{}) (*Handler, mapping.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	h := NewHandler(HandlerConfig{
		Analyzer:  stub,
		Store:     store,
		Pools:     NewPools(map[string][]string{"ORGANIZATION": {"Acme Corp"}}),
		Entities:  []string{"ORGANIZATION"},
		AllowList: allow,
	})
	return h, store
}

func TestHandler_AllowList_NotPseudonymized(t *testing.T) {
	// "SSN" is a common word the NER over-tags as ORGANIZATION; allow-list it.
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "ORGANIZATION", Text: "Northwind Labs", Score: 0.9},
		{EntityType: "ORGANIZATION", Text: "SSN", Score: 0.6},
	}}
	h, _ := newAllowListHandler(t, stub, map[string]struct{}{"ssn": {}})
	res, err := h.Pseudonymize(context.Background(), "The SSN for Northwind Labs is on file.", "s1")
	require.NoError(t, err)
	// Northwind Labs is pooled; the allow-listed "SSN" stays verbatim.
	require.Equal(t, "The SSN for Acme Corp is on file.", res.Text)
	require.Equal(t, 1, res.EntitiesDetected)
}

// A user who literally types a token like "<CREDIT_CARD_1>" must not collide
// with a real value we tokenize in the same text — collision hardening.
func TestHandler_CollisionReservedTokenAvoided(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "CREDIT_CARD", Text: "4111111111111111", Score: 1.0},
	}}
	h, _ := newStructuredHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(),
		"See ref <CREDIT_CARD_1>; charge card 4111111111111111.", "s1")
	require.NoError(t, err)
	// Real card must skip the reserved index 1 and become _2.
	require.Equal(t, "See ref <CREDIT_CARD_1>; charge card <CREDIT_CARD_2>.", res.Text)
}

func TestHandler_HigherScoreClassificationWins(t *testing.T) {
	// Same span reported as both CREDIT_CARD (1.0) and ORGANIZATION (0.85);
	// the credit-card classification must win so it tokenizes, not pools.
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "ORGANIZATION", Text: "4111111111111111", Score: 0.85},
		{EntityType: "CREDIT_CARD", Text: "4111111111111111", Score: 1.0},
	}}
	h, _ := newStructuredHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(), "Card 4111111111111111.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Card <CREDIT_CARD_1>.", res.Text)
}

func TestHandler_Decompose_FullThenBareFirstNameReverses(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "PERSON", Text: "Alice Johnson", Score: 0.9},
	}}
	h, _ := newDecomposingHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(), "Please book Alice Johnson.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Please book Jordan Avery.", res.Text)

	// Model shortens to just the first name → must reverse to "Alice".
	got, err := h.Reverse(context.Background(), "Jordan will travel.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Alice will travel.", got)

	// And the full pseudonym still reverses to the full real name.
	got, err = h.Reverse(context.Background(), "Jordan Avery confirmed.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Alice Johnson confirmed.", got)
}

func TestHandler_Decompose_BareFirstNameLaterTurnStaysConsistent(t *testing.T) {
	// Turn 1: full name introduces the mapping (+ components).
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "PERSON", Text: "Alice Johnson", Score: 0.9},
	}}
	h, _ := newDecomposingHandler(t, stub)
	_, err := h.Pseudonymize(context.Background(), "Alice Johnson is the client.", "s1")
	require.NoError(t, err)

	// Turn 2: a later message references just "Alice". Presidio detects
	// it as PERSON; it must reuse "Jordan", NOT get a fresh pseudonym.
	stub.detections = []presidio.Detection{{EntityType: "PERSON", Text: "Alice", Score: 0.9}}
	res, err := h.Pseudonymize(context.Background(), "Alice prefers mornings.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Jordan prefers mornings.", res.Text)
	require.Equal(t, 0, res.NewMappingsCreated, "bare first name reuses the component mapping")
}

func TestHandler_Decompose_Disabled_NoComponentMappings(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "PERSON", Text: "Alice Johnson", Score: 0.9},
	}}
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	h := NewHandler(HandlerConfig{
		Analyzer:             stub,
		Store:                store,
		Pools:                NewPools(map[string][]string{"PERSON": {"Jordan Avery"}}),
		Entities:             []string{"PERSON"},
		DecomposePersonNames: false,
	})
	_, err := h.Pseudonymize(context.Background(), "Alice Johnson is here.", "s1")
	require.NoError(t, err)
	// Bare first name does not reverse when decomposition is off.
	got, err := h.Reverse(context.Background(), "Jordan is here.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Jordan is here.", got)
}

func TestHandler_Pseudonymize_HappyPath(t *testing.T) {
	stub := &stubAnalyzer{
		detections: []presidio.Detection{
			{EntityType: "ORGANIZATION", Text: "Novartis", Score: 0.9},
			{EntityType: "PERSON", Text: "Thomas Weber", Score: 0.85},
		},
	}
	h, _ := newTestHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(), "Novartis hired Thomas Weber.", "s1")
	require.NoError(t, err)
	require.Equal(t, "OrgOne hired Alpha.", res.Text)
	require.Equal(t, 2, res.EntitiesDetected)
	require.Equal(t, 2, res.NewMappingsCreated)
	require.Equal(t, 2, res.SessionMappingSize)
}

func TestHandler_Pseudonymize_ExistingMappingReused(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "ORGANIZATION", Text: "Novartis", Score: 0.9},
	}}
	h, store := newTestHandler(t, stub)
	_, err := store.AddMappings(context.Background(), "s1", map[string]string{"Novartis": "PreAssigned"})
	require.NoError(t, err)

	res, err := h.Pseudonymize(context.Background(), "Novartis reports profit.", "s1")
	require.NoError(t, err)
	require.Equal(t, "PreAssigned reports profit.", res.Text)
	require.Equal(t, 0, res.NewMappingsCreated)
}

func TestHandler_Pseudonymize_NoEntities(t *testing.T) {
	stub := &stubAnalyzer{}
	h, _ := newTestHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(), "a quiet Sunday", "s1")
	require.NoError(t, err)
	require.Equal(t, "a quiet Sunday", res.Text)
	require.Equal(t, 0, res.EntitiesDetected)
}

func TestHandler_Pseudonymize_EmptyText(t *testing.T) {
	stub := &stubAnalyzer{}
	h, _ := newTestHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(), "", "s1")
	require.NoError(t, err)
	require.Equal(t, "", res.Text)
	require.Equal(t, 0, stub.calls, "empty input must skip Presidio")
}

func TestHandler_Pseudonymize_AnalyzerFailurePropagates(t *testing.T) {
	stub := &stubAnalyzer{err: errors.New("boom")}
	h, _ := newTestHandler(t, stub)
	_, err := h.Pseudonymize(context.Background(), "hello", "s1")
	require.Error(t, err)
}

func TestHandler_Reverse(t *testing.T) {
	stub := &stubAnalyzer{}
	h, store := newTestHandler(t, stub)
	_, err := store.AddMappings(context.Background(), "s1", map[string]string{"Novartis": "Alpha"})
	require.NoError(t, err)

	got, err := h.Reverse(context.Background(), "Alpha will hire.", "s1")
	require.NoError(t, err)
	require.Equal(t, "Novartis will hire.", got)
}

func TestHandler_Reverse_NoMappingIsPassthrough(t *testing.T) {
	stub := &stubAnalyzer{}
	h, _ := newTestHandler(t, stub)
	got, err := h.Reverse(context.Background(), "nothing to change", "s1")
	require.NoError(t, err)
	require.Equal(t, "nothing to change", got)
}

func TestHandler_LongestFirstAssignment(t *testing.T) {
	// Both "John" and "John Mueller" arrive from Presidio in the same call.
	// Longest first ensures John Mueller is assigned before John, so they
	// don't share a pseudonym.
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "PERSON", Text: "John", Score: 0.9},
		{EntityType: "PERSON", Text: "John Mueller", Score: 0.9},
	}}
	h, _ := newTestHandler(t, stub)
	res, err := h.Pseudonymize(context.Background(), "John Mueller and John.", "s1")
	require.NoError(t, err)
	require.Equal(t, 2, res.NewMappingsCreated)
	// Alpha and Beta both assigned; either order OK, key point is
	// they differ and John Mueller resolves first.
	require.Contains(t, res.Text, "and ")
	require.NotEqual(t, "Alpha and Alpha.", res.Text)
}

func TestHandler_MultiTurnIdempotent(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "ORGANIZATION", Text: "Novartis", Score: 0.9},
	}}
	h, _ := newTestHandler(t, stub)

	res1, err := h.Pseudonymize(context.Background(), "Novartis A", "s1")
	require.NoError(t, err)
	res2, err := h.Pseudonymize(context.Background(), "Novartis A", "s1")
	require.NoError(t, err)
	require.Equal(t, res1.Text, res2.Text)
	require.Equal(t, 0, res2.NewMappingsCreated, "second call reuses mapping")
}

// langStub returns different detections per language.
type langStub struct {
	byLang map[string][]presidio.Detection
	mu     sync.Mutex
	langs  []string
}

func (l *langStub) Analyze(_ context.Context, _ string, _ []string, language string) ([]presidio.Detection, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.langs = append(l.langs, language)
	return l.byLang[language], nil
}

func newMultiLangHandler(t *testing.T, a Analyzer, noRestore []string, entities ...string) *Handler {
	mr := miniredis.RunT(t)
	store := mapping.NewRedisStoreFromClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}),
		mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	return NewHandler(HandlerConfig{
		Analyzer:          a,
		Store:             store,
		Strategizer:       NewStrategizer(StrategizerConfig{Pools: NewPools(nil), Default: string(StrategyToken), Overrides: map[string]string{"PERSON": "token"}}),
		Entities:          entities,
		Languages:         []string{"en", "ru"},
		NoRestoreEntities: noRestore,
	})
}

// Mixed-language text: every configured language analyzes the text and the
// detections are unioned, so the Russian name is found next to the English one.
func TestPseudonymize_MixedLanguageUnion(t *testing.T) {
	text := "Hello, my colleague Николай Петров and John Smith"
	stub := &langStub{byLang: map[string][]presidio.Detection{
		"en": {{EntityType: "PERSON", Start: 44, End: 49, Score: 0.85, Text: "Smith"},
			{EntityType: "PERSON", Start: 39, End: 43, Score: 0.85, Text: "John"}},
		"ru": {{EntityType: "PERSON", Start: 20, End: 34, Score: 0.85, Text: "Николай Петров"}},
	}}
	h := newMultiLangHandler(t, stub, nil, "PERSON")
	res, err := h.Pseudonymize(context.Background(), text, "s1")
	require.NoError(t, err)
	require.Contains(t, stub.langs, "en")
	require.Contains(t, stub.langs, "ru")
	require.NotContains(t, res.Text, "Николай")
	require.NotContains(t, res.Text, "Петров")
	require.NotContains(t, res.Text, "Smith")
	require.Contains(t, res.Text, "<PERSON_")
}

// The Russian model tagging an English sentence as ORG must not swallow the
// English names found by the English model.
func TestPseudonymize_WrongScriptNERDropped(t *testing.T) {
	text := "Привет, Николай Петров. Hello, I am Nikolai Petrov and my manager"
	stub := &langStub{byLang: map[string][]presidio.Detection{
		"en": {{EntityType: "PERSON", Start: 38, End: 52, Score: 0.85, Text: "Nikolai Petrov"}},
		"ru": {{EntityType: "PERSON", Start: 8, End: 22, Score: 0.85, Text: "Николай Петров"},
			{EntityType: "ORGANIZATION", Start: 24, End: 64, Score: 0.85, Text: "Hello, I am Nikolai Petrov and my manager"}},
	}}
	h := newMultiLangHandler(t, stub, nil, "PERSON", "ORGANIZATION")
	res, err := h.Pseudonymize(context.Background(), text, "s1")
	require.NoError(t, err)
	require.NotContains(t, res.Text, "ORGANIZATION")
	require.Equal(t, "Привет, <PERSON_1>. Hello, I am <PERSON_2> and my manager", res.Text)
}

func TestMergeDetections(t *testing.T) {
	d := func(s, e int, sc float64) presidio.Detection {
		return presidio.Detection{EntityType: "PERSON", Start: s, End: e, Score: sc}
	}
	// longer span replaces the shorter overlapping one
	got := mergeDetections([][]presidio.Detection{{d(0, 7, 0.5)}, {d(0, 14, 0.4)}})
	require.Equal(t, []presidio.Detection{d(0, 14, 0.4)}, got)
	// shorter overlapping span from a later language is dropped
	got = mergeDetections([][]presidio.Detection{{d(0, 14, 0.4)}, {d(0, 7, 0.9)}})
	require.Equal(t, []presidio.Detection{d(0, 14, 0.4)}, got)
	// equal span: higher score wins
	got = mergeDetections([][]presidio.Detection{{d(0, 7, 0.4)}, {d(0, 7, 0.9)}})
	require.Equal(t, []presidio.Detection{d(0, 7, 0.9)}, got)
	// disjoint spans both kept; a long span swallows two short ones
	got = mergeDetections([][]presidio.Detection{{d(0, 3, 0.5), d(5, 8, 0.5)}, {d(10, 12, 0.5)}})
	require.Len(t, got, 3)
	got = mergeDetections([][]presidio.Detection{{d(0, 3, 0.5), d(5, 8, 0.5)}, {d(0, 8, 0.5)}})
	require.Equal(t, []presidio.Detection{d(0, 8, 0.5)}, got)
}

// No-restore entity types keep their pseudonym in responses; others are restored.
func TestReverse_NoRestoreEntities(t *testing.T) {
	text := "mail bob@example.com key AKIAABCDEFGHIJKLMNOP"
	stub := &langStub{byLang: map[string][]presidio.Detection{
		"en": {{EntityType: "EMAIL_ADDRESS", Start: 5, End: 20, Score: 1, Text: "bob@example.com"},
			{EntityType: "SECRET", Start: 25, End: 45, Score: 1, Text: "AKIAABCDEFGHIJKLMNOP"}},
	}}
	h := newMultiLangHandler(t, stub, []string{"SECRET"}, "EMAIL_ADDRESS", "SECRET")
	res, err := h.Pseudonymize(context.Background(), text, "s1")
	require.NoError(t, err)
	require.Contains(t, res.Text, "<SECRET_1>")
	require.NotContains(t, res.Text, "AKIA")
	back, err := h.Reverse(context.Background(), res.Text, "s1")
	require.NoError(t, err)
	require.Contains(t, back, "bob@example.com")
	require.Contains(t, back, "<SECRET_1>")
	st, err := h.ReverseStream(context.Background(), []string{res.Text}, "s1")
	require.NoError(t, err)
	require.Contains(t, st.Texts[0], "<SECRET_1>")
	require.Contains(t, st.Texts[0], "bob@example.com")
}

// Each entity type is held to its own threshold: a 0.4 phone match passes its
// 0.4 override while a 0.5 PERSON match fails the 0.7 global threshold.
func TestPseudonymize_PerEntityThreshold(t *testing.T) {
	mr := miniredis.RunT(t)
	store := mapping.NewRedisStoreFromClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}),
		mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	text := "Call Bob at 079 123 45 67"
	stub := &stubAnalyzer{detections: []presidio.Detection{
		{EntityType: "PERSON", Start: 5, End: 8, Score: 0.5, Text: "Bob"},
		{EntityType: "PHONE_NUMBER", Start: 12, End: 25, Score: 0.4, Text: "079 123 45 67"},
	}}
	h := NewHandler(HandlerConfig{
		Analyzer:         stub,
		Store:            store,
		Strategizer:      NewStrategizer(StrategizerConfig{Pools: NewPools(map[string][]string{"PERSON": {"Alpha"}}), Default: string(StrategyToken)}),
		Entities:         []string{"PERSON", "PHONE_NUMBER"},
		ScoreThreshold:   0.7,
		EntityThresholds: map[string]float64{"PHONE_NUMBER": 0.4},
	})
	res, err := h.Pseudonymize(context.Background(), text, "s1")
	require.NoError(t, err)
	require.Equal(t, 1, res.EntitiesDetected)
	require.Contains(t, res.Text, "Bob")
	require.NotContains(t, res.Text, "079 123 45 67")
}

func TestTrimForeignEdges(t *testing.T) {
	d := presidio.Detection{EntityType: "PERSON", Start: 10, End: 24, Text: "ECHO: Коля"}
	got := trimForeignEdges("ru", d)
	require.Equal(t, "Коля", got.Text)
	require.Equal(t, 16, got.Start)
	require.Equal(t, 24, got.End)
	require.Equal(t, "", trimForeignEdges("ru", presidio.Detection{Text: "Hello world"}).Text)
	same := presidio.Detection{Start: 1, End: 15, Text: "Николай Петров"}
	require.Equal(t, same, trimForeignEdges("ru", same))
	// mid-span foreign word is kept (e.g. initials)
	require.Equal(t, "Иван X Петров", trimForeignEdges("ru", presidio.Detection{Text: "Иван X Петров"}).Text)
}

func TestPreferNarrowerHigherScore(t *testing.T) {
	d := func(typ string, s, e int, sc float64) presidio.Detection {
		return presidio.Detection{EntityType: typ, Start: s, End: e, Score: sc}
	}
	got := preferNarrowerHigherScore([]presidio.Detection{d("PERSON", 0, 10, 0.85), d("PERSON", 6, 10, 0.95), d("PERSON", 20, 30, 0.8)})
	require.Equal(t, []presidio.Detection{d("PERSON", 6, 10, 0.95), d("PERSON", 20, 30, 0.8)}, got)
	// narrower but not higher, or another type: kept
	got = preferNarrowerHigherScore([]presidio.Detection{d("PERSON", 0, 10, 0.9), d("PERSON", 6, 10, 0.8), d("ORGANIZATION", 6, 10, 0.99)})
	require.Len(t, got, 3)
}

func TestExtendPersonSpans(t *testing.T) {
	text := "1:# Maintainer: Nikolai Petrov\n"
	start := strings.Index(text, "Nikolai")
	dets := []presidio.Detection{{EntityType: "PERSON", Start: start, End: start + len("Nikolai"), Text: "Nikolai"}}
	out := extendPersonSpans(text, dets)
	if out[0].Text != "Nikolai Petrov" || text[out[0].Start:out[0].End] != "Nikolai Petrov" {
		t.Fatalf("got %+v", out[0])
	}
	// lowercase / digit-adjacent follower is not absorbed
	text2 := "Nikolai said hi"
	d2 := extendPersonSpans(text2, []presidio.Detection{{EntityType: "PERSON", Start: 0, End: 7, Text: "Nikolai"}})
	if d2[0].Text != "Nikolai" {
		t.Fatalf("over-extended: %+v", d2[0])
	}
}

func TestTrimAtEscapes(t *testing.T) {
	txt := `x: Nikolai Petrov\ndef hello():\n`
	d := presidio.Detection{EntityType: "PERSON", Start: 3, End: len(txt), Text: txt[3:]}
	out := trimAtEscapes([]presidio.Detection{d})
	if len(out) != 1 || out[0].Text != "Nikolai Petrov" || txt[out[0].Start:out[0].End] != "Nikolai Petrov" {
		t.Fatalf("got %+v", out)
	}
}

func TestHandler_KnownValueMaskedWhenNothingDetected(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{{EntityType: "PERSON", Start: 0, End: 8, Text: "John Doe", Score: 0.9}}}
	h, _ := newTestHandler(t, stub)
	ctx := context.Background()
	r1, err := h.Pseudonymize(ctx, "John Doe is here", "s1")
	require.NoError(t, err)
	require.NotContains(t, r1.Text, "John")
	stub.detections = nil // NER misses the same name in a new context
	r2, err := h.Pseudonymize(ctx, "1:# Maintainer: John Doe", "s1")
	require.NoError(t, err)
	require.NotContains(t, r2.Text, "John")
}

func detAt(txt, sub, typ, recognizer string) presidio.Detection {
	i := strings.Index(txt, sub)
	return presidio.Detection{EntityType: typ, Start: i, End: i + len(sub), Text: sub, Score: 0.9,
		RecognitionMetadata: &presidio.RecognitionMetadata{RecognizerName: recognizer}}
}

func TestPatternOnly_DropsNERKeepsPatterns(t *testing.T) {
	txt := "Deploy to Vercel Functions, ask Коля, invoice Acme Corporation"
	stub := &stubAnalyzer{detections: []presidio.Detection{
		detAt(txt, "Vercel Functions", "ORGANIZATION", "SpacyRecognizer"),
		detAt(txt, "Коля", "PERSON", "Russian short names"),
		detAt(txt, "Acme Corporation", "ORGANIZATION", "Known organizations"), // ORG type, but a deny-list (pattern) recognizer
	}}
	h, _ := newTestHandler(t, stub)
	ctx := context.Background()

	full, err := h.PseudonymizeOpts(ctx, txt, "full", false)
	require.NoError(t, err)
	require.NotContains(t, full.Text, "Vercel Functions")

	po, err := h.PseudonymizeOpts(ctx, txt, "po", true)
	require.NoError(t, err)
	require.Contains(t, po.Text, "Vercel Functions", "NER ORG must not be masked in pattern-only mode")
	require.NotContains(t, po.Text, "Коля", "diminutive regex PERSON must survive")
	require.NotContains(t, po.Text, "Acme Corporation")
	require.Equal(t, 2, po.EntitiesDetected)
}

func TestPatternOnly_KnownSessionValueStillMasked(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{
		detAt("John Doe is here", "John Doe", "PERSON", "SpacyRecognizer")}}
	h, _ := newTestHandler(t, stub)
	ctx := context.Background()
	r1, err := h.PseudonymizeOpts(ctx, "John Doe is here", "s1", false)
	require.NoError(t, err)
	require.NotContains(t, r1.Text, "John")
	stub.detections = []presidio.Detection{detAt("System: owner John Doe, Vercel Functions", "Vercel Functions", "ORGANIZATION", "SpacyRecognizer")}
	r2, err := h.PseudonymizeOpts(ctx, "System: owner John Doe, Vercel Functions", "s1", true)
	require.NoError(t, err)
	require.NotContains(t, r2.Text, "John")
	require.Contains(t, r2.Text, "Vercel Functions")
}

func TestPatternOnly_NoMetadataIsKept(t *testing.T) {
	stub := &stubAnalyzer{detections: []presidio.Detection{{EntityType: "PERSON", Start: 0, End: 8, Text: "John Doe", Score: 0.9}}}
	h, _ := newTestHandler(t, stub)
	r, err := h.PseudonymizeOpts(context.Background(), "John Doe is here", "s", true)
	require.NoError(t, err)
	require.NotContains(t, r.Text, "John")
}
