// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package mapping

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := NewRedisStoreFromClient(client, Config{
		KeyPrefix:  "palena:pseudonymizer",
		SessionTTL: time.Minute,
		Timeout:    5 * time.Second,
	}, nil)
	return s, mr
}

func TestRedisStore_EmptyMapping(t *testing.T) {
	s, _ := newStore(t)
	m, err := s.GetMapping(context.Background(), "s1")
	require.NoError(t, err)
	require.Empty(t, m)
}

func TestRedisStore_AddThenGet(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	merged, err := s.AddMappings(ctx, "s1", map[string]string{"Novartis": "Acme Corp"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Novartis": "Acme Corp"}, merged)

	loaded, err := s.GetMapping(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, merged, loaded)
}

func TestRedisStore_Delete(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, err := s.AddMappings(ctx, "s1", map[string]string{"Novartis": "Acme Corp"})
	require.NoError(t, err)

	existed, err := s.Delete(ctx, "s1")
	require.NoError(t, err)
	require.True(t, existed)

	m, err := s.GetMapping(ctx, "s1")
	require.NoError(t, err)
	require.Empty(t, m)

	// Deleting again is idempotent — no error, existed=false.
	existed, err = s.Delete(ctx, "s1")
	require.NoError(t, err)
	require.False(t, existed)
}

func TestRedisStore_ExistingWinsOnMerge(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, err := s.AddMappings(ctx, "s1", map[string]string{"Novartis": "Acme Corp"})
	require.NoError(t, err)
	// Try to overwrite with a different value.
	merged, err := s.AddMappings(ctx, "s1", map[string]string{"Novartis": "OtherCo"})
	require.NoError(t, err)
	require.Equal(t, "Acme Corp", merged["Novartis"])
}

func TestRedisStore_ReverseMapping(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, err := s.AddMappings(ctx, "s1", map[string]string{
		"Novartis": "Acme Corp",
		"Roche":    "Beta Industries",
	})
	require.NoError(t, err)
	rev, err := s.GetReverseMapping(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "Novartis", rev["Acme Corp"])
	require.Equal(t, "Roche", rev["Beta Industries"])
}

func TestRedisStore_CorruptJSONIsReset(t *testing.T) {
	s, mr := newStore(t)
	ctx := context.Background()
	mr.HSet("palena:pseudonymizer:s1", "mapping", "{not json")
	m, err := s.GetMapping(ctx, "s1")
	require.NoError(t, err)
	require.Empty(t, m)
}

func TestRedisStore_TTLRefreshed(t *testing.T) {
	s, mr := newStore(t)
	ctx := context.Background()
	_, err := s.AddMappings(ctx, "s1", map[string]string{"a": "b"})
	require.NoError(t, err)

	mr.FastForward(30 * time.Second)
	_, err = s.GetMapping(ctx, "s1")
	require.NoError(t, err)
	mr.FastForward(45 * time.Second) // still within 60s window since last read refresh
	m, err := s.GetMapping(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "b", m["a"])
}

func TestRedisStore_SessionsIsolated(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, err := s.AddMappings(ctx, "s1", map[string]string{"Novartis": "Alpha"})
	require.NoError(t, err)
	_, err = s.AddMappings(ctx, "s2", map[string]string{"Novartis": "Beta"})
	require.NoError(t, err)

	m1, _ := s.GetMapping(ctx, "s1")
	m2, _ := s.GetMapping(ctx, "s2")
	require.Equal(t, "Alpha", m1["Novartis"])
	require.Equal(t, "Beta", m2["Novartis"])
}

func TestRedisStore_AddEmptyIsNoop(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, err := s.AddMappings(ctx, "s1", map[string]string{"Novartis": "Alpha"})
	require.NoError(t, err)
	merged, err := s.AddMappings(ctx, "s1", nil)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Novartis": "Alpha"}, merged)
}

func TestRedisStore_Ping(t *testing.T) {
	s, mr := newStore(t)
	require.NoError(t, s.Ping(context.Background()))
	mr.Close()
	require.Error(t, s.Ping(context.Background()))
}

func TestRedisStore_ZeroTTLNeverExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := NewRedisStoreFromClient(client, Config{SessionTTL: 0, Timeout: 5 * time.Second}, nil)
	ctx := context.Background()
	_, err := s.AddMappings(ctx, "s1", map[string]string{"a": "b"})
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), mr.TTL("palena:pseudonymizer:s1"))

	mr.FastForward(365 * 24 * time.Hour)
	m, err := s.GetMapping(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "b", m["a"])
	require.Equal(t, time.Duration(0), mr.TTL("palena:pseudonymizer:s1"))
}

func TestRedisStore_NewRedisStoreAllowsZeroTTL(t *testing.T) {
	_, err := NewRedisStore(Config{URL: "redis://127.0.0.1:1/0", SessionTTL: 0}, nil)
	require.NoError(t, err)
	_, err = NewRedisStore(Config{URL: "redis://127.0.0.1:1/0", SessionTTL: -1}, nil)
	require.Error(t, err)
}
