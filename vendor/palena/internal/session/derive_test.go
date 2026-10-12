// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDerive_PriorityChain(t *testing.T) {
	cases := []struct {
		name    string
		in      Input
		wantID  string
		wantSrc Source
	}{
		{
			name: "metadata key wins over everything",
			in: Input{
				StructuredMessages: []map[string]any{
					{"role": "user", "metadata": map[string]any{"session_id": "sess-1"}},
				},
				EndUserID: "eu-1",
				TraceID:   "tr-1",
				CallID:    "c-1",
			},
			wantID:  "sess-1",
			wantSrc: SourceMetadataKey,
		},
		{
			name: "newest message wins",
			in: Input{
				StructuredMessages: []map[string]any{
					{"role": "user", "metadata": map[string]any{"session_id": "old"}},
					{"role": "assistant"},
					{"role": "user", "metadata": map[string]any{"session_id": "new"}},
				},
			},
			wantID: "new", wantSrc: SourceMetadataKey,
		},
		{
			name:    "end user id when no metadata",
			in:      Input{EndUserID: "customer-42"},
			wantID:  "customer-42",
			wantSrc: SourceEndUser,
		},
		{
			name:    "trace id next",
			in:      Input{TraceID: "abc123"},
			wantID:  "abc123",
			wantSrc: SourceTraceID,
		},
		{
			name:    "call id fallback",
			in:      Input{CallID: "chatcmpl-xyz"},
			wantID:  "chatcmpl-xyz",
			wantSrc: SourceCallID,
		},
		{
			name:    "synthetic when everything empty",
			in:      Input{},
			wantSrc: SourceSynthetic,
		},
		{
			name: "custom metadata key override",
			in: Input{
				MetadataKey: "chat_id",
				StructuredMessages: []map[string]any{
					{"role": "user", "metadata": map[string]any{"chat_id": "abc"}},
				},
			},
			wantID: "abc", wantSrc: SourceMetadataKey,
		},
		{
			name: "metadata field not a map is ignored",
			in: Input{
				StructuredMessages: []map[string]any{
					{"role": "user", "metadata": "oops"},
				},
				EndUserID: "eu-only",
			},
			wantID: "eu-only", wantSrc: SourceEndUser,
		},
		{
			name: "whitespace-only metadata is ignored",
			in: Input{
				StructuredMessages: []map[string]any{
					{"role": "user", "metadata": map[string]any{"session_id": "   "}},
				},
				EndUserID: "eu-1",
			},
			wantID: "eu-1", wantSrc: SourceEndUser,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, src := Derive(tc.in)
			require.Equal(t, tc.wantSrc, src)
			if tc.wantID != "" {
				require.Equal(t, tc.wantID, id)
			} else {
				require.True(t, strings.HasPrefix(id, "anon-"))
			}
		})
	}
}

func TestDerive_NonPrintableIsHashed(t *testing.T) {
	// Newlines / null bytes in the derived id are not safe as Redis keys.
	id, src := Derive(Input{EndUserID: "abc\ndef"})
	require.Equal(t, SourceEndUser, src)
	require.True(t, strings.HasPrefix(id, "hashed-"), "got %s", id)
	require.Equal(t, id, mustDerive(Input{EndUserID: "abc\ndef"}), "must be deterministic")
}

func TestDerive_TooLongIsHashed(t *testing.T) {
	long := strings.Repeat("a", 300)
	id, _ := Derive(Input{EndUserID: long})
	require.True(t, strings.HasPrefix(id, "hashed-"))
}

func mustDerive(in Input) string {
	id, _ := Derive(in)
	return id
}
