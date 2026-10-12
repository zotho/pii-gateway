// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package audit provides typed counters and log fields for the
// pseudonymizer service. The API is deliberately narrow: it accepts
// only fixed-vocabulary values (entity types, error kinds, block
// reasons) so real names / pseudonyms cannot leak into metrics or logs
// by construction.
//
// See context/FAILURE_MODES.md § "No-PII invariant".
package audit

import (
	"crypto/sha256"
	"encoding/hex"
)

// EntityType is a fixed vocabulary of PII categories. Anything not in
// this set is normalized to "OTHER" before being used as a metric label
// to keep Prometheus cardinality bounded.
type EntityType string

const (
	EntityPerson       EntityType = "PERSON"
	EntityOrganization EntityType = "ORGANIZATION"
	EntityLocation     EntityType = "LOCATION"
	EntityOther        EntityType = "OTHER"
)

// NormalizeEntityType maps free-form Presidio entity types onto the
// fixed vocabulary. Unknown types become "OTHER".
func NormalizeEntityType(raw string) EntityType {
	switch EntityType(raw) {
	case EntityPerson, EntityOrganization, EntityLocation:
		return EntityType(raw)
	}
	return EntityOther
}

// SessionIDHash returns a short deterministic hash suitable for
// including in logs and metric labels without revealing the session id
// itself. 12 hex chars = 48 bits — plenty for correlation, useless for
// enumeration.
func SessionIDHash(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:6])
}

// Counters is the aggregated counter set for a single guardrail
// invocation. All fields are safe to log — they are counts and typed
// enums, never PII.
type Counters struct {
	EntitiesDetected      int
	EntitiesPseudonymized int
	NewMappingsCreated    int
	EntityTypes           map[EntityType]int
	ImagesProcessed       int
	ImagesPIIFound        int
	ImagesRedacted        int
	DocumentsProcessed    int
	SessionMappingSize    int
}

// NewCounters returns a zero-value counter set ready to be mutated.
func NewCounters() *Counters {
	return &Counters{EntityTypes: map[EntityType]int{}}
}

// RecordEntities increments the detection counters. count is the
// number of times entityType was seen this call. Safe to call with 0
// (no-op).
func (c *Counters) RecordEntities(entityType EntityType, count int) {
	if count <= 0 {
		return
	}
	c.EntitiesDetected += count
	c.EntitiesPseudonymized += count
	c.EntityTypes[entityType] += count
}

// EntityTypesAsMap returns a map[string]int suitable for structured
// logging (slog cannot natively marshal a map keyed by our EntityType
// alias).
func (c *Counters) EntityTypesAsMap() map[string]int {
	out := make(map[string]int, len(c.EntityTypes))
	for k, v := range c.EntityTypes {
		out[string(k)] = v
	}
	return out
}
