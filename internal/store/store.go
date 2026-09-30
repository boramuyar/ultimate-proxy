// Package store persists tenants, applications, API keys and usage events.
package store

import (
	"context"
	"errors"
	"fmt"
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
	Deployment        string // the provider deployment that served it, never its key
	Attempts          int    // upstream calls made, retries and fallbacks included
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
}

// Dimensions usage can be grouped and filtered by.
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
	GroupBy     []string          // keys of UsageDimensions
	Granularity string            // "", "hour" or "day"
	Filters     map[string]string // UsageDimensions key -> value
}

func (q *UsageQuery) Validate() error {
	for _, g := range q.GroupBy {
		if _, ok := UsageDimensions[g]; !ok {
			return fmt.Errorf("unknown group_by %q", g)
		}
	}
	for f := range q.Filters {
		if _, ok := UsageDimensions[f]; !ok {
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
	InsertUsage(ctx context.Context, events []UsageEvent) error
	QueryUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error)
	AddPrice(ctx context.Context, p *Price) error
	// ListPrices returns every price, oldest effective first.
	ListPrices(ctx context.Context) ([]Price, error)
	// SaveInsight inserts or updates an insight by ID.
	SaveInsight(ctx context.Context, in *Insight) error
	// ListInsights returns insights with the given status ("" for all), newest first.
	ListInsights(ctx context.Context, status string) ([]Insight, error)
	Close()
}
