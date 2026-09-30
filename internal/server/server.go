// Package server wires the HTTP API: the Open Responses endpoints, the admin
// API, health and metrics.
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/boramuyar/ultimate-proxy/internal/adminauth"
	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/identity"
	"github.com/boramuyar/ultimate-proxy/internal/insights"
	"github.com/boramuyar/ultimate-proxy/internal/meter"
	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/pricing"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

type Server struct {
	cfg       *config.Config
	adminAuth *adminauth.Auth
	store     store.Store
	auth      *identity.Authenticator
	meter     *meter.Meter
	router    *Router
	prices    *pricing.Table
	// insights is nil when insights are disabled.
	insights *insights.Engine
	log      *slog.Logger
}

func New(cfg *config.Config, st store.Store, m *meter.Meter, log *slog.Logger) (*Server, error) {
	router, err := NewRouter(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, adminAuth: adminauth.New(cfg.Admin, log), store: st, auth: identity.NewAuthenticator(st), meter: m, router: router, prices: pricing.New(st, log), log: log}
	if !cfg.Insights.Disabled {
		var n insights.Notifier
		if wh := insights.NewWebhooks(cfg.Insights.WebhookURL, cfg.Insights.SlackWebhookURL, log); wh != nil {
			n = wh
		}
		s.insights = insights.NewEngine(cfg.Insights, st, n, log)
	}
	return s, nil
}

// Prices returns the price table. Callers reload it at startup and keep it
// fresh with Run.
func (s *Server) Prices() *pricing.Table { return s.prices }

// Insights returns the insight engine, or nil when insights are disabled.
func (s *Server) Insights() *insights.Engine { return s.insights }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("POST /v1/responses/compact", s.handleCompact)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	admin := http.NewServeMux()
	admin.HandleFunc("GET /admin/tenants", s.listTenants)
	admin.HandleFunc("POST /admin/tenants", s.createTenant)
	admin.HandleFunc("GET /admin/tenants/{id}/applications", s.listApplications)
	admin.HandleFunc("POST /admin/tenants/{id}/applications", s.createApplication)
	admin.HandleFunc("GET /admin/applications", s.listApplications)
	admin.HandleFunc("GET /admin/applications/{id}/keys", s.listKeys)
	admin.HandleFunc("POST /admin/applications/{id}/keys", s.createKey)
	admin.HandleFunc("DELETE /admin/keys/{id}", s.revokeKey)
	admin.HandleFunc("GET /admin/usage", s.usage)
	admin.HandleFunc("GET /admin/insights", s.listInsights)
	admin.HandleFunc("GET /admin/prices", s.listPrices)
	admin.HandleFunc("POST /admin/prices", s.addPrice)
	mux.Handle("/admin/", s.requireAdmin(admin))
	mux.Handle("/admin/auth/", s.adminAuth.Handler())
	return mux
}

// Bootstrap creates the tenants, applications and keys listed in the config.
func (s *Server) Bootstrap(ctx context.Context) error {
	for _, t := range s.cfg.Bootstrap {
		tenant, err := s.store.EnsureTenant(ctx, t.Tenant)
		if err != nil {
			return err
		}
		for _, a := range t.Applications {
			canAssert := a.CanAssertUsers == nil || *a.CanAssertUsers
			app, err := s.store.EnsureApplication(ctx, tenant.ID, a.Name, canAssert)
			if err != nil {
				return err
			}
			for _, key := range a.Keys {
				if key == "" {
					continue
				}
				if _, err := s.store.CreateKey(ctx, app.ID, identity.HashKey(key), identity.DisplayPrefix(key)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
