package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
)

//go:embed schema.sql
var schema string

// Postgres is the production store.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres connects and applies the schema.
func NewPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) EnsureTenant(ctx context.Context, name string) (*Tenant, error) {
	var t Tenant
	err := p.pool.QueryRow(ctx, `
		INSERT INTO tenants (id, name) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, name, created_at`, openresponses.NewID("ten"), name).Scan(&t.ID, &t.Name, &t.CreatedAt)
	return &t, err
}

func (p *Postgres) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, name, created_at FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		err := r.Scan(&t.ID, &t.Name, &t.CreatedAt)
		return t, err
	})
}

func (p *Postgres) EnsureApplication(ctx context.Context, tenantID, name string, canAssert bool) (*Application, error) {
	var a Application
	err := p.pool.QueryRow(ctx, `
		INSERT INTO applications (id, tenant_id, name, can_assert_users) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, tenant_id, name, can_assert_users, created_at`,
		openresponses.NewID("app"), tenantID, name, canAssert).Scan(&a.ID, &a.TenantID, &a.Name, &a.CanAssertUsers, &a.CreatedAt)
	if isForeignKeyViolation(err) {
		return nil, ErrNotFound
	}
	return &a, err
}

func (p *Postgres) ListApplications(ctx context.Context, tenantID string) ([]Application, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, name, can_assert_users, created_at FROM applications
		WHERE $1 = '' OR tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Application, error) {
		var a Application
		err := r.Scan(&a.ID, &a.TenantID, &a.Name, &a.CanAssertUsers, &a.CreatedAt)
		return a, err
	})
}

func (p *Postgres) CreateKey(ctx context.Context, appID, hash, prefix string) (*APIKey, error) {
	var k APIKey
	err := p.pool.QueryRow(ctx, `
		INSERT INTO api_keys (id, app_id, key_hash, prefix) VALUES ($1, $2, $3, $4)
		ON CONFLICT (key_hash) DO UPDATE SET prefix = api_keys.prefix
		RETURNING `+keyColumns,
		openresponses.NewID("key"), appID, hash, prefix).Scan(k.scanTargets()...)
	if isForeignKeyViolation(err) {
		return nil, ErrNotFound
	}
	return &k, err
}

func (p *Postgres) RevokeKey(ctx context.Context, keyID string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ListKeys(ctx context.Context, appID string) ([]APIKey, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+keyColumns+` FROM api_keys WHERE app_id = $1 ORDER BY created_at`, appID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		err := r.Scan(k.scanTargets()...)
		return k, err
	})
}

const keyColumns = "id, app_id, prefix, created_at, revoked_at, expires_at, allowed_models"

func (k *APIKey) scanTargets() []any {
	return []any{&k.ID, &k.AppID, &k.Prefix, &k.CreatedAt, &k.RevokedAt, &k.ExpiresAt, &k.AllowedModels}
}

func (p *Postgres) GetKey(ctx context.Context, keyID string) (*APIKey, error) {
	var k APIKey
	err := p.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = $1`, keyID).Scan(k.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &k, err
}

func (p *Postgres) SetKeyPolicy(ctx context.Context, keyID string, expiresAt *time.Time, allowedModels []string) (*APIKey, error) {
	var k APIKey
	err := p.pool.QueryRow(ctx, `
		UPDATE api_keys SET expires_at = $2, allowed_models = $3 WHERE id = $1 RETURNING `+keyColumns,
		keyID, expiresAt, allowedModels).Scan(k.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &k, err
}

func (p *Postgres) LookupKey(ctx context.Context, hash string) (*Principal, error) {
	var pr Principal
	err := p.pool.QueryRow(ctx, `
		SELECT k.id, t.id, t.name, a.id, a.name, a.can_assert_users, k.expires_at, k.allowed_models
		FROM api_keys k JOIN applications a ON a.id = k.app_id JOIN tenants t ON t.id = a.tenant_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL`, hash).
		Scan(&pr.KeyID, &pr.TenantID, &pr.TenantName, &pr.AppID, &pr.AppName, &pr.CanAssertUsers, &pr.ExpiresAt, &pr.AllowedModels)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &pr, err
}

func (p *Postgres) CreateProxyToken(ctx context.Context, t *ProxyToken, hash string) error {
	if _, err := p.pool.Exec(ctx, `DELETE FROM proxy_tokens WHERE expires_at < now() - interval '1 day'`); err != nil {
		return err
	}
	t.ID = openresponses.NewID("tok")
	err := p.pool.QueryRow(ctx, `
		INSERT INTO proxy_tokens (id, token_hash, app_id, user_email, allowed_models, minted_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		t.ID, hash, t.AppID, t.UserEmail, t.AllowedModels, t.MintedBy, t.ExpiresAt).Scan(&t.CreatedAt)
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) LookupProxyToken(ctx context.Context, hash string) (*ProxyToken, error) {
	var t ProxyToken
	err := p.pool.QueryRow(ctx, `
		SELECT pt.id, t.id, t.name, a.id, a.name, pt.user_email, pt.allowed_models, pt.minted_by, pt.created_at, pt.expires_at
		FROM proxy_tokens pt JOIN applications a ON a.id = pt.app_id JOIN tenants t ON t.id = a.tenant_id
		WHERE pt.token_hash = $1 AND pt.revoked_at IS NULL AND pt.expires_at > now()`, hash).
		Scan(&t.ID, &t.TenantID, &t.TenantName, &t.AppID, &t.AppName, &t.UserEmail, &t.AllowedModels, &t.MintedBy, &t.CreatedAt, &t.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func (p *Postgres) RevokeProxyToken(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE proxy_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

var usageColumns = []string{
	"ts", "request_id", "tenant_id", "app_id", "key_id", "user_email", "user_source", "model", "provider",
	"upstream_model", "stream", "status", "error_code", "http_status", "input_tokens", "cached_input_tokens",
	"cache_write_tokens", "output_tokens", "reasoning_tokens", "usage_reported", "latency_ms", "ttft_ms",
	"prompt_cache_key", "cost_usd", "cache_status", "expected_cached_tokens", "auth_method", "subject", "tags",
}

func (p *Postgres) InsertUsage(ctx context.Context, events []UsageEvent) error {
	_, err := p.pool.CopyFrom(ctx, pgx.Identifier{"usage_events"}, usageColumns,
		pgx.CopyFromSlice(len(events), func(i int) ([]any, error) {
			e := &events[i]
			return []any{
				e.TS, e.RequestID, e.TenantID, e.AppID, e.KeyID, e.UserEmail, e.UserSource, e.Model, e.Provider,
				e.UpstreamModel, e.Stream, e.Status, e.ErrorCode, e.HTTPStatus, e.InputTokens, e.CachedInputTokens,
				e.CacheWriteTokens, e.OutputTokens, e.ReasoningTokens, e.UsageReported, e.LatencyMS, e.TTFTMS,
				e.PromptCacheKey, e.CostUSD, e.CacheStatus, e.ExpectedCachedTokens, e.AuthMethod, e.Subject, pgTags(e.Tags),
			}, nil
		}))
	return err
}

func (p *Postgres) QueryUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error) {
	var (
		sel, group []string
		args       = []any{q.From, q.To}
		where      = []string{"ts >= $1", "ts < $2"}
	)
	if q.Granularity != "" {
		sel = append(sel, fmt.Sprintf("date_trunc('%s', ts AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'", q.Granularity))
		group = append(group, "1")
	}
	for _, g := range q.GroupBy {
		sel = append(sel, pgDimension(g))
		group = append(group, fmt.Sprint(len(sel)))
	}
	for k, v := range q.Filters {
		args = append(args, v)
		where = append(where, fmt.Sprintf("%s = $%d", pgDimension(k), len(args)))
	}
	if q.TenantIn != nil {
		args = append(args, q.TenantIn)
		where = append(where, fmt.Sprintf("tenant_id = ANY($%d)", len(args)))
	}
	sel = append(sel,
		"count(*)", "count(*) FILTER (WHERE status = 'failed')",
		"coalesce(sum(input_tokens), 0)", "coalesce(sum(cached_input_tokens), 0)",
		"coalesce(sum(cache_write_tokens), 0)", "coalesce(sum(output_tokens), 0)",
		"coalesce(sum(reasoning_tokens), 0)", "coalesce(sum(cost_usd), 0)")
	sql := "SELECT " + strings.Join(sel, ", ") + " FROM usage_events WHERE " + strings.Join(where, " AND ")
	if len(group) > 0 {
		sql += " GROUP BY " + strings.Join(group, ", ")
	}
	sql += " LIMIT 10000"

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var (
			r      = UsageRow{Group: map[string]string{}}
			bucket time.Time
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
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if q.Granularity != "" {
			b := bucket.UTC()
			r.Bucket = &b
		}
		for i, g := range q.GroupBy {
			r.Group[g] = dims[i]
		}
		r.TotalTokens = r.InputTokens + r.OutputTokens
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortRows(out)
	return out, nil
}

// pgDimension is the column or expression for a validated dimension. Tag keys
// are limited to [a-z0-9_.-], so they are safe to quote inline.
func pgDimension(d string) string {
	if k, ok := TagDimension(d); ok {
		return "coalesce(tags->>'" + k + "', '')"
	}
	return UsageDimensions[d]
}

func pgTags(tags map[string]string) map[string]string {
	if tags == nil {
		return map[string]string{}
	}
	return tags
}

func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23503"
}

func (p *Postgres) SaveInsight(ctx context.Context, in *Insight) error {
	evidence, err := json.Marshal(in.Evidence)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO insights (id, kind, severity, status, tenant_id, app_id, model, title, detail, evidence, first_seen, last_seen, resolved_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (id) DO UPDATE SET severity = EXCLUDED.severity, status = EXCLUDED.status, title = EXCLUDED.title,
			detail = EXCLUDED.detail, evidence = EXCLUDED.evidence, last_seen = EXCLUDED.last_seen, resolved_at = EXCLUDED.resolved_at`,
		in.ID, in.Kind, in.Severity, in.Status, in.TenantID, in.AppID, in.Model, in.Title, in.Detail, evidence,
		in.FirstSeen, in.LastSeen, in.ResolvedAt)
	return err
}

func (p *Postgres) ListInsights(ctx context.Context, status string) ([]Insight, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, kind, severity, status, tenant_id, app_id, model, title, detail, evidence, first_seen, last_seen, resolved_at
		FROM insights WHERE $1 = '' OR status = $1 ORDER BY last_seen DESC LIMIT 1000`, status)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Insight, error) {
		var in Insight
		var evidence []byte
		err := r.Scan(&in.ID, &in.Kind, &in.Severity, &in.Status, &in.TenantID, &in.AppID, &in.Model, &in.Title,
			&in.Detail, &evidence, &in.FirstSeen, &in.LastSeen, &in.ResolvedAt)
		if err == nil {
			err = json.Unmarshal(evidence, &in.Evidence)
		}
		return in, err
	})
}

func (p *Postgres) AddAudit(ctx context.Context, e *AuditEntry) error {
	var req any
	if len(e.Request) > 0 {
		req = string(e.Request)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO audit_log (id, ts, actor_method, actor_email, actor_name, actor_role, action, method, path, target_id, status, request)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb)`,
		e.ID, e.TS, e.ActorMethod, e.ActorEmail, e.ActorName, e.ActorRole, e.Action, e.Method, e.Path, e.TargetID, e.Status, req)
	return err
}

func (p *Postgres) ListAudit(ctx context.Context, before time.Time, limit int) ([]AuditEntry, error) {
	if before.IsZero() {
		before = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, ts, actor_method, actor_email, actor_name, actor_role, action, method, path, target_id, status, request
		FROM audit_log WHERE ts < $1 ORDER BY ts DESC, id DESC LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		var req []byte
		err := r.Scan(&e.ID, &e.TS, &e.ActorMethod, &e.ActorEmail, &e.ActorName, &e.ActorRole, &e.Action, &e.Method,
			&e.Path, &e.TargetID, &e.Status, &req)
		if len(req) > 0 {
			e.Request = req
		}
		return e, err
	})
}

func (p *Postgres) AddPrice(ctx context.Context, pr *Price) error {
	return p.pool.QueryRow(ctx, `
		INSERT INTO model_prices (id, model, input, cached_input, cache_write, output, effective_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		pr.ID, pr.Model, pr.Input, pr.CachedInput, pr.CacheWrite, pr.Output, pr.EffectiveFrom).Scan(&pr.CreatedAt)
}

func (p *Postgres) ListPrices(ctx context.Context) ([]Price, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, model, input, cached_input, cache_write, output, effective_from, created_at
		FROM model_prices ORDER BY effective_from, created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Price, error) {
		var pr Price
		err := r.Scan(&pr.ID, &pr.Model, &pr.Input, &pr.CachedInput, &pr.CacheWrite, &pr.Output, &pr.EffectiveFrom, &pr.CreatedAt)
		return pr, err
	})
}

const limitColumns = "id, tenant_id, app_id, user_email, kind, amount, period, enforcement, created_at"

func scanLimit(r pgx.Row) (Limit, error) {
	var l Limit
	err := r.Scan(&l.ID, &l.TenantID, &l.AppID, &l.User, &l.Kind, &l.Amount, &l.Period, &l.Enforcement, &l.CreatedAt)
	return l, err
}

func (p *Postgres) CreateLimit(ctx context.Context, l *Limit) error {
	l.ID = openresponses.NewID("lim")
	return p.pool.QueryRow(ctx, `
		INSERT INTO limits (id, tenant_id, app_id, user_email, kind, amount, period, enforcement)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
		l.ID, l.TenantID, l.AppID, l.User, l.Kind, l.Amount, l.Period, l.Enforcement).Scan(&l.CreatedAt)
}

func (p *Postgres) UpdateLimit(ctx context.Context, l *Limit) error {
	tag, err := p.pool.Exec(ctx, `UPDATE limits SET amount = $2, period = $3, enforcement = $4 WHERE id = $1`,
		l.ID, l.Amount, l.Period, l.Enforcement)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) GetLimit(ctx context.Context, id string) (*Limit, error) {
	l, err := scanLimit(p.pool.QueryRow(ctx, `SELECT `+limitColumns+` FROM limits WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &l, err
}

func (p *Postgres) ListLimits(ctx context.Context) ([]Limit, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+limitColumns+` FROM limits ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Limit, error) { return scanLimit(r) })
}

func (p *Postgres) DeleteLimit(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM limits WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
