package pricing

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/store"
)

func TestCostUsesPriceInEffect(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []store.Price{
		{Model: "gpt-5", Input: 2, CachedInput: 0.25, CacheWrite: 2, Output: 10, EffectiveFrom: jan},
		{Model: "gpt-5", Input: 1, CachedInput: 0.1, CacheWrite: 1, Output: 5, EffectiveFrom: jun},
		{Model: "openai/o3", Input: 1, CachedInput: 1, CacheWrite: 1, Output: 1, EffectiveFrom: jan},
	} {
		_ = st.AddPrice(ctx, &p)
	}
	tb := New(st, nil)
	if _, ok := tb.Cost(jun, "smart", "openai", "gpt-5", Usage{}); ok {
		t.Fatal("price known before Reload")
	}
	if err := tb.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	u := Usage{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000}
	c, ok := tb.Cost(jan.Add(time.Hour), "smart", "openai", "gpt-5", u)
	if !ok || math.Abs(c-(0.6*2+0.4*0.25+0.1*10)) > 1e-9 {
		t.Fatalf("january cost %v %v", c, ok)
	}
	c, _ = tb.Cost(jun.Add(time.Hour), "smart", "openai", "gpt-5", u)
	if math.Abs(c-(0.6*1+0.4*0.1+0.1*5)) > 1e-9 {
		t.Fatalf("june cost %v", c)
	}
	if _, ok := tb.Cost(jan.Add(-time.Hour), "smart", "openai", "gpt-5", u); ok {
		t.Fatal("price used before it took effect")
	}
	if _, ok := tb.Cost(jan, "o3", "openai", "o3", Usage{}); !ok {
		t.Fatal("provider/model price not found")
	}
	if s := tb.SavingsPerCachedToken(jan, "smart", "openai", "gpt-5"); math.Abs(s-1.75e-6) > 1e-12 {
		t.Fatalf("savings %v", s)
	}
}
