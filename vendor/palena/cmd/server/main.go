// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

// Command server is the entrypoint for palena-litellm-pseudonymizer.
// See context/PROTOCOL.md for the HTTP contract; see the README for the
// operating rules.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/config"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/httpapi"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/image"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/pseudonymizer"
	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/text"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	if cfg.APIKey == "" {
		logger.Warn("PALENA_PSEUDONYMIZER_API_KEY is empty — accepting all requests (dev mode)")
	}

	// Redis
	store, err := mapping.NewRedisStore(mapping.Config{
		URL:        cfg.RedisURL,
		KeyPrefix:  cfg.RedisKeyPrefix,
		SessionTTL: cfg.SessionTTL,
		Timeout:    cfg.RedisTimeout,
		PoolSize:   cfg.RedisPoolSize,
		Cluster:    cfg.RedisCluster,
	}, logger)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Presidio analyzer
	analyzer := presidio.NewAnalyzer(presidio.AnalyzerConfig{
		BaseURL:        cfg.AnalyzerURL,
		Timeout:        cfg.PresidioTimeout,
		ScoreThreshold: cfg.MinScoreThreshold(),
	})

	// Substitution strategy: pool (fictional names) for nominal identities,
	// token (<CREDIT_CARD_1>) for structured PII — per-entity configurable.
	pools := text.NewPools(cfg.PoolsMap())
	strategizer := text.NewStrategizer(text.StrategizerConfig{
		Pools:               pools,
		Overrides:           cfg.StrategyOverrides(),
		Default:             cfg.EntityStrategyDefault,
		DeterministicSecret: cfg.DeterministicSecret,
	})

	// Terms that are never pseudonymized (brand names, public figures,
	// words Presidio over-tags). Shared by the text and image handlers.
	allowList := cfg.AllowListSet()

	// Text handler
	textHandler := text.NewHandler(text.HandlerConfig{
		Analyzer:             analyzer,
		Store:                store,
		Strategizer:          strategizer,
		Entities:             cfg.Entities,
		Languages:            cfg.AnalyzeLanguages(),
		NoRestoreEntities:    splitCSV(os.Getenv("PALENA_PSEUDONYMIZER_NO_RESTORE_ENTITIES")),
		DecomposePersonNames: cfg.DecomposePersonNames,
		AllowList:            allowList,
		CommonWords:          cfg.CommonWordsSet(),
		ScoreThreshold:       cfg.ScoreThreshold,
		EntityThresholds:     cfg.EntityThresholds(),
	})

	// Image handler
	imageRedactor := presidio.NewImageRedactor(presidio.ImageRedactorConfig{
		BaseURL: cfg.ImageRedactorURL,
		Timeout: cfg.PresidioTimeout,
	})
	imageHandler := image.NewHandler(image.HandlerConfig{
		Redactor:             imageRedactor,
		Store:                store,
		Strategizer:          strategizer,
		Entities:             cfg.Entities,
		PIIAction:            image.PIIAction(cfg.PIIAction),
		BlockMessageTmpl:     cfg.BlockMessage,
		MaxImageBytes:        cfg.MaxImageBytes,
		MaxImagesPerRequest:  cfg.MaxImagesPerRequest,
		DecomposePersonNames: cfg.DecomposePersonNames,
		AllowList:            allowList,
	})

	// Orchestrator (text + optional image)
	orc := pseudonymizer.NewOrchestrator(textHandler).WithImageHandler(imageHandler)

	// HTTP layer
	guardrail := httpapi.NewHandler(httpapi.HandlerConfig{
		Orchestrator:    orc,
		Logger:          logger,
		MaxRequestBytes: cfg.MaxRequestBytes,
		MetadataKey:     cfg.SessionMetadataKey,
		APIKey:          cfg.APIKey,
	})

	// Session admin (erasure). Shares the guardrail's shared secret.
	sessions := httpapi.NewSessionsHandler(httpapi.SessionsHandlerConfig{
		Store:   store,
		APIKey:  cfg.APIKey,
		Timeout: cfg.RedisTimeout,
		Logger:  logger,
	})

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	router := httpapi.NewRouter(httpapi.RouterConfig{
		Handler:             guardrail,
		SessionsHandler:     sessions,
		Store:               store,
		ImageRedactorPinger: imageRedactor,
		ReadinessTimeout:    cfg.PresidioReadiness,
		Registry:            registry,
		Logger:              logger,
	})

	srv := &http.Server{
		Addr:           cfg.HTTPAddr,
		Handler:        router,
		ReadTimeout:    cfg.HTTPReadTimeout,
		WriteTimeout:   cfg.HTTPWriteTimeout,
		IdleTimeout:    cfg.HTTPIdleTimeout,
		MaxHeaderBytes: cfg.HTTPMaxHeaderBytes,
	}

	// Graceful shutdown wiring.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", cfg.HTTPAddr,
			"presidio_analyzer_url", cfg.AnalyzerURL,
			"presidio_image_redactor_url", cfg.ImageRedactorURL,
			"redis_url", redactRedisURL(cfg.RedisURL),
			"metadata_key", cfg.SessionMetadataKey,
			"max_request_bytes", cfg.MaxRequestBytes,
			"entities", cfg.Entities,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// redactRedisURL strips inline credentials before logging.
func redactRedisURL(raw string) string {
	scheme := ""
	rest := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme = raw[:i+3]
		rest = raw[i+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return scheme + rest
}

// splitCSV splits a comma-separated list, trimming blanks.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
