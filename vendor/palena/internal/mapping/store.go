// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package mapping stores real↔pseudonym mappings in Redis, one hash
// per session. See context/SESSION_AND_STATE.md § "Redis schema".
package mapping

import "context"

// Store is the abstraction used by the pseudonymizer. It is
// implemented by RedisStore in production and by an in-memory fake
// in tests.
type Store interface {
	// GetMapping loads the current real→pseudonym mapping for the
	// session. Empty map if none yet. Never returns nil.
	GetMapping(ctx context.Context, sessionID string) (map[string]string, error)

	// AddMappings merges newMappings into the session's mapping and
	// returns the merged result. Existing entries win — if
	// newMappings["Novartis"] = "X" but the session already has
	// "Novartis" = "AcmeCorp", AcmeCorp stays. This is what makes
	// multi-turn conversations stable.
	AddMappings(ctx context.Context, sessionID string, newMappings map[string]string) (map[string]string, error)

	// GetReverseMapping returns pseudonym→real for the session.
	// Derived from GetMapping on the fly.
	GetReverseMapping(ctx context.Context, sessionID string) (map[string]string, error)

	// Delete removes a session's mapping entirely (GDPR right-to-erasure /
	// explicit session teardown). Returns whether a mapping existed. Deleting
	// a non-existent session is not an error (idempotent).
	Delete(ctx context.Context, sessionID string) (existed bool, err error)

	// Ping verifies the store is reachable. Called by /readyz.
	Ping(ctx context.Context) error

	// Close releases underlying resources on shutdown.
	Close() error
}
