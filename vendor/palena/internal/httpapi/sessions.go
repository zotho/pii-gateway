// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/audit"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
)

// SessionsHandler serves session administration — currently only
// erasure (GDPR right-to-erasure / explicit teardown). Guarded by the
// same shared secret as the guardrail endpoint.
type SessionsHandler struct {
	store   mapping.Store
	apiKey  string
	timeout time.Duration
	logger  *slog.Logger
}

// SessionsHandlerConfig groups constructor parameters.
type SessionsHandlerConfig struct {
	Store   mapping.Store
	APIKey  string
	Timeout time.Duration
	Logger  *slog.Logger
}

// NewSessionsHandler builds the session admin handler.
func NewSessionsHandler(cfg SessionsHandlerConfig) *SessionsHandler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &SessionsHandler{
		store:   cfg.Store,
		apiKey:  cfg.APIKey,
		timeout: timeout,
		logger:  logger,
	}
}

// Delete removes a session's mapping. DELETE /sessions/{session_id}.
//
//   - 200 {"deleted": true|false} — deleted reports whether a mapping existed.
//   - 400 if the session id is empty.
//   - 401 if the shared secret is set and the header is missing/wrong.
//   - 502 if the store is unreachable.
func (h *SessionsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !apiKeyValid(r.Header.Get("x-api-key"), h.apiKey) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sid := chi.URLParam(r, "session_id")
	if sid == "" {
		writeErr(w, http.StatusBadRequest, "missing_session_id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	existed, err := h.store.Delete(ctx, sid)
	if err != nil {
		h.logger.LogAttrs(r.Context(), slog.LevelError, "session_delete_error",
			slog.String("session_id_hash", audit.SessionIDHash(sid)),
			slog.String("err", err.Error()),
		)
		writeErr(w, http.StatusBadGateway, "mapping_store_unavailable")
		return
	}

	h.logger.LogAttrs(r.Context(), slog.LevelInfo, "session_deleted",
		slog.String("session_id_hash", audit.SessionIDHash(sid)),
		slog.Bool("existed", existed),
	)
	writeJSONStatus(w, http.StatusOK, map[string]bool{"deleted": existed})
}

// apiKeyValid returns true when auth is disabled (want == "") or the
// presented key matches in constant time.
func apiKeyValid(got, want string) bool {
	if want == "" {
		return true
	}
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// writeJSONStatus writes body as JSON with the given status code.
func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErr writes an ErrorResponse with the given status.
func writeErr(w http.ResponseWriter, status int, reason string) {
	writeJSONStatus(w, status, ErrorResponse{Error: reason})
}
