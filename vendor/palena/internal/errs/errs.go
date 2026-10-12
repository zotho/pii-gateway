// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Package errs defines sentinel errors + typed wrappers used across the
// pseudonymizer service. The HTTP layer maps these to specific status
// codes per context/FAILURE_MODES.md.
package errs

import (
	"errors"
	"fmt"
)

var (
	// ErrPresidioUnavailable is returned when Presidio Analyzer /
	// Image Redactor is unreachable, times out, or returns a 5xx.
	// HTTP maps to 502 on pre-call.
	ErrPresidioUnavailable = errors.New("presidio_unreachable")

	// ErrPresidioMalformed is returned when Presidio returns a
	// response we cannot decode. HTTP maps to 502 on pre-call.
	ErrPresidioMalformed = errors.New("presidio_malformed")

	// ErrMappingStoreUnavailable is returned when Redis is unreachable
	// or a Redis command fails. HTTP maps to 502 on pre-call.
	ErrMappingStoreUnavailable = errors.New("mapping_store_unreachable")

	// ErrSessionDerivationFailed is returned when we cannot derive
	// any session id from the request (bad wiring). HTTP 400.
	ErrSessionDerivationFailed = errors.New("session_id_derivation_failed")

	// ErrRequestTooLarge — HTTP 413.
	ErrRequestTooLarge = errors.New("request_too_large")

	// ErrImageTooLarge — HTTP 413.
	ErrImageTooLarge = errors.New("image_too_large")

	// ErrImageFetchFailed — could not fetch a remote image URL.
	ErrImageFetchFailed = errors.New("image_fetch_failed")
)

// BlockDecision signals that the guardrail decided to block the LLM
// call for a user-visible reason (not an error). The HTTP layer
// converts this to `action: BLOCKED` with the given reason.
type BlockDecision struct {
	Reason string
}

func (b *BlockDecision) Error() string { return b.Reason }

// NewBlock returns a *BlockDecision for the given reason code.
func NewBlock(reason string) *BlockDecision { return &BlockDecision{Reason: reason} }

// AsBlock reports whether err is a *BlockDecision and returns it.
func AsBlock(err error) (*BlockDecision, bool) {
	var b *BlockDecision
	if errors.As(err, &b) {
		return b, true
	}
	return nil, false
}

// Wrap adds context to an error while preserving errors.Is/As chains.
func Wrap(err error, format string, a ...any) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(a, err)...)
}
