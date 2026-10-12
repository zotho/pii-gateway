// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package httpapi is the HTTP layer: request/response DTOs, chi
// router, handlers. See context/PROTOCOL.md for the wire contract.
package httpapi

// GuardrailRequest matches LiteLLM's GenericGuardrailAPIRequest. Extra
// fields are accepted and ignored so a newer LiteLLM version does not
// break us.
type GuardrailRequest struct {
	InputType                        string            `json:"input_type"`
	LiteLLMCallID                    string            `json:"litellm_call_id,omitempty"`
	LiteLLMTraceID                   string            `json:"litellm_trace_id,omitempty"`
	LiteLLMVersion                   string            `json:"litellm_version,omitempty"`
	Model                            string            `json:"model,omitempty"`
	Texts                            []string          `json:"texts,omitempty"`
	StructuredMessages               []map[string]any  `json:"structured_messages,omitempty"`
	Images                           []string          `json:"images,omitempty"`
	Tools                            []map[string]any  `json:"tools,omitempty"`
	ToolCalls                        []map[string]any  `json:"tool_calls,omitempty"`
	RequestData                      RequestData       `json:"request_data,omitempty"`
	RequestHeaders                   map[string]string `json:"request_headers,omitempty"`
	AdditionalProviderSpecificParams map[string]any    `json:"additional_provider_specific_params,omitempty"`
}

// RequestData mirrors LiteLLM's GenericGuardrailAPIMetadata TypedDict.
type RequestData struct {
	UserAPIKeyHash      string `json:"user_api_key_hash,omitempty"`
	UserAPIKeyAlias     string `json:"user_api_key_alias,omitempty"`
	UserAPIKeyUserID    string `json:"user_api_key_user_id,omitempty"`
	UserAPIKeyUserEmail string `json:"user_api_key_user_email,omitempty"`
	UserAPIKeyTeamID    string `json:"user_api_key_team_id,omitempty"`
	UserAPIKeyTeamAlias string `json:"user_api_key_team_alias,omitempty"`
	UserAPIKeyEndUserID string `json:"user_api_key_end_user_id,omitempty"`
	UserAPIKeyOrgID     string `json:"user_api_key_org_id,omitempty"`
}

// GuardrailResponse matches LiteLLM's GenericGuardrailAPIResponse.
// Optional fields are omitted when empty.
type GuardrailResponse struct {
	Action              string           `json:"action"`
	BlockedReason       string           `json:"blocked_reason,omitempty"`
	Texts               []string         `json:"texts,omitempty"`
	Images              []string         `json:"images,omitempty"`
	Tools               []map[string]any `json:"tools,omitempty"`
	StreamHoldbackChars []int            `json:"stream_holdback_chars,omitempty"`
}

// ErrorResponse is used for non-200 responses. See
// context/FAILURE_MODES.md for status code mapping.
type ErrorResponse struct {
	Error string `json:"error"`
}
