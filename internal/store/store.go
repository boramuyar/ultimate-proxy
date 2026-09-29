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
}

// Principal is who a request is billed to, resolved from its API key.
type Principal struct {
	KeyID          string
	TenantID       string
	TenantName     string
	AppID          string
	AppName        string
	CanAssertUsers bool
}

// UsageEvent is one metered request.
type UsageEvent struct {
	TS                time.Time
	RequestID         string
	TenantID          string
	AppID             string
	KeyID             string
	UserEmail         string
	UserSource        string // header, metadata, safety_identifier or none
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
}

// Dimensions usage can be grouped and filtered by.
var UsageDimensions = map[string]string{
	"tenant":      "tenant_id",
	"application": "app_id",
	"email":       "user_email",
	"model":       "model",
	"provider":    "provider",
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
}

type Store interface {
	EnsureTenant(ctx context.Context, name string) (*Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	EnsureApplication(ctx context.Context, tenantID, name string, canAssertUsers bool) (*Application, error)
	ListApplications(ctx context.Context, tenantID string) ([]Application, error)
	// CreateKey stores a key by hash. It is idempotent for the same hash.
	CreateKey(ctx context.Context, appID, hash, prefix string) (*APIKey, error)
	RevokeKey(ctx context.Context, keyID string) error
	// LookupKey returns ErrNotFound for unknown or revoked keys.
	LookupKey(ctx context.Context, hash string) (*Principal, error)
	InsertUsage(ctx context.Context, events []UsageEvent) error
	QueryUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error)
	Close()
}
