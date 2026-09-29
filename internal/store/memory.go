package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
)

// Memory is an in-process store for development and tests. Nothing survives a
// restart.
type Memory struct {
	mu       sync.RWMutex
	tenants  map[string]*Tenant
	apps     map[string]*Application
	keys     map[string]*APIKey // by hash
	events   []UsageEvent
	insights map[string]Insight
	prices   []Price
}

func NewMemory() *Memory {
	return &Memory{tenants: map[string]*Tenant{}, apps: map[string]*Application{}, keys: map[string]*APIKey{}, insights: map[string]Insight{}}
}

func (m *Memory) Close() {}

func (m *Memory) EnsureTenant(_ context.Context, name string) (*Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tenants {
		if t.Name == name {
			c := *t
			return &c, nil
		}
	}
	t := &Tenant{ID: openresponses.NewID("ten"), Name: name, CreatedAt: time.Now().UTC()}
	m.tenants[t.ID] = t
	c := *t
	return &c, nil
}

func (m *Memory) ListTenants(context.Context) ([]Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Tenant, 0, len(m.tenants))
	for _, t := range m.tenants {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) EnsureApplication(_ context.Context, tenantID, name string, canAssert bool) (*Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[tenantID]; !ok {
		return nil, ErrNotFound
	}
	for _, a := range m.apps {
		if a.TenantID == tenantID && a.Name == name {
			c := *a
			return &c, nil
		}
	}
	a := &Application{ID: openresponses.NewID("app"), TenantID: tenantID, Name: name, CanAssertUsers: canAssert, CreatedAt: time.Now().UTC()}
	m.apps[a.ID] = a
	c := *a
	return &c, nil
}

func (m *Memory) ListApplications(_ context.Context, tenantID string) ([]Application, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Application
	for _, a := range m.apps {
		if tenantID == "" || a.TenantID == tenantID {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) CreateKey(_ context.Context, appID, hash, prefix string) (*APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.apps[appID]; !ok {
		return nil, ErrNotFound
	}
	if k, ok := m.keys[hash]; ok {
		c := *k
		return &c, nil
	}
	k := &APIKey{ID: openresponses.NewID("key"), AppID: appID, Prefix: prefix, CreatedAt: time.Now().UTC()}
	m.keys[hash] = k
	c := *k
	return &c, nil
}

func (m *Memory) RevokeKey(_ context.Context, keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if k.ID == keyID {
			now := time.Now().UTC()
			k.RevokedAt = &now
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) ListKeys(_ context.Context, appID string) ([]APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []APIKey{}
	for _, k := range m.keys {
		if k.AppID == appID {
			out = append(out, *k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) LookupKey(_ context.Context, hash string) (*Principal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[hash]
	if !ok || k.RevokedAt != nil {
		return nil, ErrNotFound
	}
	a := m.apps[k.AppID]
	t := m.tenants[a.TenantID]
	return &Principal{KeyID: k.ID, TenantID: t.ID, TenantName: t.Name, AppID: a.ID, AppName: a.Name, CanAssertUsers: a.CanAssertUsers}, nil
}

func (m *Memory) InsertUsage(_ context.Context, events []UsageEvent) error {
	m.mu.Lock()
	m.events = append(m.events, events...)
	m.mu.Unlock()
	return nil
}

func (m *Memory) QueryUsage(_ context.Context, q UsageQuery) ([]UsageRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rows := map[string]*UsageRow{}
	var order []string
	for i := range m.events {
		e := &m.events[i]
		if e.TS.Before(q.From) || !e.TS.Before(q.To) {
			continue
		}
		if !matches(e, q.Filters) {
			continue
		}
		var bucket *time.Time
		switch q.Granularity {
		case "hour":
			b := e.TS.UTC().Truncate(time.Hour)
			bucket = &b
		case "day":
			y, mo, d := e.TS.UTC().Date()
			b := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
			bucket = &b
		}
		group := map[string]string{}
		var key strings.Builder
		if bucket != nil {
			key.WriteString(bucket.Format(time.RFC3339))
		}
		for _, g := range q.GroupBy {
			v := dimension(e, g)
			group[g] = v
			key.WriteString("\x00" + v)
		}
		r, ok := rows[key.String()]
		if !ok {
			r = &UsageRow{Bucket: bucket, Group: group}
			rows[key.String()] = r
			order = append(order, key.String())
		}
		r.Requests++
		if e.Status == "failed" {
			r.FailedRequests++
		}
		r.InputTokens += int64(e.InputTokens)
		r.CachedInputTokens += int64(e.CachedInputTokens)
		r.CacheWriteTokens += int64(e.CacheWriteTokens)
		r.OutputTokens += int64(e.OutputTokens)
		r.ReasoningTokens += int64(e.ReasoningTokens)
		r.TotalTokens += int64(e.InputTokens + e.OutputTokens)
		r.CostUSD += e.CostUSD
	}
	out := make([]UsageRow, 0, len(order))
	for _, k := range order {
		out = append(out, *rows[k])
	}
	sortRows(out)
	return out, nil
}

func dimension(e *UsageEvent, d string) string {
	switch d {
	case "tenant":
		return e.TenantID
	case "application":
		return e.AppID
	case "email":
		return e.UserEmail
	case "model":
		return e.Model
	case "provider":
		return e.Provider
	case "cache":
		return e.CacheStatus
	}
	return ""
}

func matches(e *UsageEvent, filters map[string]string) bool {
	for k, v := range filters {
		if dimension(e, k) != v {
			return false
		}
	}
	return true
}

// sortRows orders by bucket, then by total tokens descending.
func sortRows(rows []UsageRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		bi, bj := rows[i].Bucket, rows[j].Bucket
		if bi != nil && bj != nil && !bi.Equal(*bj) {
			return bi.Before(*bj)
		}
		return rows[i].TotalTokens > rows[j].TotalTokens
	})
}

func (m *Memory) SaveInsight(_ context.Context, in *Insight) error {
	m.mu.Lock()
	m.insights[in.ID] = *in
	m.mu.Unlock()
	return nil
}

func (m *Memory) ListInsights(_ context.Context, status string) ([]Insight, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Insight{}
	for _, in := range m.insights {
		if status == "" || in.Status == status {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out, nil
}

func (m *Memory) AddPrice(_ context.Context, p *Price) error {
	m.mu.Lock()
	m.prices = append(m.prices, *p)
	m.mu.Unlock()
	return nil
}

func (m *Memory) ListPrices(context.Context) ([]Price, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]Price{}, m.prices...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].EffectiveFrom.Before(out[j].EffectiveFrom) })
	return out, nil
}
