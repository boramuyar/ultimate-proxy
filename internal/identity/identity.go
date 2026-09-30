// Package identity authenticates API keys and works out which tenant,
// application and end user a request belongs to.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

const KeyPrefix = "up_"

// NewKey returns a new API key and its hash. Only the hash is stored.
func NewKey() (key, hash string) {
	var b [24]byte
	_, _ = rand.Read(b[:])
	key = KeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return key, HashKey(key)
}

func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// DisplayPrefix is the part of a key that is safe to show in listings.
func DisplayPrefix(key string) string {
	if len(key) > 10 {
		return key[:10]
	}
	return key
}

// Authenticator resolves API keys to principals, caching lookups so the hot
// path does not touch the database. Revocations take effect within
// positiveTTL.
type Authenticator struct {
	store       store.Store
	cache       sync.Map // hash -> cacheEntry
	positiveTTL time.Duration
	negativeTTL time.Duration
}

type cacheEntry struct {
	p       *store.Principal
	expires time.Time
}

func NewAuthenticator(s store.Store) *Authenticator {
	return &Authenticator{store: s, positiveTTL: 30 * time.Second, negativeTTL: 5 * time.Second}
}

var errUnauthorized = openresponses.NewError(http.StatusUnauthorized, openresponses.ErrInvalidRequest, "invalid_api_key", "Missing or invalid API key.", "")

// Authenticate reads the bearer token from the request.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (*store.Principal, *openresponses.APIError) {
	key := BearerToken(r)
	if key == "" {
		return nil, errUnauthorized
	}
	hash := HashKey(key)
	now := time.Now()
	if v, ok := a.cache.Load(hash); ok {
		e := v.(cacheEntry)
		if now.Before(e.expires) {
			if e.p == nil {
				return nil, errUnauthorized
			}
			return e.p, nil
		}
	}
	p, err := a.store.LookupKey(ctx, hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.cache.Store(hash, cacheEntry{nil, now.Add(a.negativeTTL)})
		return nil, errUnauthorized
	case err != nil:
		return nil, openresponses.ServerError("auth_unavailable", "Could not verify the API key.")
	}
	a.cache.Store(hash, cacheEntry{p, now.Add(a.positiveTTL)})
	return p, nil
}

// Forget drops a cached key so a revocation applies immediately on this node.
func (a *Authenticator) Forget(hash string) { a.cache.Delete(hash) }

// ForgetAll clears the cache.
func (a *Authenticator) ForgetAll() { a.cache.Clear() }

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
// came from. Only applications trusted to assert users may name one; the
// order is the X-Proxy-User-Email header, metadata.user_email, then
// safety_identifier.
func ResolveUser(p *store.Principal, r *http.Request, req *openresponses.Envelope) (email, source string) {
	if !p.CanAssertUsers {
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
