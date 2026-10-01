package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/insights"
	"github.com/omni-proxy/omni-proxy/internal/store"
)

var longText = strings.Repeat("You are a careful support assistant. ", 20)

func (h *harness) send(instructions, user string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "gpt", "instructions": instructions, "input": user})
	resp := h.post(trustedKey, string(body))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("status %d", resp.StatusCode)
	}
}

func (h *harness) insights(status string) []store.Insight {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.proxy.URL+"/admin/insights?status="+status, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Data []store.Insight }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out.Data
}

func cacheStatuses(rows []store.UsageRow) map[string]int64 {
	m := map[string]int64{}
	for _, r := range rows {
		m[r.Group["cache"]] += r.Requests
	}
	return m
}

func TestDynamicInstructionsRaiseInsight(t *testing.T) {
	h := newHarness(t)
	// A timestamp at the top of the instructions busts the cache every time.
	for i := range 8 {
		h.send(fmt.Sprintf("Current time: 2026-09-29T10:%02d:00Z. %s", i, longText), "hello")
	}
	rows := h.usage(store.UsageQuery{GroupBy: []string{"cache"}}, 8)
	got := cacheStatuses(rows)
	if got[insights.CacheNewPrefix] != 1 || got[insights.CacheInstructionsDyn] != 7 {
		t.Fatalf("cache statuses %v", got)
	}
	for _, r := range rows {
		if lost := r.MissedCostUSD; (r.Group["cache"] == insights.CacheInstructionsDyn) != (lost > 0) {
			t.Fatalf("%s lost $%v", r.Group["cache"], lost)
		}
	}

	h.srv.Insights().Evaluate(context.Background(), time.Now())
	open := h.insights("open")
	if len(open) != 1 || open[0].Kind != insights.KindPrefixUnstable {
		t.Fatalf("open insights %+v", open)
	}
	in := open[0]
	if usd, _ := in.Evidence["missed_savings_usd"].(float64); usd <= 0 {
		t.Fatalf("evidence %v", in.Evidence)
	}
	if in.Severity != "critical" || in.Evidence["top_reason"] != insights.CacheInstructionsDyn || !strings.Contains(in.Detail, "timestamp") {
		t.Fatalf("insight %+v", in)
	}
	if !strings.HasPrefix(in.Title, "chat: 88% of gpt requests") {
		t.Fatalf("title %q", in.Title)
	}

	// Once the traffic goes quiet, the insight resolves.
	h.srv.Insights().Evaluate(context.Background(), time.Now().Add(time.Hour))
	if open := h.insights("open"); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	if res := h.insights("resolved"); len(res) != 1 || res[0].ResolvedAt == nil {
		t.Fatalf("resolved %+v", res)
	}
}

func TestStablePrefixHitsAndCosts(t *testing.T) {
	h := newHarness(t)
	for range 6 {
		h.send(longText, "hello")
	}
	rows := h.usage(store.UsageQuery{GroupBy: []string{"cache"}}, 6)
	got := cacheStatuses(rows)
	if got[insights.CacheNewPrefix] != 1 || got[insights.CacheHit] != 5 {
		t.Fatalf("cache statuses %v", got)
	}
	var cost float64
	for _, r := range rows {
		cost += r.CostUSD
	}
	if cost <= 0 {
		t.Fatalf("cost %v", cost)
	}
	h.srv.Insights().Evaluate(context.Background(), time.Now())
	if open := h.insights("open"); len(open) != 0 {
		t.Fatalf("unexpected insights %+v", open)
	}
}

func TestUnexpectedMissRaisesInsight(t *testing.T) {
	h := newHarness(t)
	// The upstream never caches this prefix even though it repeats.
	for range 7 {
		h.send("NOCACHE "+longText, "hello")
	}
	rows := h.usage(store.UsageQuery{GroupBy: []string{"cache"}}, 7)
	if got := cacheStatuses(rows); got[insights.CacheUnexpectedMiss] != 6 {
		t.Fatalf("cache statuses %v", got)
	}
	var lost float64
	for _, r := range rows {
		lost += r.MissedCostUSD
	}
	if lost <= 0 {
		t.Fatalf("no money lost: %+v", rows)
	}
	h.srv.Insights().Evaluate(context.Background(), time.Now())
	open := h.insights("open")
	if len(open) != 1 || open[0].Kind != insights.KindUnexpectedMiss {
		t.Fatalf("open insights %+v", open)
	}
	if n, _ := open[0].Evidence["missed_cached_tokens"].(float64); n <= 0 {
		t.Fatalf("evidence %v", open[0].Evidence)
	}
	if usd, _ := open[0].Evidence["missed_savings_usd"].(float64); usd <= 0 {
		t.Fatalf("evidence %v", open[0].Evidence)
	}
}

func TestTruncationRaisesInsight(t *testing.T) {
	h := newHarness(t)
	for range 5 {
		h.send("", "please HIT_MAX_TOKENS")
	}
	h.usage(store.UsageQuery{}, 5)
	h.srv.Insights().Evaluate(context.Background(), time.Now())
	open := h.insights("open")
	if len(open) != 1 || open[0].Kind != insights.KindTruncation {
		t.Fatalf("open insights %+v", open)
	}
}

func (h *harness) admin(method, path, body string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.proxy.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func TestPriceChangeKeepsPastCosts(t *testing.T) {
	h := newHarness(t)
	h.send("", "hello")
	before := h.usage(store.UsageQuery{GroupBy: []string{"none"}}, 1)[0].CostUSD

	// A tenfold price change applies from now on only.
	resp := h.admin(http.MethodPost, "/admin/prices", `{"model":"gpt","input":20,"cached_input":5,"output":80}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add price: %d", resp.StatusCode)
	}
	resp.Body.Close()
	h.send("", "hello")
	rows := h.usage(store.UsageQuery{GroupBy: []string{"none"}}, 2)
	if after := rows[0].CostUSD - before; before <= 0 || math.Abs(after-10*before) > 1e-9 {
		t.Fatalf("cost before %v, after %v", before, after)
	}

	all := decode(t, h.admin(http.MethodGet, "/admin/prices", ""))["data"].([]any)
	current := decode(t, h.admin(http.MethodGet, "/admin/prices?current=true", ""))["data"].([]any)
	if len(all) != 2 || len(current) != 1 || current[0].(map[string]any)["input"] != 20.0 {
		t.Fatalf("all %v current %v", all, current)
	}
	if resp := h.admin(http.MethodPost, "/admin/prices", `{"model":"gpt","input":-1,"output":1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative price accepted: %d", resp.StatusCode)
	}
}
