// Package store persists tenants, applications, API keys and usage events.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Application struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	// CanAssertUsers lets the application name its end user in a header or in
	// metadata. Turn it off for keys that ship to untrusted clients.
	CanAssertUsers bool      `json:"can_assert_users"`
	CreatedAt      time.Time `json:"created_at"`
}

type APIKey struct {
	ID        string     `json:"id"`
	AppID     string     `json:"application_id"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// ExpiresAt is when the key stops working; nil never.
	ExpiresAt *time.Time `json:"expires_at"`
	// AllowedModels limits the models the key may use; nil allows all.
	AllowedModels []string `json:"allowed_models"`
}

// Principal is who a request is billed to, resolved from its API key.
type Principal struct {
	KeyID          string
	TenantID       string
	TenantName     string
	AppID          string
	AppName        string
	CanAssertUsers bool
	ExpiresAt      *time.Time
	AllowedModels  []string
}

// ProxyToken is a short-lived token the proxy minted for a client-side
// agent, narrowed from its minter's identity. Only its hash is stored.
type ProxyToken struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id"`
	TenantName string `json:"-"`
	AppID      string `json:"application_id"`
	AppName    string `json:"-"`
	UserEmail  string `json:"user,omitempty"`
	// AllowedModels is nil when the token may use every model its
	// application may.
	AllowedModels []string   `json:"models"`
	MintedBy      string     `json:"minted_by"` // method:subject of the minter
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// UsageEvent is one metered request.
type UsageEvent struct {
	TS                time.Time
	RequestID         string
	TenantID          string
	AppID             string
	KeyID             string
	AuthMethod        string // api_key, jwt or proxy_token
	Subject           string // the key ID, the token's sub claim, or the minted token ID
	UserEmail         string
	UserSource        string // jwt, header, metadata, safety_identifier or none
	Model             string
	Provider          string
	UpstreamModel     string
	Stream            bool
	Status            string // completed, incomplete, failed
	ErrorCode         string
	HTTPStatus        int
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	ReasoningTokens   int
	UsageReported     bool
	LatencyMS         int
	TTFTMS            *int
	PromptCacheKey    string
	CostUSD           float64
	// CacheStatus says whether the prompt cache was hit and, if not, why
	// (see the insights package). ExpectedCachedTokens is how many tokens
	// should have been cached, when the same prefix was sent recently.
	CacheStatus          string
	ExpectedCachedTokens int
	// MissedCostUSD is what a cache miss cost over a hit: the expected
	// cached tokens at the input price instead of the cached input price.
	MissedCostUSD float64
	// Tags are the caller's own labels from the X-Proxy-Tags header, such
	// as feature=search. See ParseTags.
	Tags map[string]string
}

// Dimensions usage can be grouped and filtered by, besides "tag:<key>".
var UsageDimensions = map[string]string{
	"tenant":      "tenant_id",
	"application": "app_id",
	"email":       "user_email",
	"model":       "model",
	"provider":    "provider",
	"cache":       "cache_status",
}

type UsageQuery struct {
	From, To    time.Time
	GroupBy     []string          // keys of UsageDimensions, or "tag:<key>"
	Granularity string            // "", "hour" or "day"
	Filters     map[string]string // dimension -> value
	// TenantIn, when not nil, keeps only these tenants' usage.
	TenantIn []string
}

// TagDimension returns the tag key of a "tag:<key>" dimension.
func TagDimension(d string) (key string, ok bool) {
	key, ok = strings.CutPrefix(d, "tag:")
	return key, ok && validTagKey(key)
}

func validDimension(d string) bool {
	_, ok := UsageDimensions[d]
	if !ok {
		_, ok = TagDimension(d)
	}
	return ok
}

func (q *UsageQuery) Validate() error {
	for _, g := range q.GroupBy {
		if !validDimension(g) {
			return fmt.Errorf("unknown group_by %q", g)
		}
	}
	for f := range q.Filters {
		if !validDimension(f) {
			return fmt.Errorf("unknown filter %q", f)
		}
	}
	switch q.Granularity {
	case "", "hour", "day":
	default:
		return fmt.Errorf("granularity must be hour or day")
	}
	if !q.To.After(q.From) {
		return fmt.Errorf("to must be after from")
	}
	return nil
}

type UsageRow struct {
	Bucket            *time.Time        `json:"bucket,omitempty"`
	Group             map[string]string `json:"group"`
	Requests          int64             `json:"requests"`
	FailedRequests    int64             `json:"failed_requests"`
	InputTokens       int64             `json:"input_tokens"`
	CachedInputTokens int64             `json:"cached_input_tokens"`
	CacheWriteTokens  int64             `json:"cache_write_tokens"`
	OutputTokens      int64             `json:"output_tokens"`
	ReasoningTokens   int64             `json:"reasoning_tokens"`
	TotalTokens       int64             `json:"total_tokens"`
	CostUSD           float64           `json:"cost_usd"`
	MissedCostUSD     float64           `json:"missed_cost_usd"`
}

// Price is what a model costs, in USD per million tokens, from EffectiveFrom
// until a later price for the same model takes over. Prices are never edited
// in place, so past costs stay explainable. Model matches a client-facing
// alias, an upstream model name, or "<provider>/<upstream model>".
type Price struct {
	ID            string    `json:"id"`
	Model         string    `json:"model"`
	Input         float64   `json:"input"`
	CachedInput   float64   `json:"cached_input"`
	CacheWrite    float64   `json:"cache_write"`
	Output        float64   `json:"output"`
	EffectiveFrom time.Time `json:"effective_from"`
	CreatedAt     time.Time `json:"created_at"`
}

// Insight is a problem the proxy noticed in live traffic, such as an
// application whose prompt cache keeps missing.
type Insight struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Severity   string         `json:"severity"` // warning or critical
	Status     string         `json:"status"`   // open or resolved
	TenantID   string         `json:"tenant_id"`
	AppID      string         `json:"application_id"`
	Model      string         `json:"model"`
	Title      string         `json:"title"`
	Detail     string         `json:"detail"`
	Evidence   map[string]any `json:"evidence"`
	FirstSeen  time.Time      `json:"first_seen"`
	LastSeen   time.Time      `json:"last_seen"`
	ResolvedAt *time.Time     `json:"resolved_at,omitempty"`
}

// Limit kinds.
const (
	LimitRPM = "rpm" // requests per sliding minute
	LimitTPM = "tpm" // input + output tokens per sliding minute
	// Budgets count over a calendar day, week (from Monday) or month, in UTC.
	LimitBudgetUSD    = "budget_usd"    // spend, from the prices table
	LimitBudgetTokens = "budget_tokens" // input + output tokens
)

// IsBudget reports whether the limit counts over a period rather than a
// sliding minute.
func (l *Limit) IsBudget() bool { return l.Kind == LimitBudgetUSD || l.Kind == LimitBudgetTokens }

// Limit is a rate limit or budget rule. It applies to a tenant, to one of its
// applications (AppID), or to end users (User: an email, or "*" for each
// user separately), within the tenant or one application.
type Limit struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	AppID       string    `json:"application_id"`
	User        string    `json:"user"`
	Kind        string    `json:"kind"`
	Amount      float64   `json:"amount"`
	Period      string    `json:"period"`      // day, week or month for budgets; "" for per-minute limits
	Enforcement string    `json:"enforcement"` // hard blocks; soft only warns
	CreatedAt   time.Time `json:"created_at"`
}

// Scope is what the limit counts: tenant, application or user.
func (l *Limit) Scope() string {
	switch {
	case l.User != "":
		return "user"
	case l.AppID != "":
		return "application"
	}
	return "tenant"
}

// AuditEntry records one change made through the admin API, or a sign-in to
// it: who, what, on which object, and with what result.
type AuditEntry struct {
	ID          string    `json:"id"`
	TS          time.Time `json:"ts"`
	ActorMethod string    `json:"actor_method"` // oidc or token
	ActorEmail  string    `json:"actor_email,omitempty"`
	ActorName   string    `json:"actor_name,omitempty"`
	ActorRole   string    `json:"actor_role,omitempty"`
	// Action names what was done, such as key.create or sign_in.
	Action   string `json:"action"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	TargetID string `json:"target_id,omitempty"`
	Status   int    `json:"status"`
	// Request is the JSON body that was sent, if any.
	Request json.RawMessage `json:"request,omitempty"`
}

type Store interface {
	EnsureTenant(ctx context.Context, name string) (*Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	EnsureApplication(ctx context.Context, tenantID, name string, canAssertUsers bool) (*Application, error)
	ListApplications(ctx context.Context, tenantID string) ([]Application, error)
	// CreateKey stores a key by hash. It is idempotent for the same hash.
	CreateKey(ctx context.Context, appID, hash, prefix string) (*APIKey, error)
	RevokeKey(ctx context.Context, keyID string) error
	// ListKeys returns an application's keys, revoked ones included, oldest first.
	ListKeys(ctx context.Context, appID string) ([]APIKey, error)
	// GetKey returns a key by ID, revoked or not.
	GetKey(ctx context.Context, keyID string) (*APIKey, error)
	// SetKeyPolicy sets a key's expiry and allowed models (nil: all).
	SetKeyPolicy(ctx context.Context, keyID string, expiresAt *time.Time, allowedModels []string) (*APIKey, error)
	// LookupKey returns ErrNotFound for unknown or revoked keys. Expired
	// keys are returned; the caller checks ExpiresAt.
	LookupKey(ctx context.Context, hash string) (*Principal, error)
	// CreateProxyToken stores a minted token by hash, filling in its ID and
	// creation time, and forgets tokens that expired over a day ago.
	CreateProxyToken(ctx context.Context, t *ProxyToken, hash string) error
	// LookupProxyToken returns ErrNotFound for unknown, revoked or expired
	// tokens. TenantName and AppName are filled in.
	LookupProxyToken(ctx context.Context, hash string) (*ProxyToken, error)
	RevokeProxyToken(ctx context.Context, id string) error
	CreateLimit(ctx context.Context, l *Limit) error
	// UpdateLimit saves a limit's amount, period and enforcement.
	UpdateLimit(ctx context.Context, l *Limit) error
	GetLimit(ctx context.Context, id string) (*Limit, error)
	ListLimits(ctx context.Context) ([]Limit, error)
	DeleteLimit(ctx context.Context, id string) error
	InsertUsage(ctx context.Context, events []UsageEvent) error
	QueryUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error)
	AddPrice(ctx context.Context, p *Price) error
	// ListPrices returns every price, oldest effective first.
	ListPrices(ctx context.Context) ([]Price, error)
	// SaveInsight inserts or updates an insight by ID.
	SaveInsight(ctx context.Context, in *Insight) error
	// ListInsights returns insights with the given status ("" for all), newest first.
	ListInsights(ctx context.Context, status string) ([]Insight, error)
	AddAudit(ctx context.Context, e *AuditEntry) error
	// ListAudit returns up to limit entries older than before (zero: from
	// the newest), newest first.
	ListAudit(ctx context.Context, before time.Time, limit int) ([]AuditEntry, error)
	Close()
}
