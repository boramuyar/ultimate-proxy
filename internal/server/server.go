// Package server wires the HTTP API: the Open Responses endpoints, the admin
// API, health and metrics.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/boramuyar/ultimate-proxy/internal/adminauth"
	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/identity"
	"github.com/boramuyar/ultimate-proxy/internal/insights"
	"github.com/boramuyar/ultimate-proxy/internal/limits"
	"github.com/boramuyar/ultimate-proxy/internal/meter"
	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/pricing"
	"github.com/boramuyar/ultimate-proxy/internal/provider"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

type Server struct {
	cfg       *config.Config
	adminAuth *adminauth.Auth
	store     store.Store
	auth      identity.Chain
	apiKeys   *identity.APIKeys
	tokens    *identity.ProxyTokens
	meter     *meter.Meter
	// providers are the upstreams, by the name clients use in the path.
	providers map[string]*provider.OpenAI
	prices    *pricing.Table
	limits    *limits.Engine
	// insights is nil when insights are disabled.
	insights *insights.Engine
	log      *slog.Logger
}

func New(cfg *config.Config, st store.Store, m *meter.Meter, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, adminAuth: adminauth.New(cfg.Admin, log), store: st, meter: m, providers: map[string]*provider.OpenAI{}, prices: pricing.New(st, log), log: log}
	s.adminAuth.OnSignIn = s.auditSignIn
	client := provider.NewHTTPClient(cfg.ResponseHeaderTimeout)
	for _, p := range cfg.Providers {
		s.providers[p.Name] = provider.NewOpenAI(p.Name, p.BaseURL, p.APIKey, p.Headers, client)
	}
	// JWTs go first: API keys accept any token, since keys in the config
	// need not carry the up_ prefix.
	if len(cfg.Auth.JWT) > 0 {
		s.auth = append(s.auth, identity.NewJWTs(cfg.Auth.JWT, st))
	}
	s.tokens = identity.NewProxyTokens(st)
	s.auth = append(s.auth, s.tokens)
	s.apiKeys = identity.NewAPIKeys(st)
	if cfg.Auth.APIKeysEnabled() {
		s.auth = append(s.auth, s.apiKeys)
	}
	var counter limits.Counter = limits.NewMemory()
	if cfg.RedisURL != "" {
		r, err := limits.NewRedis(cfg.RedisURL)
		if err != nil {
			return nil, err
		}
		counter = r
	}
	s.limits = limits.New(st, counter, cfg.Limits.FailClosed, log)
	if !cfg.Insights.Disabled {
		var n insights.Notifier
		if wh := insights.NewWebhooks(cfg.Insights.WebhookURL, cfg.Insights.SlackWebhookURL, log); wh != nil {
			n = wh
		}
		s.insights = insights.NewEngine(cfg.Insights, st, n, log)
		s.limits.OnBudget(func(l store.Limit, user string, used float64, resets time.Time) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shown := l
			shown.Amount = used
			s.insights.Budget(ctx, insights.BudgetAlert{
				Rule: l, User: user, Used: used, ResetsAt: resets,
				Amount: limits.FormatAmount(&l), UsedText: limits.FormatAmount(&shown),
			})
		})
	}
	return s, nil
}

// Prices returns the price table. Callers reload it at startup and keep it
// fresh with Run.
func (s *Server) Prices() *pricing.Table { return s.prices }

// Limits returns the rate limit engine. Callers load its rules at startup
// and keep them fresh with Run.
func (s *Server) Limits() *limits.Engine { return s.limits }

// Insights returns the insight engine, or nil when insights are disabled.
func (s *Server) Insights() *insights.Engine { return s.insights }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tokens", s.mintToken)
	mux.HandleFunc("OPTIONS /v1/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// Everything else under /<provider>/v1/ goes to that provider.
	mux.HandleFunc("/", s.handleProvider)
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
	admin.HandleFunc("PATCH /admin/keys/{id}", s.updateKey)
	admin.HandleFunc("DELETE /admin/keys/{id}", s.revokeKey)
	admin.HandleFunc("DELETE /admin/tokens/{id}", s.revokeToken)
	admin.HandleFunc("GET /admin/usage", s.usage)
	admin.HandleFunc("GET /admin/insights", s.listInsights)
	admin.HandleFunc("GET /admin/limits", s.listLimits)
	admin.HandleFunc("POST /admin/limits", s.createLimit)
	admin.HandleFunc("GET /admin/limits/status", s.limitsStatus)
	admin.HandleFunc("PATCH /admin/limits/{id}", s.updateLimit)
	admin.HandleFunc("DELETE /admin/limits/{id}", s.deleteLimit)
	admin.HandleFunc("GET /admin/prices", s.listPrices)
	admin.HandleFunc("POST /admin/prices", s.addPrice)
	admin.HandleFunc("GET /admin/audit", s.listAudit)
	mux.Handle("/admin/", s.requireAdmin(s.audit(admin)))
	mux.Handle("/admin/auth/", s.adminAuth.Handler())
	return s.cors(mux)
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
