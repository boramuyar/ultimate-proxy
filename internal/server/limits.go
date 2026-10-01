package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/limits"
	"github.com/omni-proxy/omni-proxy/internal/openresponses"
	"github.com/omni-proxy/omni-proxy/internal/store"
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
	readable, err := s.readableTenants(r)
	if err != nil {
		s.adminError(w, err)
		return
	}
	tenant, app := r.URL.Query().Get("tenant_id"), r.URL.Query().Get("application_id")
	ls = slices.DeleteFunc(ls, func(l store.Limit) bool {
		return (tenant != "" && l.TenantID != tenant) || (app != "" && l.AppID != app) || (readable != nil && !readable[l.TenantID])
	})
	if ls == nil {
		ls = []store.Limit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": ls})
}

var (
	limitKinds   = []string{store.LimitRPM, store.LimitTPM, store.LimitBudgetUSD, store.LimitBudgetTokens}
	limitPeriods = []string{"day", "week", "month"}
)

// createLimit serves POST /admin/limits.
func (s *Server) createLimit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TenantID    string   `json:"tenant_id"`
		AppID       string   `json:"application_id"`
		User        string   `json:"user"`
		Kind        string   `json:"kind"`
		Amount      *float64 `json:"amount"`
		Period      string   `json:"period"`
		Enforcement string   `json:"enforcement"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if ok, err := s.mayTenant(r, body.TenantID, true); err != nil || !ok {
		if err != nil {
			s.adminError(w, err)
		} else {
			writeForbidden(w)
		}
		return
	}
	l := &store.Limit{
		TenantID: body.TenantID, AppID: body.AppID, User: strings.ToLower(strings.TrimSpace(body.User)),
		Kind: body.Kind, Period: body.Period, Enforcement: body.Enforcement,
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
	// A new budget starts from what the period has used so far.
	if err := s.limits.RebuildRule(r.Context(), l); err != nil {
		s.log.Warn("counting usage so far for a new budget failed", "err", err)
	}
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
	case l.IsBudget() && !slices.Contains(limitPeriods, l.Period):
		return openresponses.InvalidRequest("invalid_parameter", "budgets need a period: day, week or month (UTC).", "period")
	case !l.IsBudget() && l.Period != "":
		return openresponses.InvalidRequest("invalid_parameter", "rpm and tpm count over a sliding minute and take no period.", "period")
	case l.Enforcement != "hard" && (l.Enforcement != "soft" || !l.IsBudget()):
		return openresponses.InvalidRequest("invalid_parameter", "enforcement is hard, or soft (warn only) for budgets.", "enforcement")
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

// updateLimit serves PATCH /admin/limits/{id}; amount and, for budgets,
// enforcement can change. Changing what a rule counts means a new rule.
func (s *Server) updateLimit(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.GetLimit(r.Context(), r.PathValue("id"))
	if err != nil {
		s.adminError(w, err)
		return
	}
	var body struct {
		Amount      *float64 `json:"amount"`
		Enforcement *string  `json:"enforcement"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Amount != nil {
		l.Amount = *body.Amount
	}
	if body.Enforcement != nil {
		l.Enforcement = *body.Enforcement
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
	readable, err := s.readableTenants(r)
	if err != nil {
		s.adminError(w, err)
		return
	}
	if readable != nil {
		ls = slices.DeleteFunc(ls, func(l store.Limit) bool { return !readable[l.TenantID] })
	}
	type status struct {
		store.Limit
		Used *float64 `json:"used"`
		// ResetsAt is when a budget's period ends.
		ResetsAt *time.Time `json:"resets_at,omitempty"`
	}
	out := []status{}
	for _, l := range ls {
		st := status{Limit: l}
		if l.IsBudget() {
			end := limits.PeriodEnd(l.Period, time.Now())
			st.ResetsAt = &end
		}
		if l.User != "*" || user != "" {
			if used, err := s.limits.Used(r.Context(), &l, user); err == nil {
				st.Used = &used
			}
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
