package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// TokenPrefix starts every token the proxy mints for client-side agents.
const TokenPrefix = "upt_"

// NewToken returns a new proxy token and its hash. Only the hash is stored.
func NewToken() (token, hash string) {
	var b [24]byte
	_, _ = rand.Read(b[:])
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashKey(token)
}

// ProxyTokens authenticates tokens minted with POST /v1/tokens. Like API
// keys, lookups are cached, so a revocation takes effect within positiveTTL.
type ProxyTokens struct {
	store       store.Store
	cache       sync.Map // hash -> cacheEntry
	positiveTTL time.Duration
	negativeTTL time.Duration
}

func NewProxyTokens(s store.Store) *ProxyTokens {
	return &ProxyTokens{store: s, positiveTTL: 30 * time.Second, negativeTTL: 5 * time.Second}
}

func (p *ProxyTokens) Accepts(token string) bool { return strings.HasPrefix(token, TokenPrefix) }

func (p *ProxyTokens) Authenticate(ctx context.Context, token string) (*Identity, *openresponses.APIError) {
	hash := HashKey(token)
	now := time.Now()
	if v, ok := p.cache.Load(hash); ok {
		e := v.(cacheEntry)
		if now.Before(e.expires) {
			if e.id == nil {
				return nil, errInvalidToken
			}
			return e.id, nil
		}
	}
	t, err := p.store.LookupProxyToken(ctx, hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		p.cache.Store(hash, cacheEntry{expires: now.Add(p.negativeTTL)})
		return nil, errInvalidToken
	case err != nil:
		return nil, openresponses.ServerError("auth_unavailable", "Could not verify the token.")
	}
	id := &Identity{
		Method: MethodProxyToken, Subject: t.ID,
		TenantID: t.TenantID, TenantName: t.TenantName,
		AppID: t.AppID, AppName: t.AppName,
		User: t.UserEmail, AllowedModels: t.AllowedModels, Expires: t.ExpiresAt,
	}
	// Never cache a token past its expiry.
	expires := now.Add(p.positiveTTL)
	if t.ExpiresAt.Before(expires) {
		expires = t.ExpiresAt
	}
	p.cache.Store(hash, cacheEntry{id: id, expires: expires})
	return id, nil
}

// ForgetAll clears the cache, so a revocation applies immediately on this node.
func (p *ProxyTokens) ForgetAll() { p.cache.Clear() }
