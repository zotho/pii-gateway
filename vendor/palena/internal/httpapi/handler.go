// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/audit"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/pseudonymizer"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/session"
)

// Handler is the /beta/litellm_basic_guardrail_api endpoint.
type Handler struct {
	orc             *pseudonymizer.Orchestrator
	logger          *slog.Logger
	maxRequestBytes int64
	metadataKey     string // default when not set per-request
	apiKey          string // shared secret; empty = dev mode
}

// HandlerConfig groups constructor parameters.
type HandlerConfig struct {
	Orchestrator    *pseudonymizer.Orchestrator
	Logger          *slog.Logger
	MaxRequestBytes int64
	MetadataKey     string
	APIKey          string
}

// NewHandler builds the guardrail handler.
func NewHandler(cfg HandlerConfig) *Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBytes := cfg.MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = 32 << 20 // 32 MiB
	}
	metaKey := cfg.MetadataKey
	if metaKey == "" {
		metaKey = session.DefaultMetadataKey
	}
	return &Handler{
		orc:             cfg.Orchestrator,
		logger:          logger,
		maxRequestBytes: maxBytes,
		metadataKey:     metaKey,
		apiKey:          cfg.APIKey,
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !h.checkAPIKey(r) {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	defer func() { _ = r.Body.Close() }()

	var req GuardrailRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			h.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
			return
		}
		// Retry once with a permissive decoder — LiteLLM adds fields
		// over time and DisallowUnknownFields is too strict for a
		// forward-compatible contract. This branch only triggers when
		// the error was "unknown field".
		if strings.Contains(err.Error(), "unknown field") {
			// Re-parse permissively.
			// We already consumed the body once, so this path is a
			// fallback that returns 400 with a friendly message
			// (the caller can retry). Simpler than juggling readers.
			h.writeError(w, http.StatusBadRequest, "unknown_field")
			return
		}
		h.writeError(w, http.StatusBadRequest, "bad_json")
		return
	}

	// Translate to the internal request type.
	inputType := pseudonymizer.InputType(strings.ToLower(strings.TrimSpace(req.InputType)))
	if inputType != pseudonymizer.InputRequest && inputType != pseudonymizer.InputResponse {
		h.writeError(w, http.StatusBadRequest, "bad_input_type")
		return
	}

	// Derive session id.
	sid, src := session.Derive(session.Input{
		StructuredMessages: req.StructuredMessages,
		EndUserID:          req.RequestData.UserAPIKeyEndUserID,
		TraceID:            req.LiteLLMTraceID,
		CallID:             req.LiteLLMCallID,
		MetadataKey:        h.effectiveMetadataKey(req),
	})
	if sid == "" {
		h.writeError(w, http.StatusBadRequest, errs.ErrSessionDerivationFailed.Error())
		return
	}

	// Documents in v1 are rejected by the orchestrator; peek here so
	// we can also block on document blocks smuggled inside
	// structured_messages content.
	docCount := countDocumentBlocks(req.StructuredMessages)

	internalReq := &pseudonymizer.Request{
		InputType:       inputType,
		SessionID:       sid,
		Texts:           req.Texts,
		PatternOnly:     patternOnlyIndices(req),
		Images:          req.Images,
		DocumentCount:   docCount,
		StreamTransform: streamTransformMode(req) == "incremental_diff" && inputType == pseudonymizer.InputResponse,
	}

	resp, err := h.orc.Handle(r.Context(), internalReq)
	if err != nil {
		h.mapError(w, r, err, inputType)
		return
	}

	wire := GuardrailResponse{Action: string(resp.Action)}
	if resp.Action == pseudonymizer.ActionBlocked {
		wire.BlockedReason = resp.BlockedReason
	}
	if resp.Action == pseudonymizer.ActionIntervene {
		if len(resp.Texts) > 0 {
			wire.Texts = resp.Texts
		}
		if len(resp.Images) > 0 {
			wire.Images = resp.Images
		}
		if len(resp.StreamHoldbackChars) > 0 {
			wire.StreamHoldbackChars = resp.StreamHoldbackChars
		}
	}

	h.writeJSON(w, http.StatusOK, wire)

	h.logResult(r.Context(), &req, sid, src, resp, time.Since(start))
}

// checkAPIKey does a constant-time compare when configured.
func (h *Handler) checkAPIKey(r *http.Request) bool {
	if h.apiKey == "" {
		return true
	}
	got := r.Header.Get("x-api-key")
	if len(got) != len(h.apiKey) {
		return false
	}
	var diff byte
	for i := 0; i < len(got); i++ {
		diff |= got[i] ^ h.apiKey[i]
	}
	return diff == 0
}

// effectiveMetadataKey resolves session_id_metadata_key with request
// overriding the default.
func (h *Handler) effectiveMetadataKey(req GuardrailRequest) string {
	if v, ok := req.AdditionalProviderSpecificParams["session_id_metadata_key"].(string); ok && v != "" {
		return v
	}
	return h.metadataKey
}

// mapError converts internal errors to the right HTTP status per
// context/FAILURE_MODES.md.
func (h *Handler) mapError(w http.ResponseWriter, r *http.Request, err error, phase pseudonymizer.InputType) {
	// Post-call best-effort — mapping-store unavailability is already
	// handled inside the orchestrator (returns NONE). Anything else
	// here on post-call is unexpected.
	switch {
	case errors.Is(err, errs.ErrPresidioUnavailable),
		errors.Is(err, errs.ErrPresidioMalformed):
		h.logger.LogAttrs(r.Context(), slog.LevelError,
			"presidio_error",
			slog.String("phase", string(phase)),
			slog.String("err", err.Error()),
		)
		h.writeError(w, http.StatusBadGateway, errs.ErrPresidioUnavailable.Error())
	case errors.Is(err, errs.ErrMappingStoreUnavailable):
		h.logger.LogAttrs(r.Context(), slog.LevelError,
			"mapping_error",
			slog.String("phase", string(phase)),
			slog.String("err", err.Error()),
		)
		h.writeError(w, http.StatusBadGateway, errs.ErrMappingStoreUnavailable.Error())
	case errors.Is(err, errs.ErrRequestTooLarge):
		h.writeError(w, http.StatusRequestEntityTooLarge, errs.ErrRequestTooLarge.Error())
	default:
		h.logger.LogAttrs(r.Context(), slog.LevelError,
			"internal_error",
			slog.String("phase", string(phase)),
			slog.String("err", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, "internal")
	}
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, reason string) {
	h.writeJSON(w, status, ErrorResponse{Error: reason})
}

// logResult emits one structured line per successful call — see
// context/FAILURE_MODES.md § "Audit metadata schema".
func (h *Handler) logResult(ctx context.Context, req *GuardrailRequest, sid string, src session.Source, resp *pseudonymizer.Response, dur time.Duration) {
	counters := resp.Counters
	if counters == nil {
		counters = audit.NewCounters()
	}
	h.logger.LogAttrs(ctx, slog.LevelInfo,
		"guardrail_applied",
		slog.String("input_type", req.InputType),
		slog.String("call_id", req.LiteLLMCallID),
		slog.String("session_id_hash", audit.SessionIDHash(sid)),
		slog.String("session_id_source", string(src)),
		slog.Int64("duration_ms", dur.Milliseconds()),
		slog.String("action", string(resp.Action)),
		slog.Int("entities_detected", counters.EntitiesDetected),
		slog.Int("entities_pseudonymized", counters.EntitiesPseudonymized),
		slog.Int("new_mappings_created", counters.NewMappingsCreated),
		slog.Int("session_mapping_size", counters.SessionMappingSize),
	)
}

// streamTransformMode reads
// additional_provider_specific_params.streaming_transform_mode. Returns
// "block_only" (default) or "incremental_diff". Any other value is
// treated as the default so the handler is tolerant of typos.
func streamTransformMode(req GuardrailRequest) string {
	if v, ok := req.AdditionalProviderSpecificParams["streaming_transform_mode"].(string); ok {
		if v == "incremental_diff" {
			return "incremental_diff"
		}
	}
	return "block_only"
}

// countDocumentBlocks walks structured_messages and returns the count
// of `type: document` content blocks. Any positive count → block.
func countDocumentBlocks(msgs []map[string]any) int {
	n := 0
	for _, m := range msgs {
		content, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == "document" || t == "file" {
				n++
			}
		}
	}
	return n
}

// patternOnlyIndices reads additional_provider_specific_params.pattern_only_indices (a JSON
// array of text indices). Non-integers and out-of-range values are ignored.
func patternOnlyIndices(req GuardrailRequest) map[int]bool {
	raw, ok := req.AdditionalProviderSpecificParams["pattern_only_indices"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[int]bool, len(raw))
	for _, v := range raw {
		f, ok := v.(float64)
		if !ok || f != float64(int(f)) || f < 0 || int(f) >= len(req.Texts) {
			continue
		}
		out[int(f)] = true
	}
	return out
}
