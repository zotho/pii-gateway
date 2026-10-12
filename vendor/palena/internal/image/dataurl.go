// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package image is the image PII flow: decode LiteLLM's images[]
// entries (data URLs or remote URLs), run them through Presidio Image
// Redactor, and emit redacted PNGs back. See context/IMAGE_PIPELINE.md.
package image

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Payload is a decoded image + metadata ready to be sent to Presidio.
type Payload struct {
	Bytes     []byte
	MediaType string // e.g. "image/png"; empty when unknown
}

// DecodeDataURL parses a "data:<media>;base64,<payload>" URL.
//   - Returns (Payload, true) when the input IS a data URL and decodes.
//   - Returns (nil, false) with no error when it's clearly NOT a data URL
//     (i.e. does not start with "data:"). Callers can then try FetchURL.
//   - Returns (nil, true) with an error when the data URL is malformed.
func DecodeDataURL(s string) (*Payload, bool, error) {
	if !strings.HasPrefix(s, "data:") {
		return nil, false, nil
	}
	rest := strings.TrimPrefix(s, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil, true, errors.New("data url missing comma separator")
	}
	metadata := rest[:comma]
	payload := rest[comma+1:]

	// metadata looks like "image/png;base64" or just "image/png".
	mediaType := ""
	isBase64 := false
	for _, part := range strings.Split(metadata, ";") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if p == "base64" {
			isBase64 = true
			continue
		}
		if strings.Contains(p, "/") {
			mediaType = p
		}
	}
	if !isBase64 {
		// Plain URL-encoded data URLs are legal but nobody sends
		// images that way. Reject explicitly rather than half-support.
		return nil, true, errors.New("only base64 data urls are supported")
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Some clients strip padding — retry with RawStdEncoding.
		raw, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return nil, true, fmt.Errorf("decode base64: %w", err)
		}
	}
	return &Payload{Bytes: raw, MediaType: mediaType}, true, nil
}

// EncodeDataURL emits a "data:image/png;base64,…" URL.
func EncodeDataURL(mediaType string, data []byte) string {
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// FetchURL downloads a remote URL and returns the bytes. The caller
// provides a byte cap; anything beyond is treated as an error to
// protect Presidio + our own memory. See context/IMAGE_PIPELINE.md.
func FetchURL(ctx context.Context, client *http.Client, url string, maxBytes int64) (*Payload, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("fetch %s: body exceeds %d bytes", url, maxBytes)
	}
	mediaType := resp.Header.Get("Content-Type")
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	return &Payload{Bytes: body, MediaType: mediaType}, nil
}
