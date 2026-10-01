package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/adminauth"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// auditActions names the admin API's changes, by route pattern.
var auditActions = map[string]string{
	"POST /admin/tenants":                   "tenant.create",
	"POST /admin/tenants/{id}/applications": "application.create",
	"POST /admin/applications/{id}/keys":    "key.create",
	"PATCH /admin/keys/{id}":                "key.update",
	"DELETE /admin/keys/{id}":               "key.revoke",
	"DELETE /admin/tokens/{id}":             "token.revoke",
	"POST /admin/limits":                    "limit.create",
	"PATCH /admin/limits/{id}":              "limit.update",
	"DELETE /admin/limits/{id}":             "limit.delete",
	"POST /admin/prices":                    "price.add",
}

// maxAuditBody caps how much of a request body is kept. Admin bodies are a
// few hundred bytes; anything longer is noted as cut.
const maxAuditBody = 16 << 10

// audit records every admin request that may change something, after it is
// served, with who sent it and how it ended. Reads are not recorded.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(r.Body, maxAuditBody+1))
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		}
		rec := &auditWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		e := &store.AuditEntry{
			ID: openresponses.NewID("aud"), TS: time.Now().UTC(), Method: r.Method, Path: r.URL.Path,
			Action: auditActions[r.Pattern], TargetID: r.PathValue("id"), Status: rec.status,
		}
		if e.Action == "" {
			e.Action = "unknown"
		}
		if p := adminPrincipal(r.Context()); p != nil {
			e.ActorMethod, e.ActorEmail, e.ActorName, e.ActorRole = p.Method, p.Email, p.Name, p.Role()
		}
		switch {
		case len(body) > maxAuditBody:
			e.Request = json.RawMessage(`{"_truncated":true}`)
		case len(body) > 0 && json.Valid(body):
			e.Request = body
		}
		// A create names its new object in the reply. Only its id is kept,
		// never the reply itself: a new key's reply holds the key.
		if r.Method == http.MethodPost && rec.status < 300 {
			var created struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(rec.buf.Bytes(), &created) == nil && created.ID != "" {
				e.TargetID = created.ID
			}
		}
		s.recordAudit(e)
	})
}

// auditSignIn records a dashboard sign-in, or a refused one.
func (s *Server) auditSignIn(p adminauth.Principal, allowed bool) {
	e := &store.AuditEntry{
		ID: openresponses.NewID("aud"), TS: time.Now().UTC(), ActorMethod: p.Method, ActorEmail: p.Email, ActorName: p.Name,
		ActorRole: p.Role(), Action: "sign_in", Method: http.MethodPost, Path: "/admin/auth/token", Status: http.StatusOK,
	}
	if p.Method == adminauth.MethodOIDC {
		e.Method, e.Path = http.MethodGet, "/admin/auth/callback"
	}
	if !allowed {
		e.Action, e.Status, e.ActorRole = "sign_in.refused", http.StatusForbidden, ""
	}
	s.recordAudit(e)
}

func (s *Server) recordAudit(e *store.AuditEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.AddAudit(ctx, e); err != nil {
		s.log.Error("writing the audit log failed", "action", e.Action, "path", e.Path, "err", err)
	}
}

// auditWriter keeps the status and the start of the reply.
type auditWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
	wrote  bool
}

func (a *auditWriter) WriteHeader(code int) {
	if !a.wrote {
		a.status, a.wrote = code, true
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditWriter) Write(b []byte) (int, error) {
	a.wrote = true
	if a.buf.Len() < maxAuditBody {
		a.buf.Write(b)
	}
	return a.ResponseWriter.Write(b)
}

// listAudit serves GET /admin/audit: newest first, limit (default 100, at
// most 1000) entries before the given time.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			openresponses.WriteError(w, openresponses.InvalidRequest("invalid_parameter", "limit must be 1 to 1000", "limit"))
			return
		}
		limit = n
	}
	var before time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			openresponses.WriteError(w, openresponses.InvalidRequest("invalid_parameter", "before must be RFC 3339", "before"))
			return
		}
		before = t
	}
	entries, err := s.store.ListAudit(r.Context(), before, limit)
	if err != nil {
		s.adminError(w, err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": entries})
}

type principalKey struct{}

func withAdminPrincipal(ctx context.Context, p *adminauth.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// adminPrincipal is who made an admin request, set by requireAdmin.
func adminPrincipal(ctx context.Context) *adminauth.Principal {
	p, _ := ctx.Value(principalKey{}).(*adminauth.Principal)
	return p
}
