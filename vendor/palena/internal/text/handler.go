// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"context"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/audit"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/language"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
)

// Analyzer is the subset of *presidio.Analyzer the text handler needs.
// A tiny interface lets tests inject a stub instead of running a full
// httptest server.
type Analyzer interface {
	Analyze(ctx context.Context, text string, entities []string, language string) ([]presidio.Detection, error)
}

// Handler orchestrates: analyze → assign pseudonyms → persist → rewrite.
// It is the equivalent of the Python TextHandler.
type Handler struct {
	analyzer Analyzer
	store    mapping.Store
	strat    *Strategizer
	replacer *Replacer
	entities []string
	language language.Detector
	// languages, when it holds several codes, makes every text be analyzed
	// in ALL of them and the detections unioned (mixed-language text).
	languages []string
	// noRestore: entity types whose pseudonyms are never reversed in
	// responses (e.g. SECRET): the token prefix "<TYPE_" is matched.
	noRestore []string
	decompose bool
	allowList map[string]struct{} // lower-cased terms never pseudonymized
	// commonWords: lower-cased dictionary words; NER-derived single-word PERSON/ORGANIZATION hits on them are dropped
	commonWords map[string]struct{}
	// threshold / thresholds: the minimum score per entity type (overrides,
	// else the global one). Zero threshold means Presidio's own filtering is
	// all there is.
	threshold  float64
	thresholds map[string]float64
}

// HandlerConfig groups constructor parameters.
type HandlerConfig struct {
	Analyzer Analyzer
	Store    mapping.Store
	// Pools is used to build a default pool-only Strategizer when
	// Strategizer is nil (keeps older callers/tests working).
	Pools *Pools
	// Strategizer selects the per-entity substitution strategy. When nil,
	// a pool-only strategizer is built from Pools.
	Strategizer *Strategizer
	Entities    []string
	// Language is the one language every text is analyzed in. Ignored when
	// Languages is set.
	Language string
	// Languages, when it holds several codes, classifies each text among
	// them and analyzes it in its own language (see package language).
	Languages []string
	// NoRestoreEntities lists entity types (e.g. "SECRET") whose pseudonyms
	// are NOT reversed in responses. Works with the token strategy
	// (pseudonym "<TYPE_n>"); other pseudonym forms are unaffected.
	NoRestoreEntities []string
	// DecomposePersonNames, when true, registers first-name and
	// last-name sub-mappings for multi-token PERSON entities so that
	// bare first/last-name references stay consistent with the
	// full-name pseudonym. See ComponentMappings.
	DecomposePersonNames bool
	// AllowList holds lower-cased terms that are never pseudonymized even
	// when Presidio detects them. See Config.AllowListSet.
	AllowList map[string]struct{}
	// CommonWords: see Config.CommonWordsSet. nil disables the filter.
	CommonWords map[string]struct{}
	// ScoreThreshold is the minimum score a detection needs; EntityThresholds
	// overrides it per entity type. Presidio is asked for the lowest of them
	// (see Config.MinScoreThreshold), so each type's own is applied here.
	ScoreThreshold   float64
	EntityThresholds map[string]float64
}

// NewHandler builds a text handler. All fields are required except
// Language/Languages, which default to "en".
func NewHandler(cfg HandlerConfig) *Handler {
	codes := cfg.Languages
	if len(codes) == 0 && cfg.Language != "" {
		codes = []string{cfg.Language}
	}
	lang := language.New(codes)
	strat := cfg.Strategizer
	if strat == nil {
		// Backward-compatible default: everything uses the pool strategy.
		strat = NewStrategizer(StrategizerConfig{Pools: cfg.Pools, Default: string(StrategyPool)})
	}
	return &Handler{
		analyzer:    cfg.Analyzer,
		store:       cfg.Store,
		strat:       strat,
		replacer:    NewReplacer(),
		entities:    cfg.Entities,
		language:    lang,
		languages:   codes,
		noRestore:   cfg.NoRestoreEntities,
		decompose:   cfg.DecomposePersonNames,
		allowList:   cfg.AllowList,
		commonWords: cfg.CommonWords,

		threshold:  cfg.ScoreThreshold,
		thresholds: cfg.EntityThresholds,
	}
}

// Replacer returns the underlying text replacer (used by streaming).
func (h *Handler) Replacer() *Replacer { return h.replacer }

// PseudonymizeResult carries the rewritten text plus counters.
type PseudonymizeResult struct {
	Text               string
	EntitiesDetected   int
	NewMappingsCreated int
	EntityTypeCounts   map[audit.EntityType]int
	SessionMappingSize int
}

// Pseudonymize runs the pre-call flow for a single text string.
//
//  1. Analyze via Presidio (fail-closed).
//  2. Filter to configured entity types.
//  3. Load existing session mapping.
//  4. Assign new pseudonyms (existing entries win).
//  5. Persist the merged mapping.
//  6. Rewrite the text with the merged mapping.
func (h *Handler) Pseudonymize(ctx context.Context, text, sessionID string) (*PseudonymizeResult, error) {
	return h.PseudonymizeOpts(ctx, text, sessionID, false)
}

// PseudonymizeOpts is Pseudonymize with a "pattern-only" switch: when patternOnly is true,
// detections produced by NER-model recognizers (PERSON/ORGANIZATION/LOCATION/NRP from spaCy
// etc.) are dropped; pattern recognizers (email, phone, SECRET, deny-lists, the Russian
// diminutive regex ...) still apply, and values already in the session mapping are still
// replaced. Meant for agent system prompts, where NER only produces product-name false positives.
func (h *Handler) PseudonymizeOpts(ctx context.Context, text, sessionID string, patternOnly bool) (*PseudonymizeResult, error) {
	res := &PseudonymizeResult{
		Text:             text,
		EntityTypeCounts: map[audit.EntityType]int{},
	}
	if strings.TrimSpace(text) == "" {
		return res, nil
	}

	detections, err := h.analyze(ctx, text, patternOnly)
	if err != nil {
		return nil, err
	}
	filtered := filterByEntityType(detections, h.entities)
	filtered = h.filterScore(filtered)
	filtered = h.filterAllowList(filtered)
	res.EntitiesDetected = len(filtered)

	for _, d := range filtered {
		res.EntityTypeCounts[audit.NormalizeEntityType(d.EntityType)]++
	}

	if len(filtered) == 0 {
		merged, err := h.store.GetMapping(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		res.SessionMappingSize = len(merged)
		// no new detections, but values already known in this session must still be masked
		// (NER can miss "Святослав Иванов" in "1:# Maintainer: ..." that it caught elsewhere)
		res.Text = h.replacer.Replace(text, merged)
		return res, nil
	}

	existing, err := h.store.GetMapping(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	newMappings := h.assignNew(filtered, existing, strings.ToLower(text))
	res.NewMappingsCreated = len(newMappings)

	var merged map[string]string
	if len(newMappings) == 0 {
		merged = existing
	} else {
		merged, err = h.store.AddMappings(ctx, sessionID, newMappings)
		if err != nil {
			return nil, err
		}
	}
	res.SessionMappingSize = len(merged)
	res.Text = h.replacer.Replace(text, merged)
	return res, nil
}

// Reverse runs the post-call flow: load the reverse map and rewrite.
// Empty reverse map is a no-op (returns text unchanged).
func (h *Handler) Reverse(ctx context.Context, text, sessionID string) (string, error) {
	if text == "" {
		return text, nil
	}
	reverse, err := h.store.GetReverseMapping(ctx, sessionID)
	if err != nil {
		return "", err
	}
	reverse = h.restorable(reverse)
	if len(reverse) == 0 {
		return text, nil
	}
	return h.replacer.Replace(text, reverse), nil
}

// ReverseStreamResult carries the parallel outputs of ReverseStream.
type ReverseStreamResult struct {
	Texts     []string
	Holdbacks []int
	Modified  bool
}

// ReverseStream is the incremental_diff streaming variant of Reverse.
// Loads the reverse map once and, for each input text, returns the
// rewritten text plus a per-index holdback char count so the framework
// buffers partial pseudonyms until they fully arrive.
//
// See context/PSEUDONYMIZATION_ALGORITHM.md § "Streaming holdback".
func (h *Handler) ReverseStream(ctx context.Context, texts []string, sessionID string) (*ReverseStreamResult, error) {
	out := &ReverseStreamResult{
		Texts:     make([]string, len(texts)),
		Holdbacks: make([]int, len(texts)),
	}
	if len(texts) == 0 {
		return out, nil
	}
	reverse, err := h.store.GetReverseMapping(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	reverse = h.restorable(reverse)
	if len(reverse) == 0 {
		copy(out.Texts, texts)
		return out, nil
	}
	keys := make([]string, 0, len(reverse))
	for k := range reverse {
		keys = append(keys, k)
	}
	for i, raw := range texts {
		rewritten := h.replacer.Replace(raw, reverse)
		out.Texts[i] = rewritten
		out.Holdbacks[i] = ComputeHoldback(rewritten, keys)
		if rewritten != raw {
			out.Modified = true
		}
	}
	return out, nil
}

// assignNew selects pseudonyms for detections not already in the
// session mapping. Sorted longest-first, then alphabetical, so compound
// names get assigned before their components inside one request.
//
// reserved is the lower-cased source text; a newly-minted pseudonym is
// never allowed to be a substring of it (collision hardening).
func (h *Handler) assignNew(detections []presidio.Detection, existing map[string]string, reserved string) map[string]string {
	// De-dup by text (case-preserving) — one entity name → one mapping.
	// When the same span is classified as multiple types (e.g. a card
	// number tagged both CREDIT_CARD and, by a noisy NER, ORGANIZATION),
	// keep the higher-scoring classification so the right strategy applies.
	byText := make(map[string]presidio.Detection, len(detections))
	for _, d := range detections {
		trimmed := strings.TrimSpace(d.Text)
		if trimmed == "" {
			continue
		}
		if prev, ok := byText[trimmed]; ok && prev.Score >= d.Score {
			continue
		}
		byText[trimmed] = presidio.Detection{
			EntityType: d.EntityType,
			Text:       trimmed,
			Score:      d.Score,
		}
	}
	sorted := make([]presidio.Detection, 0, len(byText))
	for _, d := range byText {
		sorted = append(sorted, d)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if len(sorted[i].Text) != len(sorted[j].Text) {
			return len(sorted[i].Text) > len(sorted[j].Text)
		}
		return sorted[i].Text < sorted[j].Text
	})

	// Scratch mirrors what the merged mapping will be after we apply
	// this request's assignments; ensures no two new entities collide
	// on the same pseudonym.
	scratch := make(map[string]string, len(existing))
	for k, v := range existing {
		scratch[k] = v
	}

	newMap := map[string]string{}
	for _, d := range sorted {
		if _, ok := scratch[d.Text]; ok {
			continue
		}
		if hasCaseInsensitiveKey(scratch, d.Text) {
			continue
		}
		p := h.strat.Assign(d.EntityType, d.Text, scratch, reserved)
		newMap[d.Text] = p
		scratch[d.Text] = p

		// Register first/last-name sub-mappings so bare references
		// (model shortening "Jordan Avery" → "Jordan", or a later user
		// message saying just "Alice") stay consistent. Existing-wins:
		// a component already mapped for another person is left alone.
		if h.decompose && d.EntityType == "PERSON" {
			for cReal, cPseudo := range ComponentMappings(d.Text, p) {
				if _, ok := h.allowList[strings.ToLower(cReal)]; ok {
					continue // component is allow-listed (e.g. a brand surname)
				}
				if _, ok := scratch[cReal]; ok {
					continue
				}
				if hasCaseInsensitiveKey(scratch, cReal) {
					continue
				}
				newMap[cReal] = cPseudo
				scratch[cReal] = cPseudo
			}
		}
	}
	return newMap
}

func filterByEntityType(detections []presidio.Detection, allowed []string) []presidio.Detection {
	if len(allowed) == 0 {
		return detections
	}
	set := make(map[string]struct{}, len(allowed))
	for _, e := range allowed {
		set[e] = struct{}{}
	}
	out := make([]presidio.Detection, 0, len(detections))
	for _, d := range detections {
		if _, ok := set[d.EntityType]; ok {
			out = append(out, d)
		}
	}
	return out
}

// filterScore drops detections below their entity type's threshold.
func (h *Handler) filterScore(detections []presidio.Detection) []presidio.Detection {
	out := detections[:0:0]
	for _, d := range detections {
		min := h.threshold
		if t, ok := h.thresholds[d.EntityType]; ok {
			min = t
		}
		if d.Score >= min {
			out = append(out, d)
		}
	}
	return out
}

// filterAllowList drops detections whose span text is on the allow-list
// (case-insensitive). These terms are never pseudonymized — brand/product
// names, public figures, or words Presidio over-tags. No-op when the
// allow-list is empty.
func (h *Handler) filterAllowList(detections []presidio.Detection) []presidio.Detection {
	if len(h.allowList) == 0 {
		return detections
	}
	out := make([]presidio.Detection, 0, len(detections))
	for _, d := range detections {
		if allowListed(h.allowList, d.Text) && nerTypes[d.EntityType] {
			continue
		}
		if _, ok := h.allowList[strings.ToLower(strings.TrimSpace(d.Text))]; ok {
			continue
		}
		out = append(out, d)
	}
	return out
}

func hasCaseInsensitiveKey(m map[string]string, key string) bool {
	kl := strings.ToLower(key)
	for k := range m {
		if strings.ToLower(k) == kl {
			return true
		}
	}
	return false
}

// restorable drops reverse-map entries of no-restore entity types (their
// pseudonyms stay as-is in responses).
func (h *Handler) restorable(reverse map[string]string) map[string]string {
	if len(h.noRestore) == 0 || len(reverse) == 0 {
		return reverse
	}
	out := make(map[string]string, len(reverse))
outer:
	for k, v := range reverse {
		for _, t := range h.noRestore {
			if strings.HasPrefix(k, "<"+t+"_") {
				continue outer
			}
		}
		out[k] = v
	}
	return out
}

// analyze runs Presidio on text. With several configured languages the text
// is analyzed in every one of them (so "Hello, my colleague Николай Петров"
// finds both names) and the detections are unioned; see mergeDetections.
func (h *Handler) analyze(ctx context.Context, text string, patternOnly bool) ([]presidio.Detection, error) {
	langs := h.languages
	if len(langs) == 0 {
		langs = []string{""}
	}
	single := len(h.languages) <= 1
	views := []string{text}
	mask := delimMask(text)
	views = append(views, explodeViews(text)...) // same offsets; see explodeViews
	results := make([][]presidio.Detection, len(views)*len(langs))
	errs := make([]error, len(results))
	var wg sync.WaitGroup
	for vi, v := range views {
		for li, lang := range langs {
			wg.Add(1)
			go func(idx, vi int, v, lang string) {
				defer wg.Done()
				if single {
					lang = h.language.Detect(v)
				}
				dets, err := h.analyzer.Analyze(ctx, v, h.entities, lang)
				if patternOnly {
					dets = dropNER(dets)
				}
				if !single {
					dets = filterScript(lang, dets)
				}
				if vi > 0 {
					dets = splitAtMask(text, mask, dets)
				}
				results[idx], errs[idx] = dets, err
			}(vi*len(langs)+li, vi, v, lang)
		}
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err // fail closed
		}
	}
	// common-word filter runs before extendPersonSpans: "Client" must not grow into "Client Name"
	dets := filterCommonWords(h.commonWords, trimAtEscapes(preferNarrowerHigherScore(mergeDetections(results))))
	return filterNonPII(text, extendPersonSpans(text, dets)), nil
}

// dropNER removes detections that came from an NER-model recognizer.
func dropNER(dets []presidio.Detection) []presidio.Detection {
	out := dets[:0:0]
	for _, d := range dets {
		if !d.IsNER() {
			out = append(out, d)
		}
	}
	return out
}

// trimAtEscapes cuts an NER span at the first line break or literal JSON
// escape (`\n`, `\t`, `\r`, `\"`): agents embed file contents in JSON-in-JSON
// tool output, and the NER model happily swallows "Nikolai Petrov\ndef hello():"
// as one name. Empty remainders drop the detection.
func trimAtEscapes(dets []presidio.Detection) []presidio.Detection {
	out := dets[:0:0]
	for _, d := range dets {
		if nerTypes[d.EntityType] {
			cut := -1
			for i := 0; i < len(d.Text); i++ {
				c := d.Text[i]
				if c == '\n' || c == '\r' || c == '\t' || (c == '\\' && i+1 < len(d.Text) && strings.IndexByte("nrt\"", d.Text[i+1]) >= 0) {
					cut = i
					break
				}
			}
			if cut >= 0 {
				d.Text = strings.TrimSpace(d.Text[:cut])
				d.End = d.Start + len(d.Text)
				if d.Text == "" {
					continue
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// extendPersonSpans widens a PERSON detection over one directly following
// capitalised word of the same script ("Nikolai" + " Petrov"): NER models in
// code/log context often tag only the first name, which would leak the
// surname. Only forward, only letters-only words, only when that word is not
// already covered by another detection.
func extendPersonSpans(text string, dets []presidio.Detection) []presidio.Detection {
	for i, d := range dets {
		if d.EntityType != "PERSON" || d.End <= d.Start || d.End >= len(text) || text[d.End] != ' ' {
			continue
		}
		if len(strings.Fields(d.Text)) > 2 {
			continue
		}
		j := d.End + 1
		k := j
		for k < len(text) {
			r, w := utf8.DecodeRuneInString(text[k:])
			if !unicode.IsLetter(r) {
				break
			}
			k += w
		}
		if k-j < 2 {
			continue
		}
		if k < len(text) {
			if r, _ := utf8.DecodeRuneInString(text[k:]); r == '_' || unicode.IsDigit(r) || r == '-' {
				continue
			}
		}
		first, _ := utf8.DecodeRuneInString(text[j:])
		last, _ := utf8.DecodeLastRuneInString(d.Text)
		if !unicode.IsUpper(first) || !sameScript(first, last) {
			continue
		}
		covered := false
		for m, e := range dets {
			if m != i && e.Start < k && j < e.End {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		dets[i].End = k
		dets[i].Text = text[d.Start:k]
	}
	return dets
}

func sameScript(a, b rune) bool {
	return unicode.Is(unicode.Cyrillic, a) == unicode.Is(unicode.Cyrillic, b) &&
		unicode.Is(unicode.Latin, a) == unicode.Is(unicode.Latin, b)
}

// mergeDetections unions per-language detections. A detection overlapping one
// already kept replaces it only when it is longer (ties: higher score wins,
// otherwise the earlier language is kept). Order of the first list is kept.
func mergeDetections(lists [][]presidio.Detection) []presidio.Detection {
	var out []presidio.Detection
	for _, list := range lists {
		base := len(out) // detections of one language never dedupe each other
		for _, d := range list {
			var overlap []int
			for i := 0; i < base; i++ {
				if out[i].Start < d.End && d.Start < out[i].End {
					overlap = append(overlap, i)
				}
			}
			if len(overlap) == 0 {
				out = append(out, d)
				continue
			}
			wins := true
			for _, i := range overlap {
				o := out[i]
				ol, dl := o.End-o.Start, d.End-d.Start
				if dl < ol || (dl == ol && d.Score <= o.Score) {
					wins = false
					break
				}
			}
			if !wins {
				continue
			}
			// replace the first overlapped, drop the others
			out[overlap[0]] = d
			for j := len(overlap) - 1; j >= 1; j-- {
				out = append(out[:overlap[j]], out[overlap[j]+1:]...)
				base--
			}
		}
	}
	return out
}

// nerTypes are the entity types produced by a language's NER model, the only
// ones whose span can be a wrong-script guess (see language.ScriptMatches).
var nerTypes = map[string]bool{"PERSON": true, "ORGANIZATION": true, "LOCATION": true, "NRP": true}

// filterScript fixes NER detections that stray into another script than lang
// (the Russian model tagging "ECHO: Коля" or a whole English sentence): edge
// words that are not in lang's script are trimmed off the span, and a span
// that is still mostly foreign (or empty) is dropped.
func filterScript(lang string, dets []presidio.Detection) []presidio.Detection {
	out := dets[:0:0]
	for _, d := range dets {
		if nerTypes[d.EntityType] {
			for _, part := range splitForeignRuns(lang, d) {
				if part.Text != "" && language.ScriptMatches(lang, part.Text) {
					out = append(out, part)
				}
			}
			continue
		}
		out = append(out, d)
	}
	return out
}

// splitForeignRuns cuts an NER span at interior words that are not in lang's
// script: the Russian model tags "Святослав Иванов and Коля" as one PERSON; the
// foreign "and" must not make the whole span look foreign (which used to drop
// the name entirely). Each run of native-script words becomes its own span.
func splitForeignRuns(lang string, d presidio.Detection) []presidio.Detection {
	native := func(w string) bool {
		return language.ScriptMatches(lang, w) && strings.ContainsFunc(w, unicode.IsLetter)
	}
	var out []presidio.Detection
	runStart, runEnd := -1, -1
	flush := func() {
		if runStart >= 0 {
			p := d
			p.Text = d.Text[runStart:runEnd]
			p.Start = d.Start + runStart
			p.End = d.Start + runEnd
			out = append(out, p)
		}
		runStart, runEnd = -1, -1
	}
	i := 0
	for i < len(d.Text) {
		for i < len(d.Text) && (d.Text[i] == ' ' || d.Text[i] == '\t' || d.Text[i] == '\n') {
			i++
		}
		j := i
		for j < len(d.Text) && d.Text[j] != ' ' && d.Text[j] != '\t' && d.Text[j] != '\n' {
			j++
		}
		if j > i {
			if native(d.Text[i:j]) {
				if runStart < 0 {
					runStart = i
				}
				runEnd = j
			} else {
				flush()
			}
		}
		i = j
	}
	flush()
	if len(out) == 1 && out[0].Text == d.Text {
		return []presidio.Detection{d}
	}
	return out
}

// trimForeignEdges drops leading/trailing words with no letter in lang's
// script. Start/End are byte offsets; Text is updated to match. An empty Text
// in the result means nothing was left.
func trimForeignEdges(lang string, d presidio.Detection) presidio.Detection {
	words := strings.Fields(d.Text)
	foreign := func(w string) bool {
		return !language.ScriptMatches(lang, w) || !strings.ContainsFunc(w, unicode.IsLetter)
	}
	lo, hi := 0, len(words)
	for lo < hi && foreign(words[lo]) {
		lo++
	}
	for hi > lo && foreign(words[hi-1]) {
		hi--
	}
	if lo == 0 && hi == len(words) {
		return d
	}
	if lo >= hi {
		d.Text = ""
		return d
	}
	first := strings.Index(d.Text, words[lo])
	// end of the last kept word, searching from the first kept one
	last := first + strings.Index(d.Text[first:], words[hi-1]) + len(words[hi-1])
	if hi-1 > lo {
		// words may repeat: use the last occurrence
		last = strings.LastIndex(d.Text, words[hi-1]) + len(words[hi-1])
	}
	d.End = d.Start + last
	d.Start += first
	d.Text = d.Text[first:last]
	return d
}

// preferNarrowerHigherScore drops a detection when a narrower one of the same
// entity type inside it scores strictly higher: a precise recognizer hit
// ("Коля", 0.95) beats the NER span that swallowed a neighbouring word
// ("Вчера Коля", 0.85).
func preferNarrowerHigherScore(dets []presidio.Detection) []presidio.Detection {
	out := dets[:0:0]
	for i, d := range dets {
		drop := false
		for j, e := range dets {
			if i != j && e.EntityType == d.EntityType && e.Start >= d.Start && e.End <= d.End &&
				e.End-e.Start < d.End-d.Start && e.Score > d.Score {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, d)
		}
	}
	return out
}
