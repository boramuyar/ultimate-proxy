// Package pricing turns token usage into cost, from the prices table.
package pricing

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/store"
)

type price struct {
	from                                   time.Time
	input, cachedInput, cacheWrite, output float64 // USD per token
}

// Table is an in-memory copy of the prices table, so cost lookups never touch
// the database. Reload refreshes it.
type Table struct {
	store store.Store
	log   *slog.Logger
	snap  atomic.Pointer[map[string][]price] // by model, oldest effective first
}

func New(st store.Store, log *slog.Logger) *Table {
	t := &Table{store: st, log: log}
	empty := map[string][]price{}
	t.snap.Store(&empty)
	return t
}

// Reload reads every price from the store.
func (t *Table) Reload(ctx context.Context) error {
	rows, err := t.store.ListPrices(ctx)
	if err != nil {
		return err
	}
	m := map[string][]price{}
	for _, p := range rows {
		m[p.Model] = append(m[p.Model], price{
			from: p.EffectiveFrom, input: p.Input / 1e6, cachedInput: p.CachedInput / 1e6,
			cacheWrite: p.CacheWrite / 1e6, output: p.Output / 1e6,
		})
	}
	for _, list := range m {
		sort.SliceStable(list, func(i, j int) bool { return list[i].from.Before(list[j].from) })
	}
	t.snap.Store(&m)
	return nil
}

// Run reloads the table every interval, so prices added through another
// replica take effect here too.
func (t *Table) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := t.Reload(ctx); err != nil && ctx.Err() == nil {
				t.log.Error("reloading prices failed", "err", err)
			}
		}
	}
}

// Usage is the token counts a cost is computed from. InputTokens includes
// cached and cache-write tokens; OutputTokens includes reasoning tokens.
type Usage struct {
	InputTokens, CachedInputTokens, CacheWriteTokens, OutputTokens int
}

// lookup finds the price in effect at a time, trying the alias, then
// provider/upstream, then the bare upstream model.
func (t *Table) lookup(at time.Time, alias, provider, upstream string) (price, bool) {
	m := *t.snap.Load()
	for _, key := range []string{alias, provider + "/" + upstream, upstream} {
		list := m[key]
		for i := len(list) - 1; i >= 0; i-- {
			if !list[i].from.After(at) {
				return list[i], true
			}
		}
	}
	return price{}, false
}

// Cost returns the USD cost of a request made at a time, and false when no
// price was in effect.
func (t *Table) Cost(at time.Time, alias, provider, upstream string, u Usage) (float64, bool) {
	p, ok := t.lookup(at, alias, provider, upstream)
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
func (t *Table) SavingsPerCachedToken(at time.Time, alias, provider, upstream string) float64 {
	p, ok := t.lookup(at, alias, provider, upstream)
	if !ok {
		return 0
	}
	return p.input - p.cachedInput
}
