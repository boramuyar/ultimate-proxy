package store

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
)

// stores returns the memory store and, when TEST_DATABASE_URL or
// TEST_CLICKHOUSE_URL is set, a Postgres store and a store keeping usage in
// ClickHouse, so every implementation answers the same queries the same way.
func stores(t *testing.T) map[string]Store {
	t.Helper()
	out := map[string]Store{"memory": NewMemory()}
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		pg, err := NewPostgres(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.pool.Exec(context.Background(), "TRUNCATE usage_events, insights, model_prices, api_keys, applications, tenants"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pg.Close)
		out["postgres"] = pg
	}
	if url := os.Getenv("TEST_CLICKHOUSE_URL"); url != "" {
		ch := testClickHouse(t, url)
		st := WithUsage(NewMemory(), ch)
		t.Cleanup(st.Close)
		out["clickhouse"] = st
	}
	return out
}

func TestStores(t *testing.T) {
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ten, err := st.EnsureTenant(ctx, "acme")
			if err != nil {
				t.Fatal(err)
			}
			again, _ := st.EnsureTenant(ctx, "acme")
			if again.ID != ten.ID {
				t.Fatal("EnsureTenant is not idempotent")
			}
			if _, err := st.EnsureApplication(ctx, "ten_missing", "x", true); !errors.Is(err, ErrNotFound) {
				t.Fatalf("want ErrNotFound for a missing tenant, got %v", err)
			}
			app, err := st.EnsureApplication(ctx, ten.ID, "chat", false)
			if err != nil {
				t.Fatal(err)
			}
			key, err := st.CreateKey(ctx, app.ID, "hash1", "up_abc")
			if err != nil {
				t.Fatal(err)
			}
			p, err := st.LookupKey(ctx, "hash1")
			if err != nil || p.TenantName != "acme" || p.AppName != "chat" || p.CanAssertUsers {
				t.Fatalf("LookupKey = %+v, %v", p, err)
			}
			if err := st.RevokeKey(ctx, key.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.LookupKey(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("revoked key still resolves: %v", err)
			}

			base := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
			ttft := 120
			events := []UsageEvent{
				{TS: base, UserEmail: "a@x.com", Model: "m1", InputTokens: 100, CachedInputTokens: 40, OutputTokens: 10, Status: "completed", TTFTMS: &ttft},
				{TS: base.Add(time.Minute), UserEmail: "a@x.com", Model: "m1", InputTokens: 50, OutputTokens: 5, Status: "completed"},
				{TS: base.Add(2 * time.Hour), UserEmail: "b@x.com", Model: "m2", InputTokens: 7, OutputTokens: 3, Status: "failed", CostUSD: 0.25, CacheStatus: "miss_tools_changed"},
			}
			for i := range events {
				events[i].RequestID = openresponses.NewID("resp")
				events[i].TenantID, events[i].AppID, events[i].KeyID = ten.ID, app.ID, key.ID
			}
			if err := st.InsertUsage(ctx, events); err != nil {
				t.Fatal(err)
			}
			rows, err := st.QueryUsage(ctx, UsageQuery{From: base.Add(-time.Hour), To: base.Add(3 * time.Hour), GroupBy: []string{"email"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].Group["email"] != "a@x.com" || rows[0].InputTokens != 150 || rows[0].CachedInputTokens != 40 || rows[0].TotalTokens != 165 {
				t.Fatalf("group by email: %+v", rows)
			}
			if rows[1].FailedRequests != 1 {
				t.Fatalf("failed requests not counted: %+v", rows[1])
			}
			rows, err = st.QueryUsage(ctx, UsageQuery{From: base.Add(-time.Hour), To: base.Add(3 * time.Hour), Granularity: "hour", Filters: map[string]string{"model": "m1"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || !rows[0].Bucket.Equal(base.Truncate(time.Hour)) || rows[0].Requests != 2 {
				t.Fatalf("hourly buckets: %+v", rows)
			}
			rows, err = st.QueryUsage(ctx, UsageQuery{From: base.Add(-time.Hour), To: base.Add(3 * time.Hour), GroupBy: []string{"cache"}, Filters: map[string]string{"cache": "miss_tools_changed"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Requests != 1 || rows[0].CostUSD != 0.25 {
				t.Fatalf("cache status filter: %+v", rows)
			}

			in := &Insight{ID: openresponses.NewID("ins"), Kind: "error_rate", Severity: "warning", Status: "open", TenantID: ten.ID, AppID: app.ID,
				Model: "m1", Title: "t", Detail: "d", Evidence: map[string]any{"rate": 0.5}, FirstSeen: base, LastSeen: base}
			if err := st.SaveInsight(ctx, in); err != nil {
				t.Fatal(err)
			}
			resolved := base.Add(time.Minute)
			in.Status, in.ResolvedAt, in.LastSeen = "resolved", &resolved, resolved
			if err := st.SaveInsight(ctx, in); err != nil {
				t.Fatal(err)
			}
			open, _ := st.ListInsights(ctx, "open")
			all, err := st.ListInsights(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(open) != 0 || len(all) != 1 || all[0].ResolvedAt == nil || all[0].Evidence["rate"] != 0.5 {
				t.Fatalf("insights: open %+v all %+v", open, all)
			}

			keys, err := st.ListKeys(ctx, app.ID)
			if err != nil || len(keys) != 1 || keys[0].ID != key.ID || keys[0].RevokedAt == nil {
				t.Fatalf("list keys: %+v %v", keys, err)
			}

			jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, pr := range []*Price{
				{ID: openresponses.NewID("price"), Model: "m1", Input: 2, CachedInput: 1, CacheWrite: 2, Output: 8, EffectiveFrom: jan.AddDate(0, 5, 0)},
				{ID: openresponses.NewID("price"), Model: "m1", Input: 3, CachedInput: 1, CacheWrite: 3, Output: 9, EffectiveFrom: jan},
			} {
				if err := st.AddPrice(ctx, pr); err != nil {
					t.Fatal(err)
				}
			}
			prices, err := st.ListPrices(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(prices) != 2 || prices[0].Input != 3 || !prices[1].EffectiveFrom.Equal(jan.AddDate(0, 5, 0)) {
				t.Fatalf("prices %+v", prices)
			}
		})
	}
}

// testClickHouse keeps raw events forever, so fixed test dates never expire.
func testClickHouse(t *testing.T, url string) *ClickHouse {
	t.Helper()
	ch, err := NewClickHouse(context.Background(), url, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"usage_events", "usage_hourly"} {
		if err := ch.exec(context.Background(), "TRUNCATE TABLE "+table, nil, ch.db); err != nil {
			t.Fatal(err)
		}
	}
	return ch
}

// TestClickHouseRollup checks that usage stays queryable from the hourly
// rollup after the raw events are gone, and that partial hours at the edges of
// a range come from the raw events.
func TestClickHouseRollup(t *testing.T) {
	url := os.Getenv("TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("TEST_CLICKHOUSE_URL not set")
	}
	ctx := context.Background()
	ch := testClickHouse(t, url)
	defer ch.Close()

	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	var events []UsageEvent
	for i := 0; i < 4; i++ { // 10:10, 10:40, 11:10, 11:40
		events = append(events, UsageEvent{
			TS: base.Add(time.Duration(10+30*i) * time.Minute), RequestID: openresponses.NewID("resp"),
			TenantID: "t1", AppID: "a1", UserEmail: "a@x.com", Model: "m1", Status: "completed",
			InputTokens: 100, OutputTokens: 10, CostUSD: 0.5,
		})
	}
	events[3].Status = "failed"
	if err := ch.InsertUsage(ctx, events); err != nil {
		t.Fatal(err)
	}
	query := func(from, to time.Time) UsageRow {
		t.Helper()
		rows, err := ch.QueryUsage(ctx, UsageQuery{From: from, To: to, GroupBy: []string{"application"}, Filters: map[string]string{"model": "m1"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("want one row, got %+v", rows)
		}
		return rows[0]
	}
	// 10:30 to 11:30 is two partial hours, all from raw events.
	if r := query(base.Add(30*time.Minute), base.Add(90*time.Minute)); r.Requests != 2 || r.InputTokens != 200 {
		t.Fatalf("partial hours: %+v", r)
	}
	// 10:30 to 12:00: 10:40 raw, 11:00-12:00 from the rollup.
	if r := query(base.Add(30*time.Minute), base.Add(2*time.Hour)); r.Requests != 3 || r.FailedRequests != 1 || r.CostUSD != 1.5 {
		t.Fatalf("mixed: %+v", r)
	}

	// Drop the raw events, as the retention TTL would.
	if err := ch.exec(ctx, "TRUNCATE TABLE usage_events", nil, ch.db); err != nil {
		t.Fatal(err)
	}
	if r := query(base, base.Add(2*time.Hour)); r.Requests != 4 || r.OutputTokens != 40 || r.TotalTokens != 440 {
		t.Fatalf("rollup only: %+v", r)
	}
	rows, err := ch.QueryUsage(ctx, UsageQuery{From: base, To: base.Add(2 * time.Hour), Granularity: "hour"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows[1].Bucket.Equal(base.Add(time.Hour)) || rows[1].FailedRequests != 1 {
		t.Fatalf("hourly from rollup: %+v", rows)
	}
}

func TestClickHouseRetention(t *testing.T) {
	url := os.Getenv("TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("TEST_CLICKHOUSE_URL not set")
	}
	ctx := context.Background()
	ttl := func(ch *ClickHouse) string {
		rc, err := ch.do(ctx, "SELECT create_table_query FROM system.tables WHERE database = currentDatabase() AND name = 'usage_events'", nil, ch.db, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	for _, c := range []struct {
		days int
		want string
	}{{30, "toIntervalDay(30)"}, {90, "toIntervalDay(90)"}, {0, ""}} {
		ch, err := NewClickHouse(ctx, url, c.days)
		if err != nil {
			t.Fatal(err)
		}
		got := ttl(ch)
		if c.want == "" && strings.Contains(got, " TTL ") || c.want != "" && !strings.Contains(got, c.want) {
			t.Fatalf("retention %d days: table is %s", c.days, got)
		}
		ch.Close()
	}
}
