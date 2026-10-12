// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
)

func newSessionsServer(t *testing.T, apiKey string) (http.Handler, mapping.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{
		SessionTTL: time.Minute, Timeout: time.Second,
	}, nil)
	sh := NewSessionsHandler(SessionsHandlerConfig{
		Store:  store,
		APIKey: apiKey,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return NewRouter(RouterConfig{SessionsHandler: sh, Store: store}), store
}

func del(t *testing.T, srv http.Handler, path, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestSessions_Delete_ExistingReturnsDeletedTrue(t *testing.T) {
	srv, store := newSessionsServer(t, "")
	_, err := store.AddMappings(context.Background(), "s1", map[string]string{"Novartis": "Acme Corp"})
	require.NoError(t, err)

	rec := del(t, srv, "/sessions/s1", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]bool
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body["deleted"])

	m, err := store.GetMapping(context.Background(), "s1")
	require.NoError(t, err)
	require.Empty(t, m)
}

func TestSessions_Delete_MissingReturnsDeletedFalse(t *testing.T) {
	srv, _ := newSessionsServer(t, "")
	rec := del(t, srv, "/sessions/nope", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]bool
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.False(t, body["deleted"])
}

func TestSessions_Delete_RequiresAPIKey(t *testing.T) {
	srv, store := newSessionsServer(t, "secret")
	_, err := store.AddMappings(context.Background(), "s1", map[string]string{"a": "b"})
	require.NoError(t, err)

	// Wrong / missing key → 401, mapping untouched.
	require.Equal(t, http.StatusUnauthorized, del(t, srv, "/sessions/s1", "").Code)
	require.Equal(t, http.StatusUnauthorized, del(t, srv, "/sessions/s1", "wrong").Code)
	m, _ := store.GetMapping(context.Background(), "s1")
	require.NotEmpty(t, m)

	// Correct key → 200.
	require.Equal(t, http.StatusOK, del(t, srv, "/sessions/s1", "secret").Code)
}
