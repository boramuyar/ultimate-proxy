package routing

import (
	"sync"
	"time"
)

// maxSessions bounds the affinity map. Past it, expired entries are swept
// and, if that is not enough, arbitrary ones dropped: losing one only costs
// a cache miss.
const maxSessions = 200_000

// affinity remembers which deployment served a session, so its next turn
// can go where its prompt prefix is cached.
type affinity struct {
	mu sync.Mutex
	m  map[string]sticky
}

type sticky struct {
	d       *Deployment
	expires time.Time
}

// Sticky returns the deployment that last served a session, if that was
// recent enough for its cache to be warm.
func (r *Router) Sticky(key string) *Deployment {
	r.aff.mu.Lock()
	defer r.aff.mu.Unlock()
	s, ok := r.aff.m[key]
	if !ok {
		return nil
	}
	if !r.now().Before(s.expires) {
		delete(r.aff.m, key)
		return nil
	}
	return s.d
}

// Stick records that d served a session, for its provider's cache TTL.
// Unless always is set, it does nothing for providers that turned
// stickiness off or have a single deployment.
func (r *Router) Stick(key string, d *Deployment, always bool) {
	pool := r.pools[d.Provider]
	if !always && (!pool.Sticky || len(pool.Deployments) < 2) {
		return
	}
	now := r.now()
	r.aff.mu.Lock()
	defer r.aff.mu.Unlock()
	if len(r.aff.m) >= maxSessions {
		for k, s := range r.aff.m {
			if !now.Before(s.expires) {
				delete(r.aff.m, k)
			}
		}
		for k := range r.aff.m {
			if len(r.aff.m) < maxSessions*9/10 {
				break
			}
			delete(r.aff.m, k)
		}
	}
	r.aff.m[key] = sticky{d, now.Add(pool.CacheTTL)}
}

// Spread reports whether a provider has several deployments, so which one
// serves a session matters.
func (t Target) Spread() bool { return len(t.Pool.Deployments) > 1 }
