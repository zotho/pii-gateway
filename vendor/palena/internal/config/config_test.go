// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("PALENA_PSEUDONYMIZER_API_KEY", "")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.HTTPAddr)
	require.Equal(t, "http://presidio-analyzer:5001", cfg.AnalyzerURL)
	require.Equal(t, "redis://redis:6379/0", cfg.RedisURL)
	require.Equal(t, "palena:pseudonymizer", cfg.RedisKeyPrefix)
	require.Contains(t, cfg.PoolPerson, "Jordan Avery")
	require.Equal(t, "redact", cfg.PIIAction)
	// LOCATION is intentionally excluded from the default entity set to
	// avoid city-name geography drift.
	require.Equal(t, []string{"PERSON", "ORGANIZATION"}, cfg.Entities)
	require.NotContains(t, cfg.Entities, "LOCATION")
}

func TestLoad_InvalidThreshold(t *testing.T) {
	t.Setenv("PALENA_PSEUDONYMIZER_PRESIDIO_SCORE_THRESHOLD", "1.5")
	_, err := Load()
	require.Error(t, err)
}

func TestLoad_BadPIIAction(t *testing.T) {
	t.Setenv("PALENA_PSEUDONYMIZER_NON_TEXT_PII_ACTION", "murder")
	_, err := Load()
	require.Error(t, err)
}

func TestLoad_BadBlockMessage(t *testing.T) {
	t.Setenv("PALENA_PSEUDONYMIZER_NON_TEXT_BLOCK_MESSAGE", "no placeholder here")
	_, err := Load()
	require.Error(t, err)
}

func TestPoolsMap(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	m := cfg.PoolsMap()
	require.Contains(t, m, "PERSON")
	require.Contains(t, m, "ORGANIZATION")
	require.Contains(t, m, "LOCATION")
}

func TestLoad_Languages(t *testing.T) {
	t.Setenv("PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGES", "de, EN,fr")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, []string{"de", "en", "fr"}, cfg.AnalyzeLanguages())
}

func TestLoad_LanguageFallsBackToSingle(t *testing.T) {
	t.Setenv("PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGE", "it")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, []string{"it"}, cfg.AnalyzeLanguages())
}

func TestLoad_BadLanguage(t *testing.T) {
	t.Setenv("PALENA_PSEUDONYMIZER_PRESIDIO_LANGUAGES", "en,klingon")
	_, err := Load()
	require.ErrorContains(t, err, "klingon")
}

// Presidio's phone recognizer scores 0.4: the default override keeps phone
// numbers maskable under the 0.7 global threshold.
func TestLoad_EntityThresholds(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, map[string]float64{"PHONE_NUMBER": 0.4}, cfg.EntityThresholds())
	require.InDelta(t, 0.4, cfg.MinScoreThreshold(), 1e-9)

	t.Setenv("PALENA_PSEUDONYMIZER_ENTITY_SCORE_THRESHOLD", "PHONE_NUMBER:1.5")
	_, err = Load()
	require.ErrorContains(t, err, "ENTITY_SCORE_THRESHOLD")
}

func TestLoad_SessionTTL(t *testing.T) {
	cases := map[string]time.Duration{
		"":     time.Hour, // default 3600s
		"3600": time.Hour,
		"90":   90 * time.Second,
		"24h":  24 * time.Hour,
		"0":    0,
	}
	for in, want := range cases {
		t.Run("in="+in, func(t *testing.T) {
			if in != "" {
				t.Setenv("PALENA_PSEUDONYMIZER_REDIS_SESSION_TTL_SECONDS", in)
			}
			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, want, cfg.SessionTTL)
		})
	}
	for _, bad := range []string{"-1", "abc"} {
		t.Run("bad="+bad, func(t *testing.T) {
			t.Setenv("PALENA_PSEUDONYMIZER_REDIS_SESSION_TTL_SECONDS", bad)
			_, err := Load()
			require.Error(t, err)
		})
	}
}
