package pricing

import (
	"math"
	"testing"

	"github.com/boramuyar/ultimate-proxy/internal/config"
)

func TestCost(t *testing.T) {
	cached := 0.25
	tb := New([]config.Price{{Model: "gpt-5", Input: 2, CachedInput: &cached, Output: 10}, {Model: "openai/o3", Input: 1, Output: 1}})
	c, ok := tb.Cost("smart", "openai", "gpt-5", Usage{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000})
	if !ok || math.Abs(c-(0.6*2+0.4*0.25+0.1*10)) > 1e-9 {
		t.Fatalf("cost %v %v", c, ok)
	}
	if _, ok := tb.Cost("o3", "openai", "o3", Usage{}); !ok {
		t.Fatal("provider/model price not found")
	}
	if _, ok := tb.Cost("x", "openai", "unknown", Usage{}); ok {
		t.Fatal("unexpected price")
	}
	if s := tb.SavingsPerCachedToken("smart", "openai", "gpt-5"); math.Abs(s-1.75e-6) > 1e-12 {
		t.Fatalf("savings %v", s)
	}
}
