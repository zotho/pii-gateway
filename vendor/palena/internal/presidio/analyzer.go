// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package presidio wraps the external Presidio Analyzer and Image
// Redactor HTTP services. Detection failures fail-closed per
// context/FAILURE_MODES.md.
package presidio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
)

// Detection is one entity returned by Presidio Analyzer.
type Detection struct {
	EntityType string  `json:"entity_type"`
	Start      int     `json:"start"`
	End        int     `json:"end"`
	Score      float64 `json:"score"`
	Text       string  `json:"text,omitempty"` // populated if server returned it, else we fill from start/end
	// RecognitionMetadata is Presidio's recognition_metadata (which recognizer produced the hit).
	RecognitionMetadata *RecognitionMetadata `json:"recognition_metadata,omitempty"`
}

// RecognitionMetadata mirrors Presidio's per-result recognition_metadata.
type RecognitionMetadata struct {
	RecognizerName string `json:"recognizer_name,omitempty"`
}

// nerRecognizerMarkers: lower-cased substrings of the names of recognizers that wrap a
// statistical NER model (Presidio's SpacyRecognizer, Stanza, Transformers/HuggingFace, Flair,
// GLiNER). Everything else (regex, deny-list, checksum recognizers) is a "pattern" recognizer.
var nerRecognizerMarkers = []string{"spacy", "stanza", "transformers", "huggingface", "flair", "gliner", "langextract"}

// IsNER reports whether the detection came from an NER-model recognizer. A detection with no
// recognizer metadata (older Presidio, stubs) is NOT NER: unknown sources are kept (fail towards masking).
func (d Detection) IsNER() bool {
	if d.RecognitionMetadata == nil {
		return false
	}
	n := strings.ToLower(d.RecognitionMetadata.RecognizerName)
	for _, m := range nerRecognizerMarkers {
		if strings.Contains(n, m) {
			return true
		}
	}
	return false
}

// AnalyzerConfig configures the client.
type AnalyzerConfig struct {
	BaseURL        string
	Timeout        time.Duration
	ScoreThreshold float64
}

// Analyzer is the HTTP client for POST /analyze.
type Analyzer struct {
	baseURL        string
	timeout        time.Duration
	scoreThreshold float64
	http           *http.Client
}

// NewAnalyzer constructs an analyzer client.
func NewAnalyzer(cfg AnalyzerConfig) *Analyzer {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Analyzer{
		baseURL:        strings.TrimRight(cfg.BaseURL, "/"),
		timeout:        cfg.Timeout,
		scoreThreshold: cfg.ScoreThreshold,
		http:           &http.Client{Timeout: cfg.Timeout},
	}
}

// analyzeRequest is the Presidio /analyze request body.
type analyzeRequest struct {
	Text           string   `json:"text"`
	Language       string   `json:"language"`
	Entities       []string `json:"entities,omitempty"`
	ScoreThreshold float64  `json:"score_threshold,omitempty"`
}

// Analyze detects PII entities in text. Empty input or empty entity
// list returns an empty slice without a network call.
//
// Errors:
//   - errs.ErrPresidioUnavailable — connect / timeout / 5xx
//   - errs.ErrPresidioMalformed   — non-JSON body or unexpected shape
func (a *Analyzer) Analyze(ctx context.Context, text string, entities []string, language string) ([]Detection, error) {
	if text == "" || len(entities) == 0 {
		return nil, nil
	}
	if language == "" {
		language = "en"
	}

	body, err := json.Marshal(analyzeRequest{
		Text:           text,
		Language:       language,
		Entities:       entities,
		ScoreThreshold: a.scoreThreshold,
	})
	if err != nil {
		// Should not happen — plain string/slice.
		return nil, fmt.Errorf("marshal analyze request: %w", err)
	}

	url := a.baseURL + "/analyze"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build analyze request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.ErrPresidioUnavailable, "analyze request")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		return nil, errs.Wrap(errs.ErrPresidioUnavailable, "analyze status %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		// 4xx from Presidio means we sent it something wrong. That's
		// a bug on our side, but from the caller's perspective it's
		// still "detection didn't work" — fail-closed.
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, errs.Wrap(errs.ErrPresidioUnavailable, "analyze status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var raw []Detection
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, errs.Wrap(errs.ErrPresidioMalformed, "decode analyze response")
	}

	// Fill in Text field if the server omitted it (older Presidio
	// versions return only offsets). Presidio counts in characters (Python
	// code points) while Go strings index bytes: Start/End are converted to
	// byte offsets first, or every non-ASCII character before an entity
	// ("Zürich", "rencontré") shifts the span and the entity goes unmasked.
	byteAt := runeByteOffsets(text)
	out := make([]Detection, 0, len(raw))
	for _, d := range raw {
		if d.EntityType == "" {
			continue
		}
		if d.Start < 0 || d.End >= len(byteAt) || d.Start >= d.End {
			continue
		}
		d.Start, d.End = byteAt[d.Start], byteAt[d.End]
		if d.Text == "" {
			d.Text = text[d.Start:d.End]
		}
		if strings.TrimSpace(d.Text) == "" {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// runeByteOffsets maps each character index of text to its byte offset, with
// one extra entry for the end of the string.
func runeByteOffsets(text string) []int {
	out := make([]int, 0, len(text)+1)
	for i := range text {
		out = append(out, i)
	}
	return append(out, len(text))
}
