// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/audit"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/text"
)

// PIIAction mirrors config.PIIAction.
type PIIAction string

const (
	ActionRedact      PIIAction = "redact"
	ActionBlock       PIIAction = "block"
	ActionPassthrough PIIAction = "passthrough"
)

// Redactor is the subset of *presidio.ImageRedactor the image handler
// uses. Interface for testability.
type Redactor interface {
	Redact(ctx context.Context, image []byte, color string) ([]byte, error)
	Analyze(ctx context.Context, image []byte) ([]presidio.ImageDetection, error)
}

// PoolAssigner is the subset of *text.Pools we depend on. Redeclared
// here to avoid an import cycle (text imports mapping, so image would
// too if we depended on text directly).
type PoolAssigner interface {
	Assign(entityType string, used map[string]struct{}, reserved string) string
}

// Strategist is the subset of *text.Strategizer the image handler uses:
// assign a pseudonym for an OCR'd entity, dispatching pool vs token.
type Strategist interface {
	Assign(entityType, real string, scratch map[string]string, reserved string) string
}

// HandlerConfig groups constructor parameters.
type HandlerConfig struct {
	Redactor Redactor
	Store    mapping.Store
	// Pools is the fallback substitution path when Strategizer is nil.
	Pools PoolAssigner
	// Strategizer, when set, selects per-entity substitution (pool vs
	// token) for OCR'd entities — matching the text pipeline.
	Strategizer         Strategist
	Entities            []string
	PIIAction           PIIAction
	BlockMessageTmpl    string // must contain "{entity_types}"
	MaxImageBytes       int64
	MaxImagesPerRequest int
	FetchClient         *http.Client
	RedactColor         string // e.g. "0, 0, 0"
	// DecomposePersonNames mirrors the text handler option: register
	// first/last-name sub-mappings for OCR'd PERSON entities.
	DecomposePersonNames bool
	// AllowList holds lower-cased terms never pseudonymized, matching the
	// text handler. An OCR'd name on the allow-list is not shared into the
	// session mapping (but the image is still redacted per PIIAction).
	AllowList map[string]struct{}
}

// Handler orchestrates image PII detection + redaction.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler builds an image handler.
func NewHandler(cfg HandlerConfig) *Handler {
	if cfg.PIIAction == "" {
		cfg.PIIAction = ActionRedact
	}
	if cfg.BlockMessageTmpl == "" {
		cfg.BlockMessageTmpl = "This attachment contains personal data ({entity_types}) that cannot be pseudonymized. Please remove sensitive information and try again."
	}
	if cfg.MaxImageBytes <= 0 {
		cfg.MaxImageBytes = 20 << 20
	}
	if cfg.MaxImagesPerRequest <= 0 {
		cfg.MaxImagesPerRequest = 20
	}
	if cfg.RedactColor == "" {
		cfg.RedactColor = "0, 0, 0"
	}
	return &Handler{cfg: cfg}
}

// ProcessResult carries the per-image outcome.
type ProcessResult struct {
	// OutputURL is what should appear at the same index in the
	// response's images[]. Always a data URL when set.
	OutputURL string
	// Blocked is true when this image triggered a BLOCK.
	Blocked bool
	// BlockReason is user-facing when Blocked.
	BlockReason string
	// Counters merged into the caller's audit set.
	EntitiesDetected int
	EntityTypeCounts map[audit.EntityType]int
	Redacted         bool
	OCREnriched      bool
}

// ProcessAll handles a full list of images[]. The response images list
// is 1:1 with the request per context/PROTOCOL.md. If any image blocks,
// callers should short-circuit the whole request.
//
// modified reports whether any image changed vs its input.
func (h *Handler) ProcessAll(ctx context.Context, sessionID string, inputs []string) (outputs []string, block *errs.BlockDecision, modified bool, counters ImageCounters, err error) {
	if len(inputs) == 0 {
		return nil, nil, false, ImageCounters{}, nil
	}
	if len(inputs) > h.cfg.MaxImagesPerRequest {
		return nil, errs.NewBlock("too_many_images"), false, ImageCounters{}, nil
	}
	outputs = make([]string, len(inputs))
	entityTypes := map[audit.EntityType]int{}
	total := ImageCounters{PerImage: entityTypes}
	blockEntities := map[string]struct{}{}
	blockedAny := false
	for i, raw := range inputs {
		payload, err := h.resolveInput(ctx, raw)
		if err != nil {
			return nil, nil, false, total, err
		}
		if payload == nil || len(payload.Bytes) == 0 {
			outputs[i] = raw
			continue
		}
		if int64(len(payload.Bytes)) > h.cfg.MaxImageBytes {
			return nil, nil, false, total, errs.ErrImageTooLarge
		}
		res, err := h.processOne(ctx, sessionID, payload)
		if err != nil {
			return nil, nil, false, total, err
		}
		total.Total++
		total.EntitiesDetected += res.EntitiesDetected
		for k, v := range res.EntityTypeCounts {
			entityTypes[k] += v
		}
		if res.EntitiesDetected > 0 {
			total.WithPII++
		}
		if res.Redacted {
			total.Redacted++
		}
		if res.Blocked {
			blockedAny = true
			for k := range res.EntityTypeCounts {
				blockEntities[string(k)] = struct{}{}
			}
			// Continue processing so we can surface the union of
			// entity types across all images in the block message.
			continue
		}
		if res.OutputURL != "" {
			outputs[i] = res.OutputURL
			if res.OutputURL != raw {
				modified = true
			}
		} else {
			outputs[i] = raw
		}
	}
	total.PerImage = entityTypes
	if blockedAny {
		types := make([]string, 0, len(blockEntities))
		for k := range blockEntities {
			types = append(types, k)
		}
		sort.Strings(types)
		reason := strings.Replace(h.cfg.BlockMessageTmpl, "{entity_types}", strings.Join(types, ", "), 1)
		return nil, errs.NewBlock(reason), false, total, nil
	}
	return outputs, nil, modified, total, nil
}

// ImageCounters is the per-request aggregate emitted alongside the
// text counters. Callers merge these into audit.Counters.
type ImageCounters struct {
	Total            int
	WithPII          int
	Redacted         int
	EntitiesDetected int
	PerImage         map[audit.EntityType]int
}

// resolveInput turns one images[] entry into raw bytes + media type.
// Returns nil, nil when the entry is empty (skip).
func (h *Handler) resolveInput(ctx context.Context, raw string) (*Payload, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	payload, isDataURL, err := DecodeDataURL(raw)
	if err != nil {
		return nil, err
	}
	if isDataURL {
		return payload, nil
	}
	// Remote URL: fetch under size cap.
	payload, err = FetchURL(ctx, h.cfg.FetchClient, raw, h.cfg.MaxImageBytes)
	if err != nil {
		return nil, errs.Wrap(errs.ErrImageFetchFailed, "%s", err)
	}
	return payload, nil
}

// processOne runs Presidio + applies the configured action for one image.
func (h *Handler) processOne(ctx context.Context, sessionID string, payload *Payload) (*ProcessResult, error) {
	res := &ProcessResult{EntityTypeCounts: map[audit.EntityType]int{}}

	// Best-effort OCR analyze — if Presidio returns 500 on /analyze we
	// still redact but lose the OCR-text session mapping enrichment.
	detections, _ := h.cfg.Redactor.Analyze(ctx, payload.Bytes)
	if len(detections) > 0 {
		res.OCREnriched = true
	}
	relevant := filterDetections(detections, h.cfg.Entities)
	res.EntitiesDetected = len(relevant)
	for _, d := range relevant {
		res.EntityTypeCounts[audit.NormalizeEntityType(d.EntityType)]++
	}
	if len(relevant) > 0 {
		if err := h.shareWithMapping(ctx, sessionID, relevant); err != nil {
			return nil, err
		}
	}

	// If /analyze returned nothing, we STILL need to decide whether
	// the image has PII — /redact is the source of truth. Redact
	// always; then compare byte content to detect whether Presidio
	// actually drew any black boxes.
	redacted, err := h.cfg.Redactor.Redact(ctx, payload.Bytes, h.cfg.RedactColor)
	if err != nil {
		return nil, err
	}

	piiFound := len(relevant) > 0 || bytesDiffer(payload.Bytes, redacted)

	if !piiFound {
		res.OutputURL = EncodeDataURL(payload.MediaType, payload.Bytes)
		return res, nil
	}

	switch h.cfg.PIIAction {
	case ActionBlock:
		res.Blocked = true
		return res, nil
	case ActionPassthrough:
		res.OutputURL = EncodeDataURL(payload.MediaType, payload.Bytes)
		return res, nil
	default: // redact
		res.Redacted = true
		res.OutputURL = EncodeDataURL("image/png", redacted)
		return res, nil
	}
}

// shareWithMapping adds OCR'd entity names to the session mapping so
// the text-side reversal can rewrite them when the LLM types them
// back. Same deterministic ordering as text.Handler.assignNew.
func (h *Handler) shareWithMapping(ctx context.Context, sessionID string, detections []presidio.ImageDetection) error {
	existing, err := h.cfg.Store.GetMapping(ctx, sessionID)
	if err != nil {
		return err
	}
	scratch := make(map[string]string, len(existing))
	for k, v := range existing {
		scratch[k] = v
	}
	newMap := map[string]string{}

	// Sort longest-first, alphabetical tie-break.
	sorted := make([]presidio.ImageDetection, 0, len(detections))
	for _, d := range detections {
		if strings.TrimSpace(d.Text) == "" {
			continue
		}
		sorted = append(sorted, d)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if len(sorted[i].Text) != len(sorted[j].Text) {
			return len(sorted[i].Text) > len(sorted[j].Text)
		}
		return sorted[i].Text < sorted[j].Text
	})

	for _, d := range sorted {
		real := strings.TrimSpace(d.Text)
		if _, ok := h.cfg.AllowList[strings.ToLower(real)]; ok {
			continue // never pseudonymize an allow-listed name
		}
		if _, ok := scratch[real]; ok {
			continue
		}
		if hasCaseInsensitiveKey(scratch, real) {
			continue
		}
		// No surrounding text for OCR'd entities, so no reserved haystack.
		var p string
		if h.cfg.Strategizer != nil {
			p = h.cfg.Strategizer.Assign(d.EntityType, real, scratch, "")
		} else {
			p = h.cfg.Pools.Assign(d.EntityType, usedSet(scratch), "")
		}
		newMap[real] = p
		scratch[real] = p

		if h.cfg.DecomposePersonNames && d.EntityType == "PERSON" {
			for cReal, cPseudo := range text.ComponentMappings(real, p) {
				if _, ok := scratch[cReal]; ok {
					continue
				}
				if hasCaseInsensitiveKey(scratch, cReal) {
					continue
				}
				newMap[cReal] = cPseudo
				scratch[cReal] = cPseudo
			}
		}
	}
	if len(newMap) == 0 {
		return nil
	}
	_, err = h.cfg.Store.AddMappings(ctx, sessionID, newMap)
	return err
}

func filterDetections(dets []presidio.ImageDetection, allowed []string) []presidio.ImageDetection {
	if len(allowed) == 0 {
		return dets
	}
	set := make(map[string]struct{}, len(allowed))
	for _, e := range allowed {
		set[e] = struct{}{}
	}
	out := make([]presidio.ImageDetection, 0, len(dets))
	for _, d := range dets {
		if _, ok := set[d.EntityType]; ok {
			out = append(out, d)
		}
	}
	return out
}

func hasCaseInsensitiveKey(m map[string]string, key string) bool {
	kl := strings.ToLower(key)
	for k := range m {
		if strings.ToLower(k) == kl {
			return true
		}
	}
	return false
}

func usedSet(m map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for _, v := range m {
		out[v] = struct{}{}
	}
	return out
}

func bytesDiffer(a, b []byte) bool {
	if len(a) != len(b) {
		return true
	}
	return !bytes.Equal(a, b)
}

// Silence unused-import warnings if we later remove helpers.
var _ = errors.New
var _ = fmt.Sprintf
