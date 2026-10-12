// Copyright (c) 2026 bitkaio LLC. All rights reserved.
// Licensed under the Apache License, Version 2.0. See LICENSE for details.

package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/mapping"
)

// Pinger is the subset of a dependency needed for the readiness probe.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RouterConfig groups the router's inputs.
type RouterConfig struct {
	Handler             *Handler
	SessionsHandler     *SessionsHandler
	Store               mapping.Store
	AnalyzerPinger      Pinger
	ImageRedactorPinger Pinger
	ReadinessTimeout    time.Duration
	Registry            *prometheus.Registry
	Logger              *slog.Logger
}

// NewRouter constructs a chi mux with all endpoints wired.
func NewRouter(cfg RouterConfig) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	if cfg.ReadinessTimeout <= 0 {
		cfg.ReadinessTimeout = 2 * time.Second
	}

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), cfg.ReadinessTimeout)
		defer cancel()
		type status struct {
			Ready bool              `json:"ready"`
			Deps  map[string]string `json:"deps"`
		}
		out := status{Ready: true, Deps: map[string]string{}}
		checkPing := func(name string, p Pinger) {
			if p == nil {
				return
			}
			if err := p.Ping(ctx); err != nil {
				out.Ready = false
				out.Deps[name] = "unreachable"
			} else {
				out.Deps[name] = "ok"
			}
		}
		if cfg.Store != nil {
			checkPing("redis", cfg.Store)
		}
		checkPing("presidio_analyzer", cfg.AnalyzerPinger)
		checkPing("presidio_image_redactor", cfg.ImageRedactorPinger)

		w.Header().Set("Content-Type", "application/json")
		if !out.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	if cfg.Registry != nil {
		r.Handle("/metrics", promhttp.HandlerFor(cfg.Registry, promhttp.HandlerOpts{}))
	}

	if cfg.Handler != nil {
		r.Post("/beta/litellm_basic_guardrail_api", cfg.Handler.ServeHTTP)
		// Wäg gateway stream codec (pre_call / during_call / post_call).
		r.Post("/v1/waeg/check", cfg.Handler.ServeWaegHTTP)
	}

	if cfg.SessionsHandler != nil {
		r.Delete("/sessions/{session_id}", cfg.SessionsHandler.Delete)
	}

	return r
}
