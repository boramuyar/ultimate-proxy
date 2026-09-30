// Package identity authenticates callers and works out which tenant,
// application and end user a request belongs to.
package identity

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
)

// How a caller authenticated.
const (
	MethodAPIKey     = "api_key"
	MethodJWT        = "jwt"
	MethodProxyToken = "proxy_token"
)

// Identity is who a request is billed to, whatever credential it came with.
// Tenant, application and user are plain values: for API keys they come from
// the key's rows in the store.
type Identity struct {
	Method string
	// Subject identifies the credential: the key ID for API keys, the sub
	// claim for JWTs.
	Subject    string
	TenantID   string
	TenantName string
	AppID      string
	AppName    string
	// User is the end user the credential itself names, if any. API keys name
	// none; the application may assert one per request instead.
	User   string
	Groups []string
	// AllowedModels limits the models the caller may use; nil allows all.
	// Entries are model names or provider/model, and may end in *.
	AllowedModels []string
	// Expires is when the credential stops working; zero for API keys.
	Expires time.Time
	// CanAssertUsers lets the caller name its end user in a header or in
	// metadata.
	CanAssertUsers bool
}

// KeyID is the API key a request used, or "" for other credentials.
func (id *Identity) KeyID() string {
	if id.Method == MethodAPIKey {
		return id.Subject
	}
	return ""
}

// AllowsModel reports whether the caller may use a model, given the names it
// goes by: the name the client sent and the provider/upstream model it maps to.
func (id *Identity) AllowsModel(names ...string) bool {
	if id.AllowedModels == nil {
		return true
	}
	for _, pattern := range id.AllowedModels {
		prefix, wildcard := strings.CutSuffix(pattern, "*")
		for _, n := range names {
			if n == pattern || (wildcard && strings.HasPrefix(n, prefix)) {
				return true
			}
		}
	}
	return false
}

// Authenticator turns a bearer token into an Identity.
type Authenticator interface {
	// Accepts reports whether the token is one this authenticator handles.
	// The first authenticator in a chain that accepts a token decides it.
	Accepts(token string) bool
	Authenticate(ctx context.Context, token string) (*Identity, *openresponses.APIError)
}

// Chain tries authenticators in order.
type Chain []Authenticator

var errUnauthorized = openresponses.NewError(http.StatusUnauthorized, openresponses.ErrInvalidRequest, "invalid_api_key", "Missing or invalid API key.", "")

// Authenticate reads the bearer token from the request.
func (c Chain) Authenticate(ctx context.Context, r *http.Request) (*Identity, *openresponses.APIError) {
	token := BearerToken(r)
	if token == "" {
		return nil, errUnauthorized
	}
	for _, a := range c {
		if a.Accepts(token) {
			return a.Authenticate(ctx, token)
		}
	}
	return nil, errUnauthorized
}

func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// Header an application can use to name its end user.
const UserHeader = "X-Proxy-User-Email"

// ResolveUser returns the end user a request is attributed to and where that
// came from. A user named by the credential wins. Otherwise only callers
// trusted to assert users may name one; the order is the X-Proxy-User-Email
// header, metadata.user_email, then safety_identifier.
func ResolveUser(id *Identity, r *http.Request, req *openresponses.Envelope) (email, source string) {
	if id.User != "" {
		return normalize(id.User), id.Method
	}
	if !id.CanAssertUsers {
		return "", "none"
	}
	if v := strings.TrimSpace(r.Header.Get(UserHeader)); v != "" {
		return normalize(v), "header"
	}
	if v := strings.TrimSpace(req.Metadata["user_email"]); v != "" {
		return normalize(v), "metadata"
	}
	if req.SafetyIdentifier != nil && strings.TrimSpace(*req.SafetyIdentifier) != "" {
		return normalize(*req.SafetyIdentifier), "safety_identifier"
	}
	return "", "none"
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 320 {
		s = s[:320]
	}
	return s
}
