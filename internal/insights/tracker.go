package insights

import (
	"sync"
	"time"
)

// Cache statuses recorded on each usage event.
const (
	CacheHit              = "hit"
	CacheNewPrefix        = "miss_new_prefix"           // nothing cacheable was sent recently
	CacheTooShort         = "miss_too_short"            // below the provider's minimum cacheable size
	CacheUnexpectedMiss   = "miss_unexpected"           // the same prefix was sent recently but wasn't cached
	CacheInstructionsDyn  = "miss_instructions_dynamic" // instructions differ only in numbers or ids
	CacheInstructionsChg  = "miss_instructions_changed"
	CacheToolsReordered   = "miss_tools_reordered" // same tools, different order or key order
	CacheToolsChanged     = "miss_tools_changed"
	CacheHistoryRewritten = "miss_history_rewritten" // earlier turns of the conversation changed
	CacheUnknown          = "unknown"                // previous_response_id: the prefix isn't visible
	CacheRerouted         = "rerouted"               // the conversation moved to another deployment, whose cache is cold
)

// UnstablePrefix reports whether a status means the application itself keeps
// changing the start of its prompt.
func UnstablePrefix(status string) bool {
	switch status {
	case CacheInstructionsDyn, CacheInstructionsChg, CacheToolsReordered, CacheToolsChanged, CacheHistoryRewritten:
		return true
	}
	return false
}

// Expectation is what the tracker knew before the request went upstream.
type Expectation struct {
	MatchedTokens int    // tokens of the longest prefix seen within the TTL
	Divergence    string // how this prompt differs from the scope's previous one
}

type scopeKey struct {
	scope string // tenant/app/model
	hash  uint64
}

type prevKey struct {
	scope    string
	cacheKey string
}

// Tracker remembers recently sent prefixes per scope. A prefix counts as
// "seen" from the moment the request that sent it completed, because that is
// when the provider could have cached it.
type Tracker struct {
	ttl       time.Duration
	minTokens int

	mu   sync.Mutex
	seen map[scopeKey]time.Time
	prev map[prevKey]*Fingerprint
}

func NewTracker(ttl time.Duration, minTokens int) *Tracker {
	return &Tracker{ttl: ttl, minTokens: minTokens, seen: map[scopeKey]time.Time{}, prev: map[prevKey]*Fingerprint{}}
}

// Before looks up a request's prefix at the time it is sent.
func (t *Tracker) Before(scope string, fp *Fingerprint, now time.Time) Expectation {
	var exp Expectation
	if fp.HasPrevious || len(fp.Cumulative) == 0 {
		return exp
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(fp.Cumulative) - 1; i >= 0; i-- {
		if at, ok := t.seen[scopeKey{scope, fp.Cumulative[i]}]; ok && now.Sub(at) <= t.ttl && at.Before(now) {
			exp.MatchedTokens = fp.CumTokens[i]
			break
		}
	}
	if p := t.prev[prevKey{scope, fp.CacheKey}]; p != nil {
		exp.Divergence = divergence(p, fp)
	}
	return exp
}

// After records a request's prefix once the upstream has answered.
func (t *Tracker) After(scope string, fp *Fingerprint, now time.Time) {
	if fp.HasPrevious || len(fp.Cumulative) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range fp.Cumulative {
		t.seen[scopeKey{scope, h}] = now
	}
	t.prev[prevKey{scope, fp.CacheKey}] = fp
}

// Prune forgets prefixes older than the TTL.
func (t *Tracker) Prune(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, at := range t.seen {
		if now.Sub(at) > t.ttl {
			delete(t.seen, k)
		}
	}
	if len(t.prev) > 100_000 {
		t.prev = map[prevKey]*Fingerprint{}
	}
}

// divergence explains where cur's prompt departs from prev's. Different
// conversations naturally differ from their first item, so only a shared
// first item followed by a change counts as rewritten history.
func divergence(prev, cur *Fingerprint) string {
	if prev.Parts[segInstructions] != cur.Parts[segInstructions] {
		if prev.InstructionsMasked == cur.InstructionsMasked {
			return CacheInstructionsDyn
		}
		return CacheInstructionsChg
	}
	if prev.Parts[segTools] != cur.Parts[segTools] {
		if prev.ToolsCanonical == cur.ToolsCanonical {
			return CacheToolsReordered
		}
		return CacheToolsChanged
	}
	n := min(len(prev.Parts), len(cur.Parts))
	for i := segItem; i < n; i++ {
		if prev.Parts[i] != cur.Parts[i] {
			if i > segItem {
				return CacheHistoryRewritten
			}
			return ""
		}
	}
	return ""
}

// Classify decides the cache status once the upstream reported how many
// input tokens it served from cache. inputTokens is the upstream's count, or
// 0 to fall back to the fingerprint's estimate.
func (t *Tracker) Classify(fp *Fingerprint, exp Expectation, inputTokens, cachedTokens int) (status string, expected int) {
	if inputTokens <= 0 {
		inputTokens = fp.TotalTokens
	}
	switch {
	case fp.HasPrevious:
		return CacheUnknown, 0
	case cachedTokens > 0:
		return CacheHit, exp.MatchedTokens
	case inputTokens < t.minTokens:
		return CacheTooShort, 0
	case exp.MatchedTokens >= t.minTokens:
		return CacheUnexpectedMiss, exp.MatchedTokens
	case exp.Divergence != "":
		return exp.Divergence, 0
	default:
		return CacheNewPrefix, 0
	}
}
