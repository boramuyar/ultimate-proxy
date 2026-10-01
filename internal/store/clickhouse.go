package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UsageStore keeps the request log: usage events and the queries over them.
type UsageStore interface {
	InsertUsage(ctx context.Context, events []UsageEvent) error
	QueryUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error)
	Close()
}

// WithUsage returns a Store that keeps usage events in u and everything else
// (tenants, keys, prices, insights) in s.
func WithUsage(s Store, u UsageStore) Store { return &splitStore{Store: s, usage: u} }

type splitStore struct {
	Store
	usage UsageStore
}

func (s *splitStore) InsertUsage(ctx context.Context, events []UsageEvent) error {
	return s.usage.InsertUsage(ctx, events)
}

func (s *splitStore) QueryUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error) {
	return s.usage.QueryUsage(ctx, q)
}

func (s *splitStore) Close() {
	s.usage.Close()
	s.Store.Close()
}

// ClickHouse keeps usage events in ClickHouse, through its HTTP interface.
//
// Raw events (usage_events) expire after a retention period. Every insert
// also adds to an hourly rollup (usage_hourly) that is kept forever, and
// queries read whole hours from the rollup and only the partial hours at the
// edges of the range from the raw events.
type ClickHouse struct {
	endpoint string // scheme://host:port/
	db       string
	user     string
	password string
	client   *http.Client
}

const clickhouseSchema = `
CREATE TABLE IF NOT EXISTS usage_events (
    ts                     DateTime64(3, 'UTC'),
    request_id             String,
    tenant_id              LowCardinality(String),
    app_id                 LowCardinality(String),
    key_id                 LowCardinality(String),
    user_email             String,
    user_source            LowCardinality(String),
    model                  LowCardinality(String),
    provider               LowCardinality(String),
    upstream_model         LowCardinality(String),
    stream                 Bool,
    status                 LowCardinality(String),
    error_code             LowCardinality(String),
    http_status            Int32,
    input_tokens           Int32,
    cached_input_tokens    Int32,
    cache_write_tokens     Int32,
    output_tokens          Int32,
    reasoning_tokens       Int32,
    usage_reported         Bool,
    latency_ms             Int32,
    ttft_ms                Nullable(Int32),
    prompt_cache_key       String,
    cost_usd               Float64,
    cache_status           LowCardinality(String),
    expected_cached_tokens Int32,
    auth_method            LowCardinality(String),
    subject                String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (toDate(ts), tenant_id, app_id, ts)`

// Columns added after the table was first created.
const clickhouseAddColumns = `
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS auth_method LowCardinality(String),
    ADD COLUMN IF NOT EXISTS subject String`

// The hourly rollup has one row per hour and combination of the dimensions
// usage can be grouped by. Background merges add rows with the same key
// together, but not right away, so queries still sum.
const clickhouseRollupSchema = `
CREATE TABLE IF NOT EXISTS usage_hourly (
    hour                DateTime('UTC'),
    tenant_id           LowCardinality(String),
    app_id              LowCardinality(String),
    user_email          String,
    model               LowCardinality(String),
    provider            LowCardinality(String),
    cache_status        LowCardinality(String),
    requests            UInt64,
    failed_requests     UInt64,
    input_tokens        Int64,
    cached_input_tokens Int64,
    cache_write_tokens  Int64,
    output_tokens       Int64,
    reasoning_tokens    Int64,
    cost_usd            Float64
) ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (hour, tenant_id, app_id, user_email, model, provider, cache_status)`

// The materialized view fills the rollup on every insert into usage_events.
const clickhouseRollupView = `
CREATE MATERIALIZED VIEW IF NOT EXISTS usage_hourly_mv TO usage_hourly AS
SELECT
    toStartOfHour(ts) AS hour, tenant_id, app_id, user_email, model, provider, cache_status,
    count() AS requests,
    countIf(status = 'failed') AS failed_requests,
    sum(toInt64(input_tokens)) AS input_tokens,
    sum(toInt64(cached_input_tokens)) AS cached_input_tokens,
    sum(toInt64(cache_write_tokens)) AS cache_write_tokens,
    sum(toInt64(output_tokens)) AS output_tokens,
    sum(toInt64(reasoning_tokens)) AS reasoning_tokens,
    sum(cost_usd) AS cost_usd
FROM usage_events
GROUP BY hour, tenant_id, app_id, user_email, model, provider, cache_status`

// NewClickHouse connects to a URL like http://user:password@host:8123/database,
// creates the database and tables if they are missing, and sets raw events to
// expire after retentionDays (0 keeps them forever).
func NewClickHouse(ctx context.Context, rawURL string, retentionDays int) (*ClickHouse, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("clickhouse_url must look like http://user:password@host:8123/database")
	}
	c := &ClickHouse{
		endpoint: u.Scheme + "://" + u.Host + "/",
		db:       strings.Trim(u.Path, "/"),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	if c.db == "" {
		c.db = "default"
	}
	if u.User != nil {
		c.user = u.User.Username()
		c.password, _ = u.User.Password()
	}
	if err := c.exec(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdent(c.db), nil, ""); err != nil {
		return nil, fmt.Errorf("connect to clickhouse: %w", err)
	}
	for _, stmt := range []string{clickhouseSchema, clickhouseAddColumns, clickhouseRollupSchema, clickhouseRollupView} {
		if err := c.exec(ctx, stmt, nil, c.db); err != nil {
			return nil, fmt.Errorf("apply clickhouse schema: %w", err)
		}
	}
	if err := c.setRetention(ctx, retentionDays); err != nil {
		return nil, fmt.Errorf("set clickhouse retention: %w", err)
	}
	return c, nil
}

// setRetention changes the raw events' TTL only when it differs from the
// configured one, because changing it rewrites the table's existing data.
func (c *ClickHouse) setRetention(ctx context.Context, days int) error {
	if days < 0 {
		return fmt.Errorf("retention must be 0 (forever) or a number of days")
	}
	rc, err := c.do(ctx, "SELECT create_table_query FROM system.tables WHERE database = {db:String} AND name = 'usage_events'",
		url.Values{"param_db": {c.db}}, "", nil)
	if err != nil {
		return err
	}
	current, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return err
	}
	switch {
	case days == 0 && strings.Contains(string(current), " TTL "):
		return c.exec(ctx, "ALTER TABLE usage_events REMOVE TTL", nil, c.db)
	case days > 0 && !strings.Contains(string(current), fmt.Sprintf(" TTL toDateTime(ts) + toIntervalDay(%d) ", days)):
		return c.exec(ctx, fmt.Sprintf("ALTER TABLE usage_events MODIFY TTL toDateTime(ts) + INTERVAL %d DAY", days), nil, c.db)
	}
	return nil
}

func (c *ClickHouse) Close() { c.client.CloseIdleConnections() }

// do sends one statement. Query parameters ({name:Type} in the statement)
// and settings go in the URL; body, when set, is the statement's data.
func (c *ClickHouse) do(ctx context.Context, statement string, params url.Values, db string, body io.Reader) (io.ReadCloser, error) {
	if params == nil {
		params = url.Values{}
	}
	if db != "" {
		params.Set("database", db)
	}
	if body == nil {
		body = strings.NewReader(statement)
	} else {
		params.Set("query", statement)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"?"+params.Encode(), body)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp.Body, nil
}

func (c *ClickHouse) exec(ctx context.Context, statement string, params url.Values, db string) error {
	rc, err := c.do(ctx, statement, params, db, nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, rc)
	return rc.Close()
}

// chUsageEvent is a UsageEvent as a JSONEachRow line.
type chUsageEvent struct {
	TS                   string  `json:"ts"`
	RequestID            string  `json:"request_id"`
	TenantID             string  `json:"tenant_id"`
	AppID                string  `json:"app_id"`
	KeyID                string  `json:"key_id"`
	UserEmail            string  `json:"user_email"`
	UserSource           string  `json:"user_source"`
	Model                string  `json:"model"`
	Provider             string  `json:"provider"`
	UpstreamModel        string  `json:"upstream_model"`
	Stream               bool    `json:"stream"`
	Status               string  `json:"status"`
	ErrorCode            string  `json:"error_code"`
	HTTPStatus           int     `json:"http_status"`
	InputTokens          int     `json:"input_tokens"`
	CachedInputTokens    int     `json:"cached_input_tokens"`
	CacheWriteTokens     int     `json:"cache_write_tokens"`
	OutputTokens         int     `json:"output_tokens"`
	ReasoningTokens      int     `json:"reasoning_tokens"`
	UsageReported        bool    `json:"usage_reported"`
	LatencyMS            int     `json:"latency_ms"`
	TTFTMS               *int    `json:"ttft_ms"`
	PromptCacheKey       string  `json:"prompt_cache_key"`
	CostUSD              float64 `json:"cost_usd"`
	CacheStatus          string  `json:"cache_status"`
	ExpectedCachedTokens int     `json:"expected_cached_tokens"`
	AuthMethod           string  `json:"auth_method"`
	Subject              string  `json:"subject"`
}

func (c *ClickHouse) InsertUsage(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range events {
		e := &events[i]
		if err := enc.Encode(chUsageEvent{
			TS: e.TS.UTC().Format("2006-01-02 15:04:05.000"), RequestID: e.RequestID, TenantID: e.TenantID, AppID: e.AppID,
			KeyID: e.KeyID, UserEmail: e.UserEmail, UserSource: e.UserSource, Model: e.Model, Provider: e.Provider,
			UpstreamModel: e.UpstreamModel, Stream: e.Stream, Status: e.Status, ErrorCode: e.ErrorCode, HTTPStatus: e.HTTPStatus,
			InputTokens: e.InputTokens, CachedInputTokens: e.CachedInputTokens, CacheWriteTokens: e.CacheWriteTokens,
			OutputTokens: e.OutputTokens, ReasoningTokens: e.ReasoningTokens, UsageReported: e.UsageReported,
			LatencyMS: e.LatencyMS, TTFTMS: e.TTFTMS, PromptCacheKey: e.PromptCacheKey, CostUSD: e.CostUSD,
			CacheStatus: e.CacheStatus, ExpectedCachedTokens: e.ExpectedCachedTokens, AuthMethod: e.AuthMethod, Subject: e.Subject,
		}); err != nil {
			return err
		}
	}
	// Several proxies each flushing small batches would make many tiny parts;
	// async inserts let ClickHouse merge them before writing, and waiting for
	// them keeps errors visible to the meter.
	params := url.Values{"async_insert": {"1"}, "wait_for_async_insert": {"1"}}
	rc, err := c.do(ctx, "INSERT INTO usage_events FORMAT JSONEachRow", params, c.db, &buf)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, rc)
	return rc.Close()
}

func (c *ClickHouse) QueryUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error) {
	const format = "2006-01-02 15:04:05.000"
	// Whole hours in [from, to) come from the rollup, the partial hours at
	// either end from the raw events. Once raw events expire, the edges of an
	// old range are simply missing, which is at most an hour on each side.
	firstHour := q.From.UTC().Truncate(time.Hour)
	if firstHour.Before(q.From) {
		firstHour = firstHour.Add(time.Hour)
	}
	lastHour := q.To.UTC().Truncate(time.Hour)
	if !firstHour.Before(lastHour) {
		firstHour, lastHour = q.To, q.To // no whole hour: all raw
	}
	params := url.Values{
		"param_from":  {q.From.UTC().Format(format)},
		"param_to":    {q.To.UTC().Format(format)},
		"param_first": {firstHour.UTC().Format(format)},
		"param_last":  {lastHour.UTC().Format(format)},
		// Return counts and sums as JSON numbers, not strings.
		"output_format_json_quote_64bit_integers": {"0"},
	}
	var filters []string
	i := 0
	for k, v := range q.Filters {
		name := fmt.Sprintf("f%d", i)
		i++
		params.Set("param_"+name, v)
		filters = append(filters, fmt.Sprintf(" AND %s = {%s:String}", UsageDimensions[k], name))
	}
	filter := strings.Join(filters, "")
	dims := "tenant_id, app_id, user_email, model, provider, cache_status"
	source := `SELECT toDateTime64(hour, 3, 'UTC') AS t, ` + dims + `, requests, failed_requests, input_tokens,
		cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd
	FROM usage_hourly
	WHERE hour >= {first:DateTime64(3, 'UTC')} AND hour < {last:DateTime64(3, 'UTC')}` + filter + `
	UNION ALL
	SELECT ts AS t, ` + dims + `, toUInt64(1), toUInt64(status = 'failed'), toInt64(input_tokens),
		toInt64(cached_input_tokens), toInt64(cache_write_tokens), toInt64(output_tokens), toInt64(reasoning_tokens), cost_usd
	FROM usage_events
	WHERE ((ts >= {from:DateTime64(3, 'UTC')} AND ts < {first:DateTime64(3, 'UTC')})
	    OR (ts >= {last:DateTime64(3, 'UTC')} AND ts < {to:DateTime64(3, 'UTC')}))` + filter

	var sel, group []string
	switch q.Granularity {
	case "hour":
		sel = append(sel, "toUnixTimestamp(toStartOfHour(t))")
	case "day":
		sel = append(sel, "toUnixTimestamp(toStartOfDay(t))")
	}
	if q.Granularity != "" {
		group = append(group, "1")
	}
	for _, g := range q.GroupBy {
		sel = append(sel, "toString("+UsageDimensions[g]+")")
		group = append(group, fmt.Sprint(len(sel)))
	}
	sel = append(sel,
		"sum(requests)", "sum(failed_requests)", "sum(input_tokens)", "sum(cached_input_tokens)",
		"sum(cache_write_tokens)", "sum(output_tokens)", "sum(reasoning_tokens)", "sum(cost_usd)")
	sql := "SELECT " + strings.Join(sel, ", ") + " FROM (" + source + ")"
	if len(group) > 0 {
		sql += " GROUP BY " + strings.Join(group, ", ")
	}
	sql += " LIMIT 10000 FORMAT JSONCompactEachRow"

	rc, err := c.do(ctx, sql, params, c.db, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	dec := json.NewDecoder(rc)
	var out []UsageRow
	for dec.More() {
		var (
			r      = UsageRow{Group: map[string]string{}}
			bucket int64
			dims   = make([]string, len(q.GroupBy))
			dest   []any
		)
		if q.Granularity != "" {
			dest = append(dest, &bucket)
		}
		for i := range dims {
			dest = append(dest, &dims[i])
		}
		dest = append(dest, &r.Requests, &r.FailedRequests, &r.InputTokens, &r.CachedInputTokens,
			&r.CacheWriteTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CostUSD)
		if err := dec.Decode(&dest); err != nil {
			return nil, fmt.Errorf("clickhouse: reading usage rows: %w", err)
		}
		if q.Granularity != "" {
			b := time.Unix(bucket, 0).UTC()
			r.Bucket = &b
		}
		for i, g := range q.GroupBy {
			r.Group[g] = dims[i]
		}
		r.TotalTokens = r.InputTokens + r.OutputTokens
		out = append(out, r)
	}
	sortRows(out)
	return out, nil
}

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
