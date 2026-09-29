// Package pricing turns token usage into cost.
package pricing

import "github.com/boramuyar/ultimate-proxy/internal/config"

type price struct {
	input, cachedInput, cacheWrite, output float64 // USD per token
}

// Table looks up prices by model alias, upstream model or provider/upstream.
type Table struct {
	byModel map[string]price
}

func New(prices []config.Price) *Table {
	t := &Table{byModel: map[string]price{}}
	for _, p := range prices {
		pr := price{input: p.Input / 1e6, cachedInput: p.Input / 1e6, cacheWrite: p.Input / 1e6, output: p.Output / 1e6}
		if p.CachedInput != nil {
			pr.cachedInput = *p.CachedInput / 1e6
		}
		if p.CacheWrite != nil {
			pr.cacheWrite = *p.CacheWrite / 1e6
		}
		t.byModel[p.Model] = pr
	}
	return t
}

// Usage is the token counts a cost is computed from. InputTokens includes
// cached and cache-write tokens; OutputTokens includes reasoning tokens.
type Usage struct {
	InputTokens, CachedInputTokens, CacheWriteTokens, OutputTokens int
}

// Cost returns the USD cost of a request, and false when no price is known.
func (t *Table) Cost(alias, provider, upstream string, u Usage) (float64, bool) {
	p, ok := t.byModel[alias]
	if !ok {
		p, ok = t.byModel[provider+"/"+upstream]
	}
	if !ok {
		p, ok = t.byModel[upstream]
	}
	if !ok {
		return 0, false
	}
	uncached := max(u.InputTokens-u.CachedInputTokens-u.CacheWriteTokens, 0)
	return float64(uncached)*p.input +
		float64(u.CachedInputTokens)*p.cachedInput +
		float64(u.CacheWriteTokens)*p.cacheWrite +
		float64(u.OutputTokens)*p.output, true
}

// SavingsPerCachedToken is how much a cache hit saves per input token.
func (t *Table) SavingsPerCachedToken(alias, provider, upstream string) float64 {
	c1, ok := t.Cost(alias, provider, upstream, Usage{InputTokens: 1})
	if !ok {
		return 0
	}
	c2, _ := t.Cost(alias, provider, upstream, Usage{InputTokens: 1, CachedInputTokens: 1})
	return c1 - c2
}
