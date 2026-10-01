package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/omni-proxy/omni-proxy/internal/config"
	"github.com/omni-proxy/omni-proxy/internal/openresponses"
	"github.com/omni-proxy/omni-proxy/internal/store"
)

// Asymmetric algorithms only: with a shared-secret algorithm, anyone who can
// read the key set could sign tokens.
var jwtAlgs = []string{
	oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA,
}

const (
	// maxJWTCache bounds the verified-token cache; it is cleared when full.
	maxJWTCache = 100_000
	// jwtCacheTTL is the longest a verified token is trusted without being
	// checked again, even if it expires later.
	jwtCacheTTL = 5 * time.Minute
	// discoveryRetry is how long to wait after a failed discovery.
	discoveryRetry = 10 * time.Second
)

var (
	errInvalidToken = openresponses.NewError(http.StatusUnauthorized, openresponses.ErrInvalidRequest, "invalid_token", "The access token is invalid.", "")
	errTokenExpired = openresponses.NewError(http.StatusUnauthorized, openresponses.ErrInvalidRequest, "token_expired", "The access token has expired.", "")
)

// JWTs authenticates access tokens from the identity providers in the config.
// Tenant, application and user come from the token's claims. Tenants and
// applications are created in the store the first time a token names them,
// so they need no setup and show up in the dashboard by name.
type JWTs struct {
	issuers map[string]*issuer
	store   store.Store

	cache     sync.Map // token hash -> jwtEntry
	cacheSize atomic.Int64
	tenants   sync.Map // tenant name -> tenant ID
	apps      sync.Map // tenant ID + "\x00" + app name -> app ID
}

type jwtEntry struct {
	id      *Identity
	expires time.Time
}

type issuer struct {
	cfg config.JWTIssuer

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	retryAt  time.Time
}

func NewJWTs(issuers []config.JWTIssuer, st store.Store) *JWTs {
	j := &JWTs{issuers: map[string]*issuer{}, store: st}
	for _, c := range issuers {
		j.issuers[c.Issuer] = &issuer{cfg: c}
	}
	return j
}

// Accepts takes tokens shaped like a JWS: three dot-separated parts, the
// first a JSON header naming an algorithm.
func (j *JWTs) Accepts(token string) bool {
	head, _, ok := strings.Cut(token, ".")
	if !ok || strings.Count(token, ".") != 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return false
	}
	var h struct {
		Alg string `json:"alg"`
	}
	return json.Unmarshal(raw, &h) == nil && h.Alg != ""
}

func (j *JWTs) Authenticate(ctx context.Context, token string) (*Identity, *openresponses.APIError) {
	hash := HashKey(token)
	now := time.Now()
	if v, ok := j.cache.Load(hash); ok {
		e := v.(jwtEntry)
		if now.Before(e.expires) {
			return e.id, nil
		}
		j.cache.Delete(hash)
		j.cacheSize.Add(-1)
	}

	iss, ok := j.issuers[unverifiedIssuer(token)]
	if !ok {
		return nil, errInvalidToken
	}
	v, err := iss.getVerifier(ctx)
	if err != nil {
		return nil, openresponses.ServerError("auth_unavailable", "Could not reach the token issuer to verify the access token.")
	}
	tok, err := v.Verify(ctx, token)
	if err != nil {
		var expired *oidc.TokenExpiredError
		if errors.As(err, &expired) {
			return nil, errTokenExpired
		}
		return nil, errInvalidToken
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, errInvalidToken
	}
	id, apiErr := j.identity(ctx, &iss.cfg, tok.Subject, claims)
	if apiErr != nil {
		return nil, apiErr
	}
	id.Expires = tok.Expiry

	expires := tok.Expiry
	if limit := now.Add(jwtCacheTTL); expires.IsZero() || expires.After(limit) {
		expires = limit
	}
	if j.cacheSize.Add(1) > maxJWTCache {
		j.cache.Clear()
		j.cacheSize.Store(1)
	}
	j.cache.Store(hash, jwtEntry{id, expires})
	return id, nil
}

func (j *JWTs) identity(ctx context.Context, c *config.JWTIssuer, subject string, claims map[string]any) (*Identity, *openresponses.APIError) {
	tenant := c.Tenant
	if c.Claims.Tenant != "" {
		if v := claimString(claims, c.Claims.Tenant); v != "" {
			tenant = v
		}
	}
	if tenant == "" {
		return nil, openresponses.NewError(http.StatusUnauthorized, openresponses.ErrInvalidRequest, "invalid_token",
			"The access token has no "+c.Claims.Tenant+" claim to name its tenant.", "")
	}
	app := claimString(claims, c.Claims.App)
	if c.Claims.App == "" {
		if app = claimString(claims, "azp"); app == "" {
			app = claimString(claims, "client_id")
		}
	}
	if app == "" {
		app = "default"
	}
	id := &Identity{
		Method:     MethodJWT,
		Subject:    subject,
		TenantName: tenant,
		AppName:    app,
		User:       claimString(claims, c.Claims.User),
		Groups:     claimStrings(claims, c.Claims.Groups),
	}

	if c.Claims.Models != "" {
		if _, ok := claims[c.Claims.Models]; ok {
			id.AllowedModels = claimStrings(claims, c.Claims.Models)
		}
	}
	if id.AllowedModels == nil && c.GroupModels != nil {
		id.AllowedModels = []string{}
		for _, g := range id.Groups {
			id.AllowedModels = append(id.AllowedModels, c.GroupModels[g]...)
		}
	}

	var err error
	if id.TenantID, err = j.tenantID(ctx, tenant); err != nil {
		return nil, openresponses.ServerError("auth_unavailable", "Could not record the caller's tenant.")
	}
	if id.AppID, err = j.appID(ctx, id.TenantID, app); err != nil {
		return nil, openresponses.ServerError("auth_unavailable", "Could not record the caller's application.")
	}
	return id, nil
}

func (j *JWTs) tenantID(ctx context.Context, name string) (string, error) {
	if v, ok := j.tenants.Load(name); ok {
		return v.(string), nil
	}
	t, err := j.store.EnsureTenant(ctx, name)
	if err != nil {
		return "", err
	}
	j.tenants.Store(name, t.ID)
	return t.ID, nil
}

func (j *JWTs) appID(ctx context.Context, tenantID, name string) (string, error) {
	key := tenantID + "\x00" + name
	if v, ok := j.apps.Load(key); ok {
		return v.(string), nil
	}
	// The user comes from the token, so the application never asserts one.
	a, err := j.store.EnsureApplication(ctx, tenantID, name, false)
	if err != nil {
		return "", err
	}
	j.apps.Store(key, a.ID)
	return a.ID, nil
}

// getVerifier discovers the issuer's signing keys on first use, so the proxy
// starts even while an identity provider is down.
func (i *issuer) getVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.verifier != nil {
		return i.verifier, nil
	}
	if time.Now().Before(i.retryAt) {
		return nil, errors.New("issuer discovery failed recently")
	}
	jwksURL := i.cfg.JWKSURL
	if jwksURL == "" {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		p, err := oidc.NewProvider(dctx, i.cfg.Issuer)
		if err != nil {
			i.retryAt = time.Now().Add(discoveryRetry)
			return nil, err
		}
		var meta struct {
			JWKSURL string `json:"jwks_uri"`
		}
		if err := p.Claims(&meta); err != nil || meta.JWKSURL == "" {
			i.retryAt = time.Now().Add(discoveryRetry)
			return nil, errors.New("discovery document has no jwks_uri")
		}
		jwksURL = meta.JWKSURL
	}
	cfg := &oidc.Config{ClientID: i.cfg.Audience, SupportedSigningAlgs: jwtAlgs}
	i.verifier = oidc.NewVerifier(i.cfg.Issuer, newKeySet(jwksURL), cfg)
	return i.verifier, nil
}

// unverifiedIssuer reads the iss claim without checking the signature, only
// to pick which issuer's keys to check it with.
func unverifiedIssuer(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return c.Iss
}

func claimString(claims map[string]any, name string) string {
	if name == "" {
		return ""
	}
	s, _ := claims[name].(string)
	return strings.TrimSpace(s)
}

// claimStrings reads a claim holding an array of strings or a
// space-separated string, sorted.
func claimStrings(claims map[string]any, name string) []string {
	var out []string
	switch v := claims[name].(type) {
	case string:
		out = strings.Fields(v)
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}
