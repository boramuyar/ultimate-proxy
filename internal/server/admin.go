package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/identity"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// requireAdmin accepts a dashboard session or the break-glass bearer token.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminAuth.Authenticate(r) == nil {
			openresponses.WriteError(w, openresponses.NewError(http.StatusUnauthorized, openresponses.ErrInvalidRequest, "invalid_admin_token", "Missing or invalid admin credentials.", ""))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) adminError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		openresponses.WriteError(w, openresponses.NewError(http.StatusNotFound, openresponses.ErrNotFound, "not_found", "Not found.", ""))
		return
	}
	s.log.Error("admin request failed", "err", err)
	openresponses.WriteError(w, openresponses.ServerError("internal", "Internal error."))
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_json", err.Error(), ""))
		return false
	}
	return true
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.ListTenants(r.Context())
	if err != nil {
		s.adminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": ts})
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		openresponses.WriteError(w, openresponses.InvalidRequest("missing_required_parameter", "name is required.", "name"))
		return
	}
	t, err := s.store.EnsureTenant(r.Context(), body.Name)
	if err != nil {
		s.adminError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listApplications(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApplications(r.Context(), r.PathValue("id"))
	if err != nil {
		s.adminError(w, err)
		return
	}
	if apps == nil {
		apps = []store.Application{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": apps})
}

func (s *Server) createApplication(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string `json:"name"`
		CanAssertUsers *bool  `json:"can_assert_users"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		openresponses.WriteError(w, openresponses.InvalidRequest("missing_required_parameter", "name is required.", "name"))
		return
	}
	canAssert := body.CanAssertUsers == nil || *body.CanAssertUsers
	app, err := s.store.EnsureApplication(r.Context(), r.PathValue("id"), body.Name, canAssert)
	if err != nil {
		s.adminError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, app)
}

// createKey returns the plaintext key once; only its hash is stored.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	key, hash := identity.NewKey()
	k, err := s.store.CreateKey(r.Context(), r.PathValue("id"), hash, identity.DisplayPrefix(key))
	if err != nil {
		s.adminError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		*store.APIKey
		Key string `json:"key"`
	}{k, key})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys(r.Context(), r.PathValue("id"))
	if err != nil {
		s.adminError(w, err)
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": keys})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeKey(r.Context(), r.PathValue("id")); err != nil {
		s.adminError(w, err)
		return
	}
	s.apiKeys.ForgetAll()
	w.WriteHeader(http.StatusNoContent)
}

// usage serves GET /admin/usage.
//
//	group_by     comma-separated: tenant, application, email, model, provider (default tenant)
//	from, to     RFC 3339 (default: the last 24 hours)
//	granularity  hour or day (default: one row per group)
//	tenant_id, application_id, email, model, provider  filters
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := store.UsageQuery{
		To:          time.Now().UTC(),
		Granularity: qs.Get("granularity"),
		Filters:     map[string]string{},
	}
	q.From = q.To.Add(-24 * time.Hour)
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		if v := qs.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				openresponses.WriteError(w, openresponses.InvalidRequest("invalid_parameter", p.name+" must be RFC 3339", p.name))
				return
			}
			*p.dst = t
		}
	}
	groupBy := qs.Get("group_by")
	if groupBy == "" {
		groupBy = "tenant"
	}
	if groupBy != "none" {
		for _, g := range strings.Split(groupBy, ",") {
			q.GroupBy = append(q.GroupBy, strings.TrimSpace(g))
		}
	}
	for param, dim := range map[string]string{"tenant_id": "tenant", "application_id": "application", "email": "email", "model": "model", "provider": "provider", "cache_status": "cache"} {
		if v := qs.Get(param); v != "" {
			q.Filters[dim] = v
		}
	}
	if err := q.Validate(); err != nil {
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_parameter", err.Error(), ""))
		return
	}
	rows, err := s.store.QueryUsage(r.Context(), q)
	if err != nil {
		s.adminError(w, err)
		return
	}
	if err := s.addNames(r, rows); err != nil {
		s.adminError(w, err)
		return
	}
	if rows == nil {
		rows = []store.UsageRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from":     q.From,
		"to":       q.To,
		"group_by": q.GroupBy,
		"data":     rows,
	})
}

// addNames adds tenant_name and application_name next to their ids.
func (s *Server) addNames(r *http.Request, rows []store.UsageRow) error {
	if len(rows) == 0 {
		return nil
	}
	_, hasTenant := rows[0].Group["tenant"]
	_, hasApp := rows[0].Group["application"]
	if hasTenant {
		ts, err := s.store.ListTenants(r.Context())
		if err != nil {
			return err
		}
		names := map[string]string{}
		for _, t := range ts {
			names[t.ID] = t.Name
		}
		for _, row := range rows {
			row.Group["tenant_name"] = names[row.Group["tenant"]]
		}
	}
	if hasApp {
		apps, err := s.store.ListApplications(r.Context(), "")
		if err != nil {
			return err
		}
		names := map[string]string{}
		for _, a := range apps {
			names[a.ID] = a.Name
		}
		for _, row := range rows {
			row.Group["application_name"] = names[row.Group["application"]]
		}
	}
	return nil
}

// listInsights serves GET /admin/insights?status=open|resolved|all (default open).
func (s *Server) listInsights(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "":
		status = "open"
	case "all":
		status = ""
	case "open", "resolved":
	default:
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_parameter", "status must be open, resolved or all", "status"))
		return
	}
	list, err := s.store.ListInsights(r.Context(), status)
	if err != nil {
		s.adminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

// listPrices serves GET /admin/prices: every price version, oldest first.
// ?current=true keeps only the price in effect now for each model.
func (s *Server) listPrices(w http.ResponseWriter, r *http.Request) {
	prices, err := s.store.ListPrices(r.Context())
	if err != nil {
		s.adminError(w, err)
		return
	}
	if r.URL.Query().Get("current") == "true" {
		now := time.Now()
		latest := map[string]int{}
		var models []string
		for i, p := range prices {
			if p.EffectiveFrom.After(now) {
				continue
			}
			if _, ok := latest[p.Model]; !ok {
				models = append(models, p.Model)
			}
			latest[p.Model] = i
		}
		current := make([]store.Price, 0, len(models))
		for _, m := range models {
			current = append(current, prices[latest[m]])
		}
		prices = current
	}
	if prices == nil {
		prices = []store.Price{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": prices})
}

// addPrice serves POST /admin/prices. A price is never edited: posting a new
// one for the same model supersedes the old one from effective_from on
// (default now), and past requests keep the cost they were charged.
func (s *Server) addPrice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model         string     `json:"model"`
		Input         *float64   `json:"input"`
		CachedInput   *float64   `json:"cached_input"`
		CacheWrite    *float64   `json:"cache_write"`
		Output        *float64   `json:"output"`
		EffectiveFrom *time.Time `json:"effective_from"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Model) == "" || body.Input == nil || body.Output == nil {
		openresponses.WriteError(w, openresponses.InvalidRequest("missing_required_parameter", "model, input and output are required (USD per million tokens).", ""))
		return
	}
	p := &store.Price{
		ID: openresponses.NewID("price"), Model: body.Model, Input: *body.Input, Output: *body.Output,
		CachedInput: *body.Input, CacheWrite: *body.Input, EffectiveFrom: time.Now().UTC(),
	}
	if body.CachedInput != nil {
		p.CachedInput = *body.CachedInput
	}
	if body.CacheWrite != nil {
		p.CacheWrite = *body.CacheWrite
	}
	if body.EffectiveFrom != nil {
		p.EffectiveFrom = body.EffectiveFrom.UTC()
	}
	if p.Input < 0 || p.CachedInput < 0 || p.CacheWrite < 0 || p.Output < 0 {
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_parameter", "prices must not be negative.", ""))
		return
	}
	if err := s.store.AddPrice(r.Context(), p); err != nil {
		s.adminError(w, err)
		return
	}
	if err := s.prices.Reload(r.Context()); err != nil {
		s.log.Error("reloading prices failed", "err", err)
	}
	writeJSON(w, http.StatusCreated, p)
}
