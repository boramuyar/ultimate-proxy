package insights

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

var pad = strings.Repeat("stable system prompt text ", 40)

func classify(t *testing.T, tr *Tracker, bodies ...string) []string {
	t.Helper()
	now := time.Now()
	var out []string
	for _, b := range bodies {
		fp := Compute([]byte(b))
		exp := tr.Before("s", fp, now)
		st, _ := tr.Classify(fp, exp, 0, 0) // the upstream never caches here
		tr.After("s", fp, now)
		now = now.Add(time.Second)
		out = append(out, st)
	}
	return out
}

func TestClassifyDivergence(t *testing.T) {
	cases := []struct {
		name   string
		second string
		want   string
	}{
		{"dynamic instructions", `{"instructions":"Today is 2026-09-30. ` + pad + `","input":"hi ` + pad + `"}`, CacheInstructionsDyn},
		{"changed instructions", `{"instructions":"Be terse. ` + pad + `","input":"hi ` + pad + `"}`, CacheInstructionsChg},
		{"tools reordered", `{"instructions":"Today is 2026-09-29. ` + pad + `","tools":[{"type":"function","name":"b"},{"name":"a","type":"function"}],"input":"hi ` + pad + `"}`, CacheToolsReordered},
		{"tools changed", `{"instructions":"Today is 2026-09-29. ` + pad + `","tools":[{"type":"function","name":"a"},{"type":"function","name":"c"}],"input":"hi ` + pad + `"}`, CacheToolsChanged},
		{"history rewritten", `{"instructions":"Today is 2026-09-29. ` + pad + `","tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"input":[{"role":"user","content":"hi"},{"role":"assistant","content":"summary"},{"role":"user","content":"more ` + pad + `"}]}`, CacheHistoryRewritten},
		{"identical prefix missed", `{"instructions":"Today is 2026-09-29. ` + pad + `","tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"input":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello ` + pad + `"},{"role":"user","content":"again"}]}`, CacheUnexpectedMiss},
	}
	first := `{"instructions":"Today is 2026-09-29. ` + pad + `","tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"input":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello ` + pad + `"}]}`
	// The shared instructions alone are below the minimum, so a divergence
	// right after them explains the miss; a longer shared prefix should hit.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(t, NewTracker(5*time.Minute, 300), first, c.second)
			if got[0] != CacheNewPrefix || got[1] != c.want {
				t.Fatalf("got %v, want [%s %s]", got, CacheNewPrefix, c.want)
			}
		})
	}
}

func TestClassifyOther(t *testing.T) {
	tr := NewTracker(5*time.Minute, 1000)
	if got := classify(t, tr, `{"input":"short"}`); got[0] != CacheTooShort {
		t.Fatal(got)
	}
	if got := classify(t, tr, `{"previous_response_id":"resp_1","input":"x"}`); got[0] != CacheUnknown {
		t.Fatal(got)
	}
	fp := Compute([]byte(`{"instructions":"` + pad + `"}`))
	if st, _ := tr.Classify(fp, Expectation{}, 2000, 1500); st != CacheHit {
		t.Fatal(st)
	}
	// Separate conversations sharing only the system prompt are not rewrites.
	tr = NewTracker(5*time.Minute, 10)
	got := classify(t, tr, `{"instructions":"x","input":[{"role":"user","content":"`+pad+`"}]}`, `{"instructions":"x","input":[{"role":"user","content":"other `+pad+`"}]}`)
	if got[1] != CacheNewPrefix {
		t.Fatal(got)
	}
}

func TestSeenExpiresAfterTTL(t *testing.T) {
	tr := NewTracker(time.Minute, 10)
	fp := Compute([]byte(`{"instructions":"` + pad + `"}`))
	now := time.Now()
	tr.After("s", fp, now)
	if exp := tr.Before("s", fp, now.Add(30*time.Second)); exp.MatchedTokens == 0 {
		t.Fatal("expected a match within the TTL")
	}
	if exp := tr.Before("s", fp, now.Add(2*time.Minute)); exp.MatchedTokens != 0 {
		t.Fatal("expected no match after the TTL")
	}
	if exp := tr.Before("other", fp, now); exp.MatchedTokens != 0 {
		t.Fatal("scopes must not share prefixes")
	}
}

type recorder struct{ events []string }

func (r *recorder) Notify(event string, in store.Insight) {
	r.events = append(r.events, event+":"+in.Kind)
}

func TestEngineHysteresis(t *testing.T) {
	cfg := config.Insights{Window: 10 * time.Minute, MinRequests: 10, UnstablePrefixRate: 0.3, UnexpectedMissRate: 0.3, ErrorRate: 0.2, TruncationRate: 0.5}
	st := store.NewMemory()
	rec := &recorder{}
	e := NewEngine(cfg, st, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	obs := func(at time.Time, failed bool) {
		o := Observation{TS: at, TenantID: "t", AppID: "a", AppName: "app", Model: "m", Status: "completed"}
		if failed {
			o.Status, o.ErrorCode = "failed", "server_error"
		}
		e.Record(o)
	}
	for i := range 10 {
		obs(base, i < 3) // 30% errors
	}
	e.Evaluate(ctx, base.Add(time.Minute))
	if len(rec.events) != 1 || rec.events[0] != "opened:error_rate" {
		t.Fatalf("events %v", rec.events)
	}
	// 15% is below the threshold but above half of it: stays open.
	for range 10 {
		obs(base.Add(2*time.Minute), false)
	}
	e.Evaluate(ctx, base.Add(3*time.Minute))
	if len(rec.events) != 1 {
		t.Fatalf("events %v", rec.events)
	}
	// 3/40 = 7.5% is below half: resolves.
	for range 20 {
		obs(base.Add(4*time.Minute), false)
	}
	e.Evaluate(ctx, base.Add(5*time.Minute))
	if len(rec.events) != 2 || rec.events[1] != "resolved:error_rate" {
		t.Fatalf("events %v", rec.events)
	}
	all, _ := st.ListInsights(ctx, "")
	if len(all) != 1 || all[0].Status != "resolved" || !strings.Contains(all[0].Detail, "server_error (3)") || !strings.Contains(all[0].Title, "15%") {
		t.Fatalf("stored %+v", all)
	}
}

func TestBudgetInsight(t *testing.T) {
	st := store.NewMemory()
	rec := &recorder{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := NewEngine(config.Insights{Window: 10 * time.Minute}, st, rec, quiet)
	ctx := context.Background()
	resets := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rule := store.Limit{ID: "lim_1", TenantID: "t", User: "*", Kind: store.LimitBudgetUSD, Amount: 10, Period: "day", Enforcement: "hard"}
	alert := func(used float64) {
		e.Budget(ctx, BudgetAlert{Rule: rule, User: "ann@x.com", Used: used, ResetsAt: resets, Amount: "$10.00", UsedText: "$x"})
	}
	alert(8)
	alert(8.5) // same severity: updated, not notified again
	alert(10)
	if strings.Join(rec.events, ",") != "opened:budget_threshold,escalated:budget_threshold" {
		t.Fatalf("events %v", rec.events)
	}
	open, _ := st.ListInsights(ctx, "open")
	if len(open) != 1 || open[0].Severity != "critical" || !strings.Contains(open[0].Title, "Daily spend budget for ann@x.com is 100% used") {
		t.Fatalf("open %+v", open)
	}
	// Quiet traffic does not resolve it; a restart keeps it; the period end does.
	e.Evaluate(ctx, time.Now())
	e2 := NewEngine(config.Insights{Window: 10 * time.Minute}, st, rec, quiet)
	if err := e2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	e2.Evaluate(ctx, time.Now())
	if open, _ := st.ListInsights(ctx, "open"); len(open) != 1 {
		t.Fatal("resolved before the period ended")
	}
	e2.Evaluate(ctx, resets.Add(time.Second))
	if open, _ := st.ListInsights(ctx, "open"); len(open) != 0 {
		t.Fatal("still open after the period ended")
	}
}
