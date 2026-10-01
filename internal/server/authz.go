package server

import (
	"context"
	"net/http"

	"github.com/omni-proxy/omni-proxy/internal/openresponses"
	"github.com/omni-proxy/omni-proxy/internal/store"
)

// Admin API access. Every route names what it needs:
//
//	needAdmin    a full admin: tenants, prices and tokens belong to no tenant
//	needReadAll  read access to every tenant: the audit log
//	needSignedIn anyone signed in; the handler shows only readable tenants
//	needTenant   read (GET) or change the tenant a resolver finds for the
//	             request's {id}
type access struct {
	kind   int
	tenant func(r *http.Request) (string, error) // tenant id, for needTenant
}

const (
	needSignedIn = iota
	needAdmin
	needReadAll
	needTenant
)

var (
	signedIn  = access{kind: needSignedIn}
	onlyAdmin = access{kind: needAdmin}
	readAll   = access{kind: needReadAll}
)

func tenantOf(f func(r *http.Request) (string, error)) access {
	return access{kind: needTenant, tenant: f}
}

// allow wraps an admin route with its access rule.
func (s *Server) allow(a access, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := adminPrincipal(r.Context())
		ok := p != nil
		switch a.kind {
		case needAdmin:
			ok = ok && p.IsAdmin()
		case needReadAll:
			ok = ok && p.ReadsAll()
		case needTenant:
			if !ok {
				break
			}
			id, err := a.tenant(r)
			if err != nil {
				s.adminError(w, err)
				return
			}
			if ok, err = s.mayTenant(r, id, r.Method != http.MethodGet); err != nil {
				s.adminError(w, err)
				return
			}
		}
		if !ok {
			writeForbidden(w)
			return
		}
		h(w, r)
	}
}

func writeForbidden(w http.ResponseWriter) {
	openresponses.WriteError(w, openresponses.NewError(http.StatusForbidden, openresponses.ErrInvalidRequest, "forbidden",
		"Your role does not allow this.", ""))
}

// mayTenant reports whether the caller may read, or change, a tenant.
// Roles name tenants by name; an unknown id is allowed only to those who
// may act on every tenant, so they get the store's not found.
func (s *Server) mayTenant(r *http.Request, tenantID string, write bool) (bool, error) {
	p := adminPrincipal(r.Context())
	if p.IsAdmin() || !write && p.ReadsAll() {
		return true, nil
	}
	name, err := s.tenantName(r.Context(), tenantID)
	if err != nil {
		return false, err
	}
	if write {
		return p.CanWrite(name), nil
	}
	return p.CanRead(name), nil
}

func (s *Server) tenantName(ctx context.Context, id string) (string, error) {
	ts, err := s.store.ListTenants(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range ts {
		if t.ID == id {
			return t.Name, nil
		}
	}
	return "", nil
}

// readableTenants is nil when the caller may read every tenant, and
// otherwise the ids of the tenants it may read (possibly none).
func (s *Server) readableTenants(r *http.Request) (map[string]bool, error) {
	p := adminPrincipal(r.Context())
	if p.ReadsAll() {
		return nil, nil
	}
	ts, err := s.store.ListTenants(r.Context())
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, t := range ts {
		if p.CanRead(t.Name) {
			out[t.ID] = true
		}
	}
	return out, nil
}

// Tenant resolvers for needTenant routes.

func pathTenant(r *http.Request) (string, error) { return r.PathValue("id"), nil }

func (s *Server) appTenant(ctx context.Context, appID string) (string, error) {
	apps, err := s.store.ListApplications(ctx, "")
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		if a.ID == appID {
			return a.TenantID, nil
		}
	}
	return "", store.ErrNotFound
}

func (s *Server) pathAppTenant(r *http.Request) (string, error) {
	return s.appTenant(r.Context(), r.PathValue("id"))
}

func (s *Server) pathKeyTenant(r *http.Request) (string, error) {
	k, err := s.store.GetKey(r.Context(), r.PathValue("id"))
	if err != nil {
		return "", err
	}
	return s.appTenant(r.Context(), k.AppID)
}

func (s *Server) pathLimitTenant(r *http.Request) (string, error) {
	l, err := s.store.GetLimit(r.Context(), r.PathValue("id"))
	if err != nil {
		return "", err
	}
	return l.TenantID, nil
}
