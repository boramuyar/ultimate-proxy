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
    expected_cached_tokens Int32
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (toDate(ts), tenant_id, app_id, ts)`

// NewClickHouse connects to a URL like http://user:password@host:8123/database
// and creates the database and table if they are missing.
func NewClickHouse(ctx context.Context, rawURL string) (*ClickHouse, error) {
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
	if err := c.exec(ctx, clickhouseSchema, nil, c.db); err != nil {
		return nil, fmt.Errorf("apply clickhouse schema: %w", err)
	}
	return c, nil
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
			CacheStatus: e.CacheStatus, ExpectedCachedTokens: e.ExpectedCachedTokens,
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
	var (
		sel, group []string
		params     = url.Values{
			"param_from": {q.From.UTC().Format("2006-01-02 15:04:05.000")},
			"param_to":   {q.To.UTC().Format("2006-01-02 15:04:05.000")},
			// Return counts and sums as JSON numbers, not strings.
			"output_format_json_quote_64bit_integers": {"0"},
		}
		where = []string{"ts >= {from:DateTime64(3, 'UTC')}", "ts < {to:DateTime64(3, 'UTC')}"}
	)
	switch q.Granularity {
	case "hour":
		sel = append(sel, "toUnixTimestamp(toStartOfHour(ts))")
	case "day":
		sel = append(sel, "toUnixTimestamp(toStartOfDay(ts))")
	}
	if q.Granularity != "" {
		group = append(group, "1")
	}
	for _, g := range q.GroupBy {
		sel = append(sel, "toString("+UsageDimensions[g]+")")
		group = append(group, fmt.Sprint(len(sel)))
	}
	i := 0
	for k, v := range q.Filters {
		name := fmt.Sprintf("f%d", i)
		i++
		params.Set("param_"+name, v)
		where = append(where, fmt.Sprintf("%s = {%s:String}", UsageDimensions[k], name))
	}
	sel = append(sel,
		"count()", "countIf(status = 'failed')",
		"sum(toInt64(input_tokens))", "sum(toInt64(cached_input_tokens))",
		"sum(toInt64(cache_write_tokens))", "sum(toInt64(output_tokens))",
		"sum(toInt64(reasoning_tokens))", "sum(cost_usd)")
	sql := "SELECT " + strings.Join(sel, ", ") + " FROM usage_events WHERE " + strings.Join(where, " AND ")
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
