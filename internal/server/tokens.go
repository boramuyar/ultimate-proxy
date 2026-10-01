package server

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/identity"
	"github.com/omni-proxy/omni-proxy/internal/openresponses"
	"github.com/omni-proxy/omni-proxy/internal/store"
)

const (
	defaultTokenTTL = 15 * time.Minute
	minTokenTTL     = time.Minute
	maxTokenTTL     = 24 * time.Hour
)

// mintToken serves POST /v1/tokens: a backend holding an API key, or an
// agent holding the user's own access token, gets a short-lived token to
// hand to code running on the user's machine or in a browser. The token can
// only narrow what its minter may do: the same tenant and application, the
// minter's user or one it may assert, a subset of its models, and no later
// expiry than the minter's own credential.
func (s *Server) mintToken(w http.ResponseWriter, r *http.Request) {
	id, apiErr := s.auth.Authenticate(r.Context(), r)
	if apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return
	}
	if id.Method == identity.MethodProxyToken {
		openresponses.WriteError(w, forbidden("token_cannot_mint", "A minted token cannot mint further tokens.", ""))
		return
	}
	var body struct {
		User      string    `json:"user"`
		Models    *[]string `json:"models"`
		ExpiresIn *int      `json:"expires_in"` // seconds
	}
	if !decodeBody(w, r, &body) {
		return
	}

	user := strings.ToLower(strings.TrimSpace(body.User))
	switch {
	case id.User != "":
		if user != "" && user != strings.ToLower(id.User) {
			openresponses.WriteError(w, forbidden("user_not_allowed", "A token can only be minted for the user of the credential minting it.", "user"))
			return
		}
		user = strings.ToLower(id.User)
	case user != "" && !id.CanAssertUsers:
		openresponses.WriteError(w, forbidden("user_not_allowed", "This application may not name its users.", "user"))
		return
	}

	models := id.AllowedModels
	if body.Models != nil {
		models = []string{}
		for _, m := range *body.Models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			if !covers(id.AllowedModels, m) {
				openresponses.WriteError(w, forbidden("model_not_allowed", "The minting credential may not use '"+m+"'.", "models"))
				return
			}
			models = append(models, m)
		}
	}

	ttl := defaultTokenTTL
	if body.ExpiresIn != nil {
		ttl = time.Duration(*body.ExpiresIn) * time.Second
		if ttl < minTokenTTL || ttl > maxTokenTTL {
			openresponses.WriteError(w, openresponses.InvalidRequest("invalid_value", "expires_in must be between 60 and 86400 seconds.", "expires_in"))
			return
		}
	}
	expires := time.Now().Add(ttl).UTC().Truncate(time.Second)
	if !id.Expires.IsZero() && id.Expires.Before(expires) {
		expires = id.Expires.UTC()
	}

	token, hash := identity.NewToken()
	t := &store.ProxyToken{
		AppID: id.AppID, UserEmail: user, AllowedModels: models,
		MintedBy: id.Method + ":" + id.Subject, ExpiresAt: expires,
	}
	if err := s.store.CreateProxyToken(r.Context(), t, hash); err != nil {
		s.log.Error("minting a token failed", "err", err)
		openresponses.WriteError(w, openresponses.ServerError("internal", "Could not mint the token."))
		return
	}
	resp := map[string]any{"id": t.ID, "token": token, "expires_at": t.ExpiresAt, "user": t.UserEmail, "models": t.AllowedModels}
	writeJSON(w, http.StatusCreated, resp)
}

// covers reports whether a minter's allowed patterns may grant model, which
// may itself end in *. aliases are other names the model goes by.
func covers(patterns []string, model string, aliases ...string) bool {
	if patterns == nil {
		return true
	}
	if slices.Contains(patterns, model) {
		return true
	}
	want, wantAll := strings.CutSuffix(model, "*")
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok && strings.HasPrefix(want, prefix) {
			return true
		}
	}
	return !wantAll && (&identity.Identity{AllowedModels: patterns}).AllowsModel(append([]string{model}, aliases...)...)
}

func forbidden(code, message, param string) *openresponses.APIError {
	return openresponses.NewError(http.StatusForbidden, openresponses.ErrInvalidRequest, code, message, param)
}

// revokeToken serves DELETE /admin/tokens/{id}.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeProxyToken(r.Context(), r.PathValue("id")); err != nil {
		s.adminError(w, err)
		return
	}
	s.tokens.ForgetAll()
	w.WriteHeader(http.StatusNoContent)
}

// cors lets pages on the configured origins call /v1 from a browser. Callers
// authenticate with bearer tokens, never cookies, so credentials stay off.
func (s *Server) cors(next http.Handler) http.Handler {
	origins := s.cfg.Auth.CORSOrigins
	if len(origins) == 0 {
		return next
	}
	anyOrigin := slices.Contains(origins, "*")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && apiPath(r.URL.Path) && (anyOrigin || slices.Contains(origins, origin)) {
			h := w.Header()
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", "X-Proxy-Request-Id")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+identity.UserHeader+", "+TagsHeader)
				h.Set("Access-Control-Max-Age", "600")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// apiPath reports whether a path is part of the client API: /v1/tokens or
// /<provider>/v1/...
func apiPath(path string) bool {
	first, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return first == "v1" || (first != "admin" && strings.HasPrefix(rest, "v1/"))
}
