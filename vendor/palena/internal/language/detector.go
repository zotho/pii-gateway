// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package language picks the language Presidio analyzes a text in.
//
// Presidio's NER is per language: German text analyzed as English misses most
// German names. With one configured language every text is analyzed in it.
// With several, each text is classified among them (lingua-go, offline, pure
// Go) and analyzed with that language's model; a text too short or ambiguous to
// classify falls back to the first configured language.
package language

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/pemistahl/lingua-go"
)

// Detector returns the ISO 639-1 code Presidio should analyze text in.
type Detector interface {
	Detect(text string) string
}

// Fixed always answers the same language.
type Fixed string

// Detect implements Detector.
func (f Fixed) Detect(string) string { return string(f) }

// Lingua classifies text among a fixed set of languages.
type Lingua struct {
	detector lingua.LanguageDetector
	fallback string
}

// Detect implements Detector.
func (l *Lingua) Detect(text string) string {
	lang, ok := l.detector.DetectLanguageOf(text)
	if !ok {
		return l.fallback
	}
	return strings.ToLower(lang.IsoCode639_1().String())
}

// Parse validates ISO 639-1 codes, lower-cases them and drops duplicates,
// keeping order (the first is the fallback).
func Parse(codes []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		if lingua.GetIsoCode639_1FromValue(strings.ToUpper(c)) == lingua.UnknownIsoCode639_1 {
			return nil, fmt.Errorf("unknown ISO 639-1 language code %q", c)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// New builds a detector for already-validated codes (see Parse). One language
// needs no classification; several load their models up front so the first
// request does not pay for it.
func New(codes []string) Detector {
	switch len(codes) {
	case 0:
		return Fixed("en")
	case 1:
		return Fixed(codes[0])
	}
	langs := make([]lingua.Language, 0, len(codes))
	for _, c := range codes {
		langs = append(langs, lingua.GetLanguageFromIsoCode639_1(lingua.GetIsoCode639_1FromValue(strings.ToUpper(c))))
	}
	return &Lingua{
		detector: lingua.NewLanguageDetectorBuilder().
			FromLanguages(langs...).
			WithPreloadedLanguageModels().
			Build(),
		fallback: codes[0],
	}
}

// ScriptMatches reports whether most letters of s are in the script of lang.
// Presidio's NER model of one language happily tags foreign-script text (the
// Russian model labels a whole English sentence ORG), which would swallow
// correct detections of the other language when results are unioned. Text
// without letters, and languages whose script is unknown here, always match.
func ScriptMatches(lang, s string) bool {
	var table *unicode.RangeTable
	switch strings.ToLower(lang) {
	case "ru", "uk", "bg", "be", "sr", "mk", "kk", "mn":
		table = unicode.Cyrillic
	case "en", "de", "fr", "es", "it", "pt", "nl", "pl", "cs", "sv", "da", "nb", "nn", "fi", "ro", "hu", "tr", "ca", "hr", "sk", "sl", "lt", "lv", "et", "id":
		table = unicode.Latin
	default:
		return true
	}
	var in, letters int
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
			if unicode.Is(table, r) {
				in++
			}
		}
	}
	return letters == 0 || in*2 >= letters
}
