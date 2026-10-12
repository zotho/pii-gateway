// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/pseudonymizer"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/session"
)

// WaegRequest is the Wäg gateway HTTP guardrail request shape
// (pre_call / during_call / post_call). See waeg docs/guardrails.md.
type WaegRequest struct {
	Guardrail    string           `json:"guardrail"`
	Phase        string           `json:"phase"`
	RequestID    string           `json:"request_id"`
	Model        string           `json:"model"`
	Text         string           `json:"text"`
	InputText    string           `json:"input_text"`
	OutputText   string           `json:"output_text"`
	Pending      string           `json:"pending"`
	CommittedLen int              `json:"committed_len"`
	Delta        string           `json:"delta"`
	EOF          bool             `json:"eof"`
	Role         string           `json:"role"`
	Stream       bool             `json:"stream"`
	StreamChunk  bool             `json:"stream_chunk"`
	ChunkIndex   int              `json:"chunk_index"`
	Messages     []map[string]any `json:"messages"`
	Identity     *WaegIdentity    `json:"identity"`
}

// WaegIdentity is optional caller context when forward_identity is on.
type WaegIdentity struct {
	TenantID      string `json:"tenant_id"`
	UserID        string `json:"user_id"`
	VirtualKeyID  string `json:"virtual_key_id"`
	TeamID        string `json:"team_id"`
	AuthMethod    string `json:"auth_method"`
	SessionID     string `json:"session_id"`
	PrincipalKind string `json:"principal_kind"`
	ApplicationID string `json:"application_id"`
}

// WaegResponse is the Wäg action protocol (allow | mask | block + hold).
type WaegResponse struct {
	Action     string   `json:"action"`
	Reason     string   `json:"reason,omitempty"`
	Text       string   `json:"text,omitempty"`
	Pending    string   `json:"pending,omitempty"`
	HoldChars  int      `json:"hold_chars,omitempty"`
	RulesHit   []string `json:"rules_hit,omitempty"`
	RedactRest bool     `json:"redact_rest,omitempty"`
}

// ServeWaegHTTP handles POST /v1/waeg/check for the Wäg stream codec.
//
// Mapping:
//   - pre_call → input_type=request (pseudonymize)
//   - during_call / stream / post_call → input_type=response (reverse)
//   - request_id → litellm_call_id (pre↔stream pairing)
//   - identity.session_id → multi-turn session when present
//
// Auth: accepts x-api-key OR Authorization: Bearer (Wäg default).
func (h *Handler) ServeWaegHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !h.checkAPIKeyWaeg(r) {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	defer func() { _ = r.Body.Close() }()

	var wr WaegRequest
	if err := json.NewDecoder(r.Body).Decode(&wr); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			h.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
			return
		}
		h.writeError(w, http.StatusBadRequest, "bad_json")
		return
	}

	phase := strings.ToLower(strings.TrimSpace(wr.Phase))
	var inputType pseudonymizer.InputType
	var texts []string
	streamTransform := false

	switch phase {
	case "pre_call":
		inputType = pseudonymizer.InputRequest
		t := firstNonEmpty(wr.InputText, wr.Text)
		if t != "" {
			texts = []string{t}
		}
	case "during_call", "during_stream", "stream", "post_call":
		inputType = pseudonymizer.InputResponse
		// Prefer pending (uncommitted) for stream transform; else full text.
		t := firstNonEmpty(wr.Pending, wr.OutputText, wr.Text)
		if t != "" {
			texts = []string{t}
		}
		streamTransform = phase != "post_call"
	default:
		h.writeError(w, http.StatusBadRequest, "bad_phase")
		return
	}

	// Session: Wäg session fingerprint > user_id > request_id.
	endUser := ""
	sessMeta := ""
	if wr.Identity != nil {
		endUser = wr.Identity.UserID
		sessMeta = wr.Identity.SessionID
	}
	callID := strings.TrimSpace(wr.RequestID)
	if callID == "" {
		callID = "waeg-anon"
	}

	// Prefer explicit session_id; else derive like LiteLLM path.
	sid := sessMeta
	src := session.Source("waeg_identity")
	if sid == "" {
		var dsrc session.Source
		sid, dsrc = session.Derive(session.Input{
			EndUserID:   endUser,
			TraceID:     "",
			CallID:      callID,
			MetadataKey: h.metadataKey,
		})
		src = dsrc
	}
	if sid == "" {
		sid = callID
		src = session.Source("waeg_request_id")
	}

	internalReq := &pseudonymizer.Request{
		InputType:       inputType,
		SessionID:       sid,
		Texts:           texts,
		StreamTransform: streamTransform && inputType == pseudonymizer.InputResponse,
	}

	resp, err := h.orc.Handle(r.Context(), internalReq)
	if err != nil {
		h.mapError(w, r, err, inputType)
		return
	}

	out := WaegResponse{RulesHit: []string{"palena-pseudonymizer"}}
	switch resp.Action {
	case pseudonymizer.ActionBlocked:
		out.Action = "block"
		out.Reason = resp.BlockedReason
	case pseudonymizer.ActionIntervene:
		out.Action = "mask"
		if len(resp.Texts) > 0 {
			out.Text = resp.Texts[0]
			out.Pending = resp.Texts[0]
		}
		if len(resp.StreamHoldbackChars) > 0 {
			out.HoldChars = resp.StreamHoldbackChars[0]
		}
		// Transform path: never redact_rest — progressive emit.
		out.RedactRest = false
	default:
		out.Action = "allow"
	}

	h.writeJSON(w, http.StatusOK, out)
	h.logger.LogAttrs(r.Context(), slog.LevelInfo,
		"waeg_guardrail_applied",
		slog.String("phase", phase),
		slog.String("request_id", callID),
		slog.String("session_source", string(src)),
		slog.String("action", out.Action),
		slog.Int("hold_chars", out.HoldChars),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
}

// checkAPIKeyWaeg accepts x-api-key or Authorization: Bearer (Wäg default).
func (h *Handler) checkAPIKeyWaeg(r *http.Request) bool {
	if h.apiKey == "" {
		return true
	}
	if h.checkAPIKey(r) {
		return true
	}
	auth := r.Header.Get("Authorization")
	const p = "Bearer "
	if strings.HasPrefix(auth, p) {
		got := strings.TrimSpace(strings.TrimPrefix(auth, p))
		if len(got) != len(h.apiKey) {
			return false
		}
		var diff byte
		for i := 0; i < len(got); i++ {
			diff |= got[i] ^ h.apiKey[i]
		}
		return diff == 0
	}
	return false
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
