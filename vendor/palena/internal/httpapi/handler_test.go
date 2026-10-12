// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/pseudonymizer"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/text"
)

type stubAnalyzer struct {
	dets []presidio.Detection
	err  error
}

func (s *stubAnalyzer) Analyze(_ context.Context, _ string, _ []string, _ string) ([]presidio.Detection, error) {
	return s.dets, s.err
}

type stubPinger struct{ err error }

func (s *stubPinger) Ping(_ context.Context) error { return s.err }

func newTestServer(t *testing.T, analyzer text.Analyzer) (http.Handler, mapping.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{
		SessionTTL: time.Minute, Timeout: time.Second,
	}, nil)
	th := text.NewHandler(text.HandlerConfig{
		Analyzer: analyzer,
		Store:    store,
		Pools:    text.NewPools(map[string][]string{"PERSON": {"Alpha"}, "ORGANIZATION": {"OrgOne"}}),
		Entities: []string{"PERSON", "ORGANIZATION"},
	})
	orc := pseudonymizer.NewOrchestrator(th)

	handler := NewHandler(HandlerConfig{
		Orchestrator: orc,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return NewRouter(RouterConfig{Handler: handler, Store: store}), store
}

func post(t *testing.T, srv http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestHandler_RequestPseudonymizes(t *testing.T) {
	srv, _ := newTestServer(t, &stubAnalyzer{
		dets: []presidio.Detection{{EntityType: "PERSON", Text: "Thomas Weber", Score: 0.9}},
	})
	rec := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{
		InputType:     "request",
		LiteLLMCallID: "call-1",
		Texts:         []string{"Please contact Thomas Weber."},
		RequestData:   RequestData{UserAPIKeyEndUserID: "user-1"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp GuardrailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "GUARDRAIL_INTERVENED", resp.Action)
	require.Equal(t, []string{"Please contact Alpha."}, resp.Texts)
}

func TestHandler_ResponseReverses(t *testing.T) {
	srv, _ := newTestServer(t, &stubAnalyzer{
		dets: []presidio.Detection{{EntityType: "PERSON", Text: "Thomas Weber", Score: 0.9}},
	})
	// pre-call to populate
	pre := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{
		InputType:     "request",
		LiteLLMCallID: "call-1",
		Texts:         []string{"Hi Thomas Weber"},
		RequestData:   RequestData{UserAPIKeyEndUserID: "user-1"},
	})
	require.Equal(t, http.StatusOK, pre.Code)

	// post-call sees the pseudonym
	postRec := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{
		InputType:     "response",
		LiteLLMCallID: "call-1",
		Texts:         []string{"Alpha reported success."},
		RequestData:   RequestData{UserAPIKeyEndUserID: "user-1"},
	})
	require.Equal(t, http.StatusOK, postRec.Code)
	var resp GuardrailResponse
	require.NoError(t, json.Unmarshal(postRec.Body.Bytes(), &resp))
	require.Equal(t, "GUARDRAIL_INTERVENED", resp.Action)
	require.Equal(t, []string{"Thomas Weber reported success."}, resp.Texts)
}

func TestHandler_PresidioUnavailable_502(t *testing.T) {
	srv, _ := newTestServer(t, &stubAnalyzer{err: errs.Wrap(errs.ErrPresidioUnavailable, "boom")})
	rec := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{
		InputType:     "request",
		LiteLLMCallID: "call-1",
		Texts:         []string{"hi"},
	})
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestHandler_DocumentsBlocked(t *testing.T) {
	srv, _ := newTestServer(t, &stubAnalyzer{})
	rec := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{
		InputType:     "request",
		LiteLLMCallID: "call-1",
		StructuredMessages: []map[string]any{
			{"role": "user", "content": []any{
				map[string]any{"type": "document", "source": map[string]any{"data": "..."}},
			}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp GuardrailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "BLOCKED", resp.Action)
	require.Equal(t, "documents_not_supported_in_v1", resp.BlockedReason)
}

func TestHandler_BadInputType_400(t *testing.T) {
	srv, _ := newTestServer(t, &stubAnalyzer{})
	rec := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{
		InputType: "moderation",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandler_UnauthorizedWithoutAPIKey(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	th := text.NewHandler(text.HandlerConfig{
		Analyzer: &stubAnalyzer{}, Store: store,
		Pools: text.NewPools(nil), Entities: []string{"PERSON"},
	})
	orc := pseudonymizer.NewOrchestrator(th)
	handler := NewHandler(HandlerConfig{
		Orchestrator: orc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIKey: "secret",
	})
	srv := NewRouter(RouterConfig{Handler: handler, Store: store})

	rec := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{InputType: "request"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// With correct key
	buf, _ := json.Marshal(GuardrailRequest{InputType: "request", LiteLLMCallID: "c"})
	req := httptest.NewRequest(http.MethodPost, "/beta/litellm_basic_guardrail_api", bytes.NewReader(buf))
	req.Header.Set("x-api-key", "secret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestReadyz_AllHealthy(t *testing.T) {
	srv, _ := newTestServer(t, &stubAnalyzer{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestReadyz_UnavailableDep_503(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	analyzer := &stubPinger{err: errors.New("down")}

	th := text.NewHandler(text.HandlerConfig{
		Analyzer: &stubAnalyzer{}, Store: store,
		Pools: text.NewPools(nil), Entities: []string{"PERSON"},
	})
	orc := pseudonymizer.NewOrchestrator(th)
	handler := NewHandler(HandlerConfig{Orchestrator: orc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := NewRouter(RouterConfig{Handler: handler, Store: store, AnalyzerPinger: analyzer})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "presidio_analyzer")
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, &stubAnalyzer{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHandler_PatternOnlyIndices(t *testing.T) {
	ner := func(sub string) presidio.Detection {
		return presidio.Detection{EntityType: "ORGANIZATION", Text: sub, Score: 0.9,
			RecognitionMetadata: &presidio.RecognitionMetadata{RecognizerName: "SpacyRecognizer"}}
	}
	srv, _ := newTestServer(t, &stubAnalyzer{dets: []presidio.Detection{ner("Vercel Functions")}})
	rec := post(t, srv, "/beta/litellm_basic_guardrail_api", GuardrailRequest{
		InputType:                        "request",
		LiteLLMCallID:                    "c1",
		Texts:                            []string{"We use Vercel Functions", "We use Vercel Functions"},
		AdditionalProviderSpecificParams: map[string]any{"pattern_only_indices": []int{0, 7, -1}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp GuardrailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "GUARDRAIL_INTERVENED", resp.Action)
	require.Equal(t, "We use Vercel Functions", resp.Texts[0], "index 0 is pattern-only")
	require.Equal(t, "We use OrgOne", resp.Texts[1])
}
