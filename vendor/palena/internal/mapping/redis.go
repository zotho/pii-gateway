// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package mapping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
)

// Config controls the Redis client + session TTL / key prefix.
type Config struct {
	URL        string
	KeyPrefix  string
	SessionTTL time.Duration
	Timeout    time.Duration
	PoolSize   int
	Cluster    bool
}

// RedisStore implements Store against a real Redis (or miniredis in tests).
type RedisStore struct {
	client    redis.UniversalClient
	keyPrefix string
	ttl       time.Duration
	timeout   time.Duration
	logger    *slog.Logger
}

// atomicAddMappings is a Lua script that reads the current mapping,
// merges newMappings with "existing wins" semantics, writes back only
// if changed, and always refreshes the TTL. All in one atomic step.
//
// KEYS[1]  = session hash key
// ARGV[1]  = JSON of newMappings ({"real": "pseudo", ...})
// ARGV[2]  = "mapping" (field name)
// ARGV[3]  = TTL in seconds
//
// Returns JSON of the merged mapping.
const atomicAddMappings = `
local existing_json = redis.call("HGET", KEYS[1], ARGV[2])
local merged = {}
if existing_json then
  merged = cjson.decode(existing_json)
end
local new_map = cjson.decode(ARGV[1])
local changed = false
for k, v in pairs(new_map) do
  if merged[k] == nil then
    merged[k] = v
    changed = true
  end
end
local merged_json
if next(merged) == nil then
  merged_json = "{}"
else
  merged_json = cjson.encode(merged)
end
if changed then
  redis.call("HSET", KEYS[1], ARGV[2], merged_json)
end
if tonumber(ARGV[3]) > 0 then
  redis.call("EXPIRE", KEYS[1], tonumber(ARGV[3]))
else
  redis.call("PERSIST", KEYS[1])
end
return merged_json
`

const mappingField = "mapping"

// NewRedisStore connects to Redis using the given config. The
// connection is lazy — Ping() at readiness time confirms reachability.
func NewRedisStore(cfg Config, logger *slog.Logger) (*RedisStore, error) {
	if cfg.URL == "" {
		return nil, errors.New("redis url required")
	}
	if cfg.SessionTTL < 0 {
		return nil, errors.New("redis session ttl must be >= 0 (0 = no expiry)")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "palena:pseudonymizer"
	}
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 10
	}

	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opts.PoolSize = cfg.PoolSize
	opts.ReadTimeout = cfg.Timeout
	opts.WriteTimeout = cfg.Timeout

	client := redis.NewClient(opts)
	return &RedisStore{
		client:    client,
		keyPrefix: cfg.KeyPrefix,
		ttl:       cfg.SessionTTL,
		timeout:   cfg.Timeout,
		logger:    logger,
	}, nil
}

// NewRedisStoreFromClient wraps an existing client (useful for tests
// with miniredis).
func NewRedisStoreFromClient(client redis.UniversalClient, cfg Config, logger *slog.Logger) *RedisStore {
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "palena:pseudonymizer"
	}
	if cfg.SessionTTL < 0 {
		cfg.SessionTTL = time.Hour
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &RedisStore{
		client:    client,
		keyPrefix: cfg.KeyPrefix,
		ttl:       cfg.SessionTTL,
		timeout:   cfg.Timeout,
		logger:    logger,
	}
}

func (s *RedisStore) key(sessionID string) string {
	return s.keyPrefix + ":" + sessionID
}

// Ping — used by the readiness handler.
func (s *RedisStore) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.client.Ping(ctx).Err(); err != nil {
		return errs.Wrap(errs.ErrMappingStoreUnavailable, "ping")
	}
	return nil
}

// Close — used by graceful shutdown.
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// GetMapping loads and JSON-decodes the mapping field. TTL is refreshed
// as a side effect.
func (s *RedisStore) GetMapping(ctx context.Context, sessionID string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	key := s.key(sessionID)
	raw, err := s.client.HGet(ctx, key, mappingField).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, errs.Wrap(errs.ErrMappingStoreUnavailable, "hget %s", key)
	}
	// Refresh TTL (or clear it when ttl == 0) — best-effort, ignore errors.
	if s.ttl > 0 {
		_ = s.client.Expire(ctx, key, s.ttl).Err()
	} else {
		_ = s.client.Persist(ctx, key).Err()
	}

	if errors.Is(err, redis.Nil) || raw == "" {
		return map[string]string{}, nil
	}

	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		if s.logger != nil {
			s.logger.Warn("corrupt mapping payload, resetting",
				"session_id_hash", hashID(sessionID),
				"err", err.Error(),
			)
		}
		return map[string]string{}, nil
	}
	return out, nil
}

// AddMappings merges via the atomic Lua script and returns the full
// resulting mapping.
func (s *RedisStore) AddMappings(ctx context.Context, sessionID string, newMappings map[string]string) (map[string]string, error) {
	if len(newMappings) == 0 {
		return s.GetMapping(ctx, sessionID)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	key := s.key(sessionID)
	newJSON, err := json.Marshal(newMappings)
	if err != nil {
		return nil, fmt.Errorf("marshal new mappings: %w", err)
	}

	res, err := s.client.Eval(ctx, atomicAddMappings,
		[]string{key},
		string(newJSON), mappingField, int(s.ttl.Seconds()),
	).Result()
	if err != nil {
		return nil, errs.Wrap(errs.ErrMappingStoreUnavailable, "eval add mappings")
	}
	merged, ok := res.(string)
	if !ok {
		return nil, errs.Wrap(errs.ErrMappingStoreUnavailable, "unexpected eval return type %T", res)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(merged), &out); err != nil {
		return nil, errs.Wrap(errs.ErrMappingStoreUnavailable, "decode merged mapping")
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}

// Delete removes the session's hash entirely. Idempotent — deleting a
// missing key returns existed=false with no error.
func (s *RedisStore) Delete(ctx context.Context, sessionID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	n, err := s.client.Del(ctx, s.key(sessionID)).Result()
	if err != nil {
		return false, errs.Wrap(errs.ErrMappingStoreUnavailable, "del %s", s.key(sessionID))
	}
	return n > 0, nil
}

// GetReverseMapping loads the forward mapping and inverts it in memory.
func (s *RedisStore) GetReverseMapping(ctx context.Context, sessionID string) (map[string]string, error) {
	fwd, err := s.GetMapping(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rev := make(map[string]string, len(fwd))
	for k, v := range fwd {
		rev[v] = k
	}
	return rev, nil
}

// hashID keeps corrupt-payload log lines free of session id in clear.
// Duplicates the audit package's helper to avoid an import cycle risk.
func hashID(s string) string {
	// Small inline SHA-256 truncation. Slightly less than the audit
	// version to keep this file tiny — 8 hex chars is enough for
	// debug correlation.
	if s == "" {
		return ""
	}
	// Use a lightweight FNV-ish trick — the audit package owns the
	// canonical hash used in metrics; this helper only appears in
	// warning logs and never as a metric label.
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)[:8]
}
