// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package text

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// Pools holds the list of fictional names available for each entity
// type. See context/PSEUDONYMIZATION_ALGORITHM.md § "Pool assignment"
// for the exact assignment algorithm.
type Pools struct {
	byType map[string][]string
}

// syntheticPrefix returns the fallback prefix used when a pool is
// exhausted. Unknown types fall back to a TitleCase of the type name.
var syntheticPrefix = map[string]string{
	"PERSON":       "Person",
	"ORGANIZATION": "Organization",
	"LOCATION":     "Location",
}

// NewPools constructs a Pools from a map[type]list of names.
func NewPools(pools map[string][]string) *Pools {
	byType := make(map[string][]string, len(pools))
	for k, v := range pools {
		cleaned := make([]string, 0, len(v))
		for _, entry := range v {
			e := strings.TrimSpace(entry)
			if e != "" {
				cleaned = append(cleaned, e)
			}
		}
		byType[k] = cleaned
	}
	return &Pools{byType: byType}
}

// Assign returns the next unused pseudonym for entityType, given the
// current mapping's values (any pseudonym already assigned).
//
// The chosen pseudonym is NOT added to `used` — callers add it via
// their own scratch map so multiple concurrent assignments in one
// request don't collide.
//
// `reserved` is a lower-cased haystack the chosen pseudonym must not be a
// substring of (collision hardening); pass "" to disable.
func (p *Pools) Assign(entityType string, used map[string]struct{}, reserved string) string {
	for _, candidate := range p.byType[entityType] {
		if _, taken := used[candidate]; taken {
			continue
		}
		if reservedContains(reserved, candidate) {
			continue
		}
		return candidate
	}
	return p.syntheticFallback(entityType, used, reserved)
}

// AssignDeterministic picks a pool pseudonym by a keyed HMAC of the real
// value, so the same real name maps to the same pseudonym across sessions.
// It starts at the HMAC-derived slot and probes forward (mod pool size) to
// the first candidate that is neither already used nor reserved, so
// within-session collisions between two different real values still resolve
// to distinct pseudonyms (at the cost of cross-session stability for the
// loser of the collision). Falls back to the synthetic scheme when the pool
// is empty or fully taken.
func (p *Pools) AssignDeterministic(entityType, real string, used map[string]struct{}, reserved string, secret []byte) string {
	pool := p.byType[entityType]
	if len(pool) == 0 {
		return p.syntheticFallback(entityType, used, reserved)
	}
	start := int(hmacIndex(secret, strings.ToLower(strings.TrimSpace(real)), uint64(len(pool))))
	for i := 0; i < len(pool); i++ {
		candidate := pool[(start+i)%len(pool)]
		if _, taken := used[candidate]; taken {
			continue
		}
		if reservedContains(reserved, candidate) {
			continue
		}
		return candidate
	}
	return p.syntheticFallback(entityType, used, reserved)
}

// syntheticFallback returns a "Prefix-N" pseudonym for the smallest positive
// N that is unused and not reserved. Used when a pool is exhausted or empty.
func (p *Pools) syntheticFallback(entityType string, used map[string]struct{}, reserved string) string {
	prefix := syntheticPrefix[entityType]
	if prefix == "" {
		// Unknown entity type: TitleCase the type name once.
		prefix = titleCase(entityType)
	}
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s-%d", prefix, n)
		if _, taken := used[candidate]; taken {
			continue
		}
		if reservedContains(reserved, candidate) {
			continue
		}
		return candidate
	}
}

// hmacIndex returns HMAC-SHA256(secret, value) reduced into [0, mod).
func hmacIndex(secret []byte, value string, mod uint64) uint64 {
	if mod == 0 {
		return 0
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(value))
	sum := mac.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8]) % mod
}

// usedFromMap returns a set of values from a real→pseudo mapping.
// Helper for callers.
func usedFromMap(m map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for _, v := range m {
		out[v] = struct{}{}
	}
	return out
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	lowered := strings.ToLower(s)
	return strings.ToUpper(lowered[:1]) + lowered[1:]
}
