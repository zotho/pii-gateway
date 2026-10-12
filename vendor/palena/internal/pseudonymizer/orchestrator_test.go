// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package pseudonymizer

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/text"
)

type fakeAnalyzer struct{ dets []presidio.Detection }

func (f *fakeAnalyzer) Analyze(_ context.Context, _ string, _ []string, _ string) ([]presidio.Detection, error) {
	return f.dets, nil
}

func newOrc(t *testing.T, dets []presidio.Detection) *Orchestrator {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := mapping.NewRedisStoreFromClient(client, mapping.Config{SessionTTL: time.Minute, Timeout: time.Second}, nil)
	th := text.NewHandler(text.HandlerConfig{
		Analyzer: &fakeAnalyzer{dets: dets},
		Store:    store,
		Pools:    text.NewPools(map[string][]string{"PERSON": {"Alpha"}, "ORGANIZATION": {"OrgOne"}}),
		Entities: []string{"PERSON", "ORGANIZATION"},
	})
	return NewOrchestrator(th)
}

func TestOrchestrator_RequestModifies(t *testing.T) {
	o := newOrc(t, []presidio.Detection{
		{EntityType: "PERSON", Text: "Thomas Weber", Score: 0.9},
	})
	res, err := o.Handle(context.Background(), &Request{
		InputType: InputRequest,
		SessionID: "s1",
		Texts:     []string{"Please contact Thomas Weber."},
	})
	require.NoError(t, err)
	require.Equal(t, ActionIntervene, res.Action)
	require.Equal(t, []string{"Please contact Alpha."}, res.Texts)
	require.Equal(t, 1, res.Counters.EntitiesPseudonymized)
}

func TestOrchestrator_RequestNoModification(t *testing.T) {
	o := newOrc(t, nil)
	res, err := o.Handle(context.Background(), &Request{
		InputType: InputRequest,
		SessionID: "s1",
		Texts:     []string{"a quiet Sunday"},
	})
	require.NoError(t, err)
	require.Equal(t, ActionNone, res.Action)
	require.Nil(t, res.Texts)
}

func TestOrchestrator_ResponseReverses(t *testing.T) {
	o := newOrc(t, []presidio.Detection{
		{EntityType: "PERSON", Text: "Thomas Weber", Score: 0.9},
	})
	// First pre-call to populate the mapping.
	_, err := o.Handle(context.Background(), &Request{
		InputType: InputRequest, SessionID: "s1", Texts: []string{"Hi Thomas Weber"},
	})
	require.NoError(t, err)

	// Post-call sees the pseudonym in the LLM response.
	res, err := o.Handle(context.Background(), &Request{
		InputType: InputResponse, SessionID: "s1", Texts: []string{"Alpha reported success."},
	})
	require.NoError(t, err)
	require.Equal(t, ActionIntervene, res.Action)
	require.Equal(t, []string{"Thomas Weber reported success."}, res.Texts)
}

func TestOrchestrator_ResponseNoMappingIsNone(t *testing.T) {
	o := newOrc(t, nil)
	res, err := o.Handle(context.Background(), &Request{
		InputType: InputResponse, SessionID: "s1", Texts: []string{"nothing to reverse"},
	})
	require.NoError(t, err)
	require.Equal(t, ActionNone, res.Action)
}

func TestOrchestrator_DocumentsRejected(t *testing.T) {
	o := newOrc(t, nil)
	res, err := o.Handle(context.Background(), &Request{
		InputType: InputRequest, DocumentCount: 1,
	})
	require.NoError(t, err)
	require.Equal(t, ActionBlocked, res.Action)
	require.Equal(t, "documents_not_supported_in_v1", res.BlockedReason)
}

func TestOrchestrator_UnknownInputType(t *testing.T) {
	o := newOrc(t, nil)
	_, err := o.Handle(context.Background(), &Request{InputType: "other"})
	require.Error(t, err)
}
