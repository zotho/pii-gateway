// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package session derives a stable session id from a LiteLLM guardrail
// request. See context/SESSION_AND_STATE.md § "Derivation chain".
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Source names the origin of the derived id. Emitted as a Prometheus
// label so operators can spot wiring mistakes (e.g. everything falling
// back to litellm_call_id → conversations don't share state).
type Source string

const (
	SourceMetadataKey Source = "metadata_key"
	SourceEndUser     Source = "user_api_key_end_user_id"
	SourceTraceID     Source = "litellm_trace_id"
	SourceCallID      Source = "litellm_call_id"
	SourceSynthetic   Source = "synthetic"
)

// DefaultMetadataKey is the field name we look for in
// structured_messages[*].metadata when the operator did not override
// via additional_provider_specific_params.session_id_metadata_key.
const DefaultMetadataKey = "session_id"

// Input is the subset of a guardrail request we need for derivation.
// Kept as a plain struct (rather than the wire type) so the session
// package has no dependency on internal/httpapi.
type Input struct {
	// StructuredMessages is a list of message objects in OpenAI shape.
	// We walk them newest-first to find a per-message session_id in
	// message.metadata.
	StructuredMessages []map[string]any

	// EndUserID surfaces from request_data.user_api_key_end_user_id.
	EndUserID string

	// TraceID is litellm_trace_id.
	TraceID string

	// CallID is litellm_call_id.
	CallID string

	// MetadataKey overrides DefaultMetadataKey when the operator sets
	// additional_provider_specific_params.session_id_metadata_key.
	MetadataKey string
}

// Derive walks the priority chain and returns a session id + its
// source. Never returns empty.
func Derive(in Input) (string, Source) {
	key := in.MetadataKey
	if key == "" {
		key = DefaultMetadataKey
	}

	// 1. structured_messages[*].metadata[<key>] — newest first.
	for i := len(in.StructuredMessages) - 1; i >= 0; i-- {
		msg := in.StructuredMessages[i]
		meta, ok := msg["metadata"].(map[string]any)
		if !ok {
			continue
		}
		v, _ := meta[key].(string)
		v = strings.TrimSpace(v)
		if v != "" {
			return sanitize(v), SourceMetadataKey
		}
	}

	// 2. end_user_id
	if v := strings.TrimSpace(in.EndUserID); v != "" {
		return sanitize(v), SourceEndUser
	}

	// 3. trace id
	if v := strings.TrimSpace(in.TraceID); v != "" {
		return sanitize(v), SourceTraceID
	}

	// 4. call id — always present in real LiteLLM traffic
	if v := strings.TrimSpace(in.CallID); v != "" {
		return sanitize(v), SourceCallID
	}

	// 5. synthetic — never let two unrelated requests collide on empty
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand should never fail on Linux/darwin; if it does we
		// still need to return something distinct — use hex of a fixed
		// pattern instead of crashing the request. Extremely unlikely.
		return "anon-fallback", SourceSynthetic
	}
	return "anon-" + hex.EncodeToString(buf), SourceSynthetic
}

// sanitize rejects control chars and caps length. Session ids end up in
// Redis keys and log fields; they must be printable ASCII and bounded.
// Non-conforming inputs are SHA-256'd (via a small hash helper) rather
// than dropped so their identity is still stable.
var printableRE = regexp.MustCompile(`^[\x20-\x7E]{1,256}$`)

func sanitize(v string) string {
	if printableRE.MatchString(v) {
		return v
	}
	// Not printable or too long → deterministic hash so identity is
	// still stable per input, but the id is safe as a Redis key.
	sum := sha256.Sum256([]byte(v))
	return "hashed-" + hex.EncodeToString(sum[:16])
}
