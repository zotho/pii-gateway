// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package image

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeDataURL_Base64PNG(t *testing.T) {
	raw := []byte("\x89PNG\r\n\x1a\nfake")
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	p, ok, err := DecodeDataURL(url)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, raw, p.Bytes)
	require.Equal(t, "image/png", p.MediaType)
}

func TestDecodeDataURL_NonDataURLReturnsFalse(t *testing.T) {
	_, ok, err := DecodeDataURL("https://example.com/img.png")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestDecodeDataURL_RejectsPlainDataURL(t *testing.T) {
	_, ok, err := DecodeDataURL("data:image/png,hello")
	require.Error(t, err)
	require.True(t, ok)
}

func TestDecodeDataURL_BadBase64(t *testing.T) {
	_, ok, err := DecodeDataURL("data:image/png;base64,!!!not base64!!!")
	require.Error(t, err)
	require.True(t, ok)
}

func TestEncodeDataURL_DefaultsToPNG(t *testing.T) {
	got := EncodeDataURL("", []byte("abc"))
	require.Equal(t, "data:image/png;base64,YWJj", got)
}

func TestFetchURL_Success(t *testing.T) {
	body := []byte("\x89PNG remote content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	p, err := FetchURL(context.Background(), nil, srv.URL, 1<<20)
	require.NoError(t, err)
	require.Equal(t, body, p.Bytes)
	require.Equal(t, "image/png", p.MediaType)
}

func TestFetchURL_SizeCapEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 100))
	}))
	defer srv.Close()
	_, err := FetchURL(context.Background(), nil, srv.URL, 50)
	require.Error(t, err)
}

func TestFetchURL_400IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := FetchURL(context.Background(), nil, srv.URL, 1<<20)
	require.Error(t, err)
}
