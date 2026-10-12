// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package image

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
)

type fakeRedactor struct {
	redactBytes []byte
	redactErr   error
	analyzeOut  []presidio.ImageDetection
	analyzeErr  error
	redactCalls int
}

func (f *fakeRedactor) Redact(_ context.Context, image []byte, _ string) ([]byte, error) {
	f.redactCalls++
	if f.redactErr != nil {
		return nil, f.redactErr
	}
	if f.redactBytes != nil {
		return f.redactBytes, nil
	}
	return image, nil // no-op: signals "nothing to redact"
}

func (f *fakeRedactor) Analyze(_ context.Context, _ []byte) ([]presidio.ImageDetection, error) {
	return f.analyzeOut, f.analyzeErr
}

type simplePools struct {
	next map[string][]string
}

func (p *simplePools) Assign(entityType string, used map[string]struct{}, _ string) string {
	for _, name := range p.next[entityType] {
		if _, taken := used[name]; !taken {
			return name
		}
	}
	return "Synthetic-1"
}

func newHandler(t *testing.T, redactor Redactor) (*Handler, mapping.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{
		SessionTTL: time.Minute, Timeout: time.Second,
	}, nil)
	return NewHandler(HandlerConfig{
		Redactor: redactor,
		Store:    store,
		Pools:    &simplePools{next: map[string][]string{"PERSON": {"Alpha"}, "ORGANIZATION": {"OrgOne"}}},
		Entities: []string{"PERSON", "ORGANIZATION"},
	}), store
}

func b64(png []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func TestProcessAll_NoImagesNoop(t *testing.T) {
	h, _ := newHandler(t, &fakeRedactor{})
	out, block, mod, counters, err := h.ProcessAll(context.Background(), "s1", nil)
	require.NoError(t, err)
	require.Nil(t, block)
	require.False(t, mod)
	require.Empty(t, out)
	require.Zero(t, counters.Total)
}

func TestProcessAll_RedactedImageReplacesInput(t *testing.T) {
	orig := []byte("\x89PNG original content")
	redacted := []byte("\x89PNG redacted content")
	h, _ := newHandler(t, &fakeRedactor{
		redactBytes: redacted,
		analyzeOut:  []presidio.ImageDetection{{EntityType: "PERSON", Text: "Alice", Score: 0.9}},
	})
	out, block, mod, counters, err := h.ProcessAll(context.Background(), "s1", []string{b64(orig)})
	require.NoError(t, err)
	require.Nil(t, block)
	require.True(t, mod)
	require.Equal(t, 1, counters.Total)
	require.Equal(t, 1, counters.WithPII)
	require.Equal(t, 1, counters.Redacted)
	require.NotEqual(t, b64(orig), out[0])
	require.Contains(t, out[0], "data:image/png;base64,")
}

func TestProcessAll_NoPIIPassesUnchanged(t *testing.T) {
	orig := []byte("\x89PNG clean content")
	// Redactor returns identical bytes → no diff → no PII.
	h, _ := newHandler(t, &fakeRedactor{redactBytes: orig})
	out, block, mod, counters, err := h.ProcessAll(context.Background(), "s1", []string{b64(orig)})
	require.NoError(t, err)
	require.Nil(t, block)
	require.False(t, mod)
	require.Equal(t, 1, counters.Total)
	require.Zero(t, counters.WithPII)
	// Output still emitted (as data URL of original bytes) so LiteLLM
	// keeps the 1:1 slot mapping.
	require.NotEmpty(t, out[0])
}

func TestProcessAll_BlockActionRaisesBlock(t *testing.T) {
	orig := []byte("\x89PNG original")
	redacted := []byte("\x89PNG DIFFERENT")
	h, _ := newHandler(t, &fakeRedactor{
		redactBytes: redacted,
		analyzeOut:  []presidio.ImageDetection{{EntityType: "PERSON", Text: "Alice", Score: 0.9}},
	})
	h.cfg.PIIAction = ActionBlock
	_, block, _, _, err := h.ProcessAll(context.Background(), "s1", []string{b64(orig)})
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Contains(t, block.Reason, "PERSON")
	require.Contains(t, block.Reason, "personal data")
}

func TestProcessAll_PassthroughEmitsOriginal(t *testing.T) {
	orig := []byte("\x89PNG original")
	redacted := []byte("\x89PNG different")
	h, _ := newHandler(t, &fakeRedactor{
		redactBytes: redacted,
		analyzeOut:  []presidio.ImageDetection{{EntityType: "PERSON", Text: "Alice", Score: 0.9}},
	})
	h.cfg.PIIAction = ActionPassthrough
	out, block, mod, _, err := h.ProcessAll(context.Background(), "s1", []string{b64(orig)})
	require.NoError(t, err)
	require.Nil(t, block)
	require.False(t, mod, "passthrough emits original bytes, so no modification")
	require.Contains(t, out[0], "data:image/png;base64,")
}

func TestProcessAll_TooManyImagesBlocks(t *testing.T) {
	h, _ := newHandler(t, &fakeRedactor{})
	h.cfg.MaxImagesPerRequest = 1
	inputs := []string{b64([]byte("a")), b64([]byte("b"))}
	_, block, _, _, err := h.ProcessAll(context.Background(), "s1", inputs)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, "too_many_images", block.Reason)
}

func TestProcessAll_ImageTooLargeReturnsError(t *testing.T) {
	h, _ := newHandler(t, &fakeRedactor{})
	h.cfg.MaxImageBytes = 4
	_, _, _, _, err := h.ProcessAll(context.Background(), "s1", []string{b64([]byte("longer than four"))})
	require.Error(t, err)
	require.True(t, errors.Is(err, errs.ErrImageTooLarge))
}

func TestProcessAll_MappingEnrichedFromOCR(t *testing.T) {
	orig := []byte("\x89PNG original")
	redacted := []byte("\x89PNG redacted")
	h, store := newHandler(t, &fakeRedactor{
		redactBytes: redacted,
		analyzeOut: []presidio.ImageDetection{
			{EntityType: "PERSON", Text: "Alice Johnson", Score: 0.9},
		},
	})
	_, _, _, _, err := h.ProcessAll(context.Background(), "s1", []string{b64(orig)})
	require.NoError(t, err)
	m, err := store.GetMapping(context.Background(), "s1")
	require.NoError(t, err)
	require.Contains(t, m, "Alice Johnson")
	require.Equal(t, "Alpha", m["Alice Johnson"])
}
