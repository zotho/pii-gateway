// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package presidio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
)

func newImageRedactor(t *testing.T, h http.HandlerFunc) *ImageRedactor {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewImageRedactor(ImageRedactorConfig{BaseURL: srv.URL})
}

func TestImageRedactor_RedactReturnsBytes(t *testing.T) {
	want := []byte("\x89PNG\r\n\x1a\nredacted content")
	c := newImageRedactor(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/redact", r.URL.Path)
		require.Contains(t, r.Header.Get("Content-Type"), "multipart/form-data")
		require.NoError(t, r.ParseMultipartForm(1<<20))
		require.NotNil(t, r.MultipartForm.File["image"])
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(want)
	})
	got, err := c.Redact(context.Background(), []byte("input-png"), "0, 0, 0")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestImageRedactor_RedactEmptyReturnsNil(t *testing.T) {
	c := NewImageRedactor(ImageRedactorConfig{BaseURL: "http://unused"})
	got, err := c.Redact(context.Background(), nil, "")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestImageRedactor_Redact5xxIsUnavailable(t *testing.T) {
	c := newImageRedactor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := c.Redact(context.Background(), []byte("x"), "")
	require.Error(t, err)
	require.True(t, errors.Is(err, errs.ErrPresidioUnavailable))
}

func TestImageRedactor_AnalyzeSuccess(t *testing.T) {
	c := newImageRedactor(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/analyze", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]ImageDetection{
			{EntityType: "PERSON", Text: "Alice", Score: 0.9, Left: 10, Top: 5, Width: 40, Height: 12},
		})
	})
	got, err := c.Analyze(context.Background(), []byte("x"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "Alice", got[0].Text)
}

func TestImageRedactor_AnalyzeGracefulOn500(t *testing.T) {
	// Public Presidio Docker images often return 500 on /analyze.
	// We must degrade gracefully rather than fail-closed.
	c := newImageRedactor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	got, err := c.Analyze(context.Background(), []byte("x"))
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestImageRedactor_MultipartFieldsFormat(t *testing.T) {
	// Ensure the multipart body includes both 'image' and 'data' fields
	// so Presidio's redact endpoint doesn't reject us.
	var receivedColor string
	c := newImageRedactor(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(1<<20))
		data := r.MultipartForm.Value["data"]
		require.NotEmpty(t, data)
		var payload map[string]string
		require.NoError(t, json.Unmarshal([]byte(data[0]), &payload))
		receivedColor = payload["color_fill"]
		_, _ = w.Write([]byte("ok"))
	})
	_, err := c.Redact(context.Background(), []byte("x"), "255, 0, 0")
	require.NoError(t, err)
	require.Equal(t, "255, 0, 0", receivedColor)
}

// Exercise the helper directly for coverage.
func TestWriteImagePart(t *testing.T) {
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	require.NoError(t, writeImagePart(mw, "image", "x.png", []byte("payload")))
	require.NoError(t, mw.Close())
	require.Contains(t, buf.String(), "payload")
}

// Silence unused-import warnings if we later remove helpers above.
var _ = io.EOF
