// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package pseudonymizer orchestrates one guardrail invocation:
// dispatches the request to the text / image handlers and aggregates
// results. See context/PROTOCOL.md for the request/response shape and
// context/PSEUDONYMIZATION_ALGORITHM.md for the per-text semantics.
package pseudonymizer

import (
	"context"
	"errors"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/audit"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/image"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/text"
)

// InputType matches PROTOCOL.md § "input_type". Kept as its own type
// so the handler layer can validate the incoming value at the edge.
type InputType string

const (
	InputRequest  InputType = "request"
	InputResponse InputType = "response"
)

// Action mirrors LiteLLM's action enum on the response.
type Action string

const (
	ActionNone      Action = "NONE"
	ActionIntervene Action = "GUARDRAIL_INTERVENED"
	ActionBlocked   Action = "BLOCKED"
)

// Request is the internal shape passed to Handle(). The HTTP layer
// converts the wire type into this.
type Request struct {
	InputType InputType
	SessionID string
	// Texts is the list to pseudonymize (pre-call) or reverse (post-call).
	// Position matters — the response Texts must be the same length and
	// order.
	Texts []string
	// PatternOnly holds indices into Texts that are analyzed in "pattern-only" mode (NER-derived
	// detections dropped; see text.Handler.PseudonymizeOpts). Pre-call only.
	PatternOnly map[int]bool
	// Images are LiteLLM's images[] entries (data URLs or remote URLs).
	// Only consulted on the pre-call phase.
	Images []string
	// Documents (v2 only) — for now, the handler layer returns a
	// BLOCKED action if this is non-zero.
	DocumentCount int
	// StreamTransform enables incremental_diff mode for
	// input_type=response. When true, the orchestrator emits per-choice
	// holdback char counts alongside the reversed text.
	StreamTransform bool
}

// Response is the internal shape returned by Handle(). The HTTP layer
// converts it back into wire JSON.
type Response struct {
	Action        Action
	BlockedReason string
	// Texts only carries values when Action == GUARDRAIL_INTERVENED.
	Texts []string
	// Images parallel to request Images (1:1). Only set when the image
	// pipeline changed at least one entry.
	Images []string
	// StreamHoldbackChars is populated only in incremental_diff
	// streaming mode; parallel to Texts by index.
	StreamHoldbackChars []int
	// Counters is the aggregated audit set for this call.
	Counters *audit.Counters
}

// Orchestrator is the top-level dispatcher.
type Orchestrator struct {
	text  *text.Handler
	image *image.Handler // may be nil when M1 wiring
}

// NewOrchestrator wires the text handler. Image handler is optional
// and added via WithImageHandler.
func NewOrchestrator(textHandler *text.Handler) *Orchestrator {
	return &Orchestrator{text: textHandler}
}

// WithImageHandler installs the image pipeline. Returns the receiver so
// callers can chain.
func (o *Orchestrator) WithImageHandler(h *image.Handler) *Orchestrator {
	o.image = h
	return o
}

// Handle dispatches per input_type.
func (o *Orchestrator) Handle(ctx context.Context, req *Request) (*Response, error) {
	if req.DocumentCount > 0 {
		return &Response{
			Action:        ActionBlocked,
			BlockedReason: "documents_not_supported_in_v1",
			Counters:      audit.NewCounters(),
		}, nil
	}

	switch req.InputType {
	case InputRequest:
		return o.handleRequest(ctx, req)
	case InputResponse:
		return o.handleResponse(ctx, req)
	default:
		return nil, errors.New("unknown input_type")
	}
}

func (o *Orchestrator) handleRequest(ctx context.Context, req *Request) (*Response, error) {
	counters := audit.NewCounters()
	out := &Response{Counters: counters}

	// Text pass
	texts := make([]string, len(req.Texts))
	textModified := false
	for i, raw := range req.Texts {
		res, err := o.text.PseudonymizeOpts(ctx, raw, req.SessionID, req.PatternOnly[i])
		if err != nil {
			return nil, err
		}
		texts[i] = res.Text
		if res.Text != raw {
			textModified = true
		}
		counters.EntitiesDetected += res.EntitiesDetected
		counters.EntitiesPseudonymized += res.EntitiesDetected
		counters.NewMappingsCreated += res.NewMappingsCreated
		for k, v := range res.EntityTypeCounts {
			counters.EntityTypes[k] += v
		}
		counters.SessionMappingSize = res.SessionMappingSize
	}

	// Image pass (only when the image handler is installed)
	var images []string
	imageModified := false
	if o.image != nil && len(req.Images) > 0 {
		imgOut, block, mod, imgCounters, err := o.image.ProcessAll(ctx, req.SessionID, req.Images)
		if err != nil {
			return nil, err
		}
		if block != nil {
			out.Action = ActionBlocked
			out.BlockedReason = block.Reason
			return out, nil
		}
		counters.ImagesProcessed += imgCounters.Total
		counters.ImagesPIIFound += imgCounters.WithPII
		counters.ImagesRedacted += imgCounters.Redacted
		counters.EntitiesDetected += imgCounters.EntitiesDetected
		for k, v := range imgCounters.PerImage {
			counters.EntityTypes[k] += v
		}
		if mod {
			images = imgOut
			imageModified = true
		}
	}

	if textModified || imageModified {
		out.Action = ActionIntervene
		if textModified {
			out.Texts = texts
		}
		if imageModified {
			out.Images = images
		}
	} else {
		out.Action = ActionNone
	}
	return out, nil
}

func (o *Orchestrator) handleResponse(ctx context.Context, req *Request) (*Response, error) {
	counters := audit.NewCounters()
	out := &Response{Counters: counters}

	if len(req.Texts) == 0 {
		out.Action = ActionNone
		return out, nil
	}

	texts := make([]string, len(req.Texts))
	modified := false

	// Streaming path — reverse + compute per-text holdback in one shot.
	if req.StreamTransform {
		result, err := o.text.ReverseStream(ctx, req.Texts, req.SessionID)
		if err != nil {
			if errors.Is(err, errs.ErrMappingStoreUnavailable) {
				out.Action = ActionNone
				return out, nil
			}
			return nil, err
		}
		anyHoldback := false
		for _, h := range result.Holdbacks {
			if h > 0 {
				anyHoldback = true
				break
			}
		}
		if !result.Modified && !anyHoldback {
			out.Action = ActionNone
			return out, nil
		}
		out.Action = ActionIntervene
		out.Texts = result.Texts
		out.StreamHoldbackChars = result.Holdbacks
		return out, nil
	}

	for i, raw := range req.Texts {
		reversed, err := o.text.Reverse(ctx, raw, req.SessionID)
		if err != nil {
			// Post-call is best-effort: on mapping-store failure we
			// return NONE so the LLM response is not lost.
			if errors.Is(err, errs.ErrMappingStoreUnavailable) {
				out.Action = ActionNone
				return out, nil
			}
			return nil, err
		}
		texts[i] = reversed
		if reversed != raw {
			modified = true
		}
	}

	if modified {
		out.Action = ActionIntervene
		out.Texts = texts
	} else {
		out.Action = ActionNone
	}
	return out, nil
}
