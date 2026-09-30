package server

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/boramuyar/ultimate-proxy/internal/limits"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// limitSubject is who a request counts against.
func limitSubject(tenantID, appID, email string) limits.Subject {
	return limits.Subject{TenantID: tenantID, AppID: appID, User: email}
}

// listLimits serves GET /admin/limits, optionally ?tenant_id= and
// ?application_id=.
func (s *Server) listLimits(w http.ResponseWriter, r *http.Request) {
	ls, err := s.store.ListLimits(r.Context())
	if err != nil {
		s.adminError(w, err)
		return
	}
	tenant, app := r.URL.Query().Get("tenant_id"), r.URL.Query().Get("application_id")
	ls = slices.DeleteFunc(ls, func(l store.Limit) bool {
		return (tenant != "" && l.TenantID != tenant) || (app != "" && l.AppID != app)
	})
	if ls == nil {
		ls = []store.Limit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": ls})
}

var limitKinds = []string{store.LimitRPM, store.LimitTPM}

// createLimit serves POST /admin/limits.
func (s *Server) createLimit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TenantID    string   `json:"tenant_id"`
		AppID       string   `json:"application_id"`
		User        string   `json:"user"`
		Kind        string   `json:"kind"`
		Amount      *float64 `json:"amount"`
		Enforcement string   `json:"enforcement"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	l := &store.Limit{
		TenantID: body.TenantID, AppID: body.AppID, User: strings.ToLower(strings.TrimSpace(body.User)),
		Kind: body.Kind, Enforcement: body.Enforcement,
	}
	if body.Amount != nil {
		l.Amount = *body.Amount
	}
	if l.Enforcement == "" {
		l.Enforcement = "hard"
	}
	if e := s.validateLimit(r.Context(), l, body.Amount == nil); e != nil {
		openresponses.WriteError(w, e)
		return
	}
	if err := s.store.CreateLimit(r.Context(), l); err != nil {
		s.adminError(w, err)
		return
	}
	s.reloadLimits(r.Context())
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) validateLimit(ctx context.Context, l *store.Limit, missingAmount bool) *openresponses.APIError {
	switch {
	case l.TenantID == "" || l.Kind == "" || missingAmount:
		return openresponses.InvalidRequest("missing_required_parameter", "tenant_id, kind and amount are required.", "")
	case !slices.Contains(limitKinds, l.Kind):
		return openresponses.InvalidRequest("invalid_parameter", "kind must be one of "+strings.Join(limitKinds, ", ")+".", "kind")
	case l.Amount <= 0:
		return openresponses.InvalidRequest("invalid_parameter", "amount must be positive.", "amount")
	case l.Enforcement != "hard":
		return openresponses.InvalidRequest("invalid_parameter", "rate limits are always enforced; enforcement must be hard.", "enforcement")
	}
	apps, err := s.store.ListApplications(ctx, l.TenantID)
	if err != nil {
		return openresponses.ServerError("internal", "Internal error.")
	}
	if len(apps) == 0 {
		// A tenant without applications may still exist; check it does.
		ts, err := s.store.ListTenants(ctx)
		if err != nil {
			return openresponses.ServerError("internal", "Internal error.")
		}
		if !slices.ContainsFunc(ts, func(t store.Tenant) bool { return t.ID == l.TenantID }) {
			return openresponses.InvalidRequest("invalid_parameter", "Unknown tenant.", "tenant_id")
		}
	}
	if l.AppID != "" && !slices.ContainsFunc(apps, func(a store.Application) bool { return a.ID == l.AppID }) {
		return openresponses.InvalidRequest("invalid_parameter", "The application is not in this tenant.", "application_id")
	}
	return nil
}

// updateLimit serves PATCH /admin/limits/{id}; only amount can change.
// Changing what a rule counts means a new rule.
func (s *Server) updateLimit(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.GetLimit(r.Context(), r.PathValue("id"))
	if err != nil {
		s.adminError(w, err)
		return
	}
	var body struct {
		Amount *float64 `json:"amount"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Amount != nil {
		l.Amount = *body.Amount
	}
	if e := s.validateLimit(r.Context(), l, false); e != nil {
		openresponses.WriteError(w, e)
		return
	}
	if err := s.store.UpdateLimit(r.Context(), l); err != nil {
		s.adminError(w, err)
		return
	}
	s.reloadLimits(r.Context())
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) deleteLimit(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteLimit(r.Context(), r.PathValue("id")); err != nil {
		s.adminError(w, err)
		return
	}
	s.reloadLimits(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// reloadLimits applies an admin change on this replica at once; the others
// pick it up on their next reload.
func (s *Server) reloadLimits(ctx context.Context) {
	if err := s.limits.Reload(ctx); err != nil {
		s.log.Error("reloading limits failed", "err", err)
	}
}

// limitsStatus serves GET /admin/limits/status: each rule's current use.
// Per-user rules report ?user='s use, or null without one.
func (s *Server) limitsStatus(w http.ResponseWriter, r *http.Request) {
	user := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("user")))
	ls, err := s.store.ListLimits(r.Context())
	if err != nil {
		s.adminError(w, err)
		return
	}
	type status struct {
		store.Limit
		Used *float64 `json:"used"`
	}
	out := []status{}
	for _, l := range ls {
		st := status{Limit: l}
		if l.User != "*" || user != "" {
			if used, err := s.limits.Used(r.Context(), &l, user); err == nil {
				st.Used = &used
			}
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
