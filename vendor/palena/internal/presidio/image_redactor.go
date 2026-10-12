// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package presidio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/errs"
)

// ImageDetection is one entity Presidio Image Redactor found. When the
// deployed Presidio container does not support /analyze (many public
// versions return 500 on it), the Text field is empty and callers
// cannot enrich the text-side session mapping from images. See
// context/IMAGE_PIPELINE.md.
type ImageDetection struct {
	EntityType string  `json:"entity_type"`
	Text       string  `json:"text,omitempty"`
	Score      float64 `json:"score"`
	Left       int     `json:"left"`
	Top        int     `json:"top"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
}

// ImageRedactorConfig configures the client.
type ImageRedactorConfig struct {
	BaseURL string
	Timeout time.Duration
}

// ImageRedactor is the HTTP client for the Image Redactor service.
type ImageRedactor struct {
	baseURL string
	timeout time.Duration
	http    *http.Client
}

// NewImageRedactor constructs the client.
func NewImageRedactor(cfg ImageRedactorConfig) *ImageRedactor {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	return &ImageRedactor{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		timeout: cfg.Timeout,
		http:    &http.Client{Timeout: cfg.Timeout},
	}
}

// Ping — used by the readiness handler.
func (c *ImageRedactor) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return errs.Wrap(errs.ErrPresidioUnavailable, "image_redactor health")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return errs.Wrap(errs.ErrPresidioUnavailable, "image_redactor health status %d", resp.StatusCode)
	}
	return nil
}

// Redact sends the image to POST /redact and returns the redacted PNG
// bytes. When the input has no PII, Presidio returns bytes that are
// visually identical (but may not be byte-identical due to re-encoding).
// The handler layer decides whether "PII was found" by comparing.
//
// color: RGB tuple string, e.g. "0, 0, 0" for black. Empty defaults to
// the Presidio default (black).
func (c *ImageRedactor) Redact(ctx context.Context, imageBytes []byte, color string) ([]byte, error) {
	if len(imageBytes) == 0 {
		return nil, nil
	}

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := writeImagePart(mw, "image", "image.png", imageBytes); err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}
	if color != "" {
		payload, _ := json.Marshal(map[string]string{"color_fill": color})
		if err := mw.WriteField("data", string(payload)); err != nil {
			return nil, fmt.Errorf("build multipart data field: %w", err)
		}
	} else {
		if err := mw.WriteField("data", "{}"); err != nil {
			return nil, fmt.Errorf("build multipart data field: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/redact", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.ErrPresidioUnavailable, "image_redactor /redact")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return nil, errs.Wrap(errs.ErrPresidioUnavailable, "image_redactor /redact status %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, errs.Wrap(errs.ErrPresidioUnavailable, "image_redactor /redact status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	return io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64 MiB cap
}

// Analyze best-effort calls POST /analyze to get bboxes + OCR text.
// Many public Presidio Image Redactor Docker images return 500 on this
// endpoint. In that case we return (nil, nil) and continue without
// enriching the text-side session mapping — the redacted image still
// gets sent to the LLM, we just can't reverse-substitute the person's
// name if the LLM types it back in text.
func (c *ImageRedactor) Analyze(ctx context.Context, imageBytes []byte) ([]ImageDetection, error) {
	if len(imageBytes) == 0 {
		return nil, nil
	}
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := writeImagePart(mw, "image", "image.png", imageBytes); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/analyze", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		// Best-effort: treat as "not supported" rather than fail-closed.
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil // graceful degradation
	}

	var out []ImageDetection
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil
	}
	return out, nil
}

func writeImagePart(mw *multipart.Writer, field, filename string, data []byte) error {
	// Custom content-type header because CreateFormFile hardcodes
	// application/octet-stream on some Go stdlibs which Presidio doesn't
	// mind — but this makes the intent explicit.
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}
