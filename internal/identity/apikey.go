package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
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

// APIKeys authenticates the proxy's own API keys, caching lookups so the hot
// path does not touch the database. Revocations take effect within
// positiveTTL.
type APIKeys struct {
	store       store.Store
	cache       sync.Map // hash -> cacheEntry
	positiveTTL time.Duration
	negativeTTL time.Duration
}

type cacheEntry struct {
	id      *Identity
	err     *openresponses.APIError // why id is nil; errUnauthorized when unset
	expires time.Time
}

var errKeyExpired = openresponses.NewError(http.StatusUnauthorized, openresponses.ErrInvalidRequest, "api_key_expired", "The API key has expired.", "")

func NewAPIKeys(s store.Store) *APIKeys {
	return &APIKeys{store: s, positiveTTL: 30 * time.Second, negativeTTL: 5 * time.Second}
}

// Accepts takes any token: keys given in the config need not carry the up_
// prefix, so API keys go last in a chain.
func (a *APIKeys) Accepts(string) bool { return true }

func (a *APIKeys) Authenticate(ctx context.Context, key string) (*Identity, *openresponses.APIError) {
	hash := HashKey(key)
	now := time.Now()
	if v, ok := a.cache.Load(hash); ok {
		e := v.(cacheEntry)
		if now.Before(e.expires) {
			if e.id == nil {
				if e.err != nil {
					return nil, e.err
				}
				return nil, errUnauthorized
			}
			return e.id, nil
		}
	}
	p, err := a.store.LookupKey(ctx, hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.cache.Store(hash, cacheEntry{expires: now.Add(a.negativeTTL)})
		return nil, errUnauthorized
	case err != nil:
		return nil, openresponses.ServerError("auth_unavailable", "Could not verify the API key.")
	case p.ExpiresAt != nil && !now.Before(*p.ExpiresAt):
		a.cache.Store(hash, cacheEntry{err: errKeyExpired, expires: now.Add(a.negativeTTL)})
		return nil, errKeyExpired
	}
	id := &Identity{
		Method: MethodAPIKey, Subject: p.KeyID,
		TenantID: p.TenantID, TenantName: p.TenantName,
		AppID: p.AppID, AppName: p.AppName,
		CanAssertUsers: p.CanAssertUsers,
		AllowedModels:  p.AllowedModels,
	}
	// Never cache a key past its expiry, so it stops working on time.
	expires := now.Add(a.positiveTTL)
	if p.ExpiresAt != nil {
		id.Expires = *p.ExpiresAt
		if p.ExpiresAt.Before(expires) {
			expires = *p.ExpiresAt
		}
	}
	a.cache.Store(hash, cacheEntry{id: id, expires: expires})
	return id, nil
}

// Forget drops a cached key so a revocation applies immediately on this node.
func (a *APIKeys) Forget(hash string) { a.cache.Delete(hash) }

// ForgetAll clears the cache.
func (a *APIKeys) ForgetAll() { a.cache.Clear() }
