// Package config loads the proxy's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen string `yaml:"listen"`
	// DatabaseURL is a Postgres URL. When empty, the proxy keeps everything
	// in memory (development only).
	DatabaseURL string `yaml:"database_url"`
	// ClickHouseURL is where the request log (usage events) goes, as
	// http://user:password@host:8123/database. When empty, usage stays in
	// the database_url store.
	ClickHouseURL string `yaml:"clickhouse_url"`
	// RedisURL is a Valkey (or Redis) URL, redis://host:6379/0, where rate
	// limit counters live so every replica shares them. When empty, each
	// replica counts on its own.
	RedisURL string `yaml:"redis_url"`
	Limits   Limits `yaml:"limits"`
	// AdminToken is the old name of Admin.Token, still accepted.
	AdminToken string `yaml:"admin_token"`
	Admin      Admin  `yaml:"admin"`
	Auth       Auth   `yaml:"auth"`

	MaxRequestBytes       int64         `yaml:"max_request_bytes"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`

	Usage struct {
		QueueSize     int           `yaml:"queue_size"`
		BatchSize     int           `yaml:"batch_size"`
		FlushInterval time.Duration `yaml:"flush_interval"`
		// RetentionDays is how long ClickHouse keeps each request's raw usage
		// event. Hourly totals are kept forever. 0 keeps raw events forever;
		// unset means 90.
		RetentionDays *int `yaml:"retention_days"`
	} `yaml:"usage"`

	Providers []Provider `yaml:"providers"`
	Insights  Insights   `yaml:"insights"`
	Bootstrap []Tenant   `yaml:"bootstrap"`
}

// Limits configures rate limit enforcement. The rules themselves live in the
// database and are managed with /admin/limits.
type Limits struct {
	// FailClosed refuses requests while the counters (Valkey) are
	// unreachable. By default they are let through.
	FailClosed bool `yaml:"fail_closed"`
	// ReloadInterval is how often each replica rereads the rules.
	ReloadInterval time.Duration `yaml:"reload_interval"`
}

// Insights configures problem detection and alerting.
type Insights struct {
	Disabled bool `yaml:"disabled"`
	// CacheTTL is how long a provider keeps a prompt prefix cached. A prefix
	// sent again within it is expected to hit.
	CacheTTL time.Duration `yaml:"cache_ttl"`
	// MinCacheableTokens is the provider's minimum cacheable prompt size.
	MinCacheableTokens int `yaml:"min_cacheable_tokens"`
	// Window is how much recent traffic each rule looks at.
	Window time.Duration `yaml:"window"`
	// EvaluateInterval is how often rules run.
	EvaluateInterval time.Duration `yaml:"evaluate_interval"`
	// MinRequests is the least traffic in the window before a rule may fire.
	MinRequests int `yaml:"min_requests"`

	// Thresholds, as fractions of the window's requests. An insight opens at
	// the threshold and resolves when the rate falls below half of it.
	UnstablePrefixRate float64 `yaml:"unstable_prefix_rate"`
	UnexpectedMissRate float64 `yaml:"unexpected_miss_rate"`
	ErrorRate          float64 `yaml:"error_rate"`
	TruncationRate     float64 `yaml:"truncation_rate"`

	// WebhookURL receives a JSON POST whenever an insight opens or resolves.
	WebhookURL string `yaml:"webhook_url"`
	// SlackWebhookURL receives the same as a Slack message.
	SlackWebhookURL string `yaml:"slack_webhook_url"`
}

// Admin configures who may use /admin and the dashboard. Sign-in is through
// an OpenID Connect provider, a static token, or both. With neither, the
// admin API is disabled.
type Admin struct {
	// Token is a break-glass bearer token, for scripts and for when the
	// identity provider is down. Empty disables it.
	Token string `yaml:"token"`
	// SessionSecret signs dashboard session cookies. When empty, a random one
	// is made at startup, so sessions end when the proxy restarts. Set it when
	// running more than one proxy.
	SessionSecret string        `yaml:"session_secret"`
	SessionTTL    time.Duration `yaml:"session_ttl"`
	OIDC          OIDC          `yaml:"oidc"`
}

// Auth configures how API callers authenticate: with the proxy's own API
// keys, with JWTs from the organization's own identity provider, or both.
type Auth struct {
	// APIKeys turns the proxy's own keys on or off. Unset means on.
	APIKeys *bool       `yaml:"api_keys"`
	JWT     []JWTIssuer `yaml:"jwt"`
	// CORSOrigins are the web origins whose pages may call /v1 from a
	// browser, such as https://app.example.com, or "*" for any. Empty
	// allows none.
	CORSOrigins []string `yaml:"cors_origins"`
}

// APIKeysEnabled reports whether the proxy's own API keys are accepted.
func (a *Auth) APIKeysEnabled() bool { return a.APIKeys == nil || *a.APIKeys }

// JWTIssuer is an identity provider whose access tokens the proxy accepts:
// Keycloak (one entry per realm), Microsoft Entra ID, Okta, Auth0 and so on.
type JWTIssuer struct {
	// Issuer must equal the tokens' iss claim exactly, trailing slash
	// included. Its signing keys are found
	// through <issuer>/.well-known/openid-configuration unless JWKSURL is set.
	Issuer  string `yaml:"issuer"`
	JWKSURL string `yaml:"jwks_url"`
	// Audience must be in the tokens' aud claim, so tokens the provider
	// issued for other services are refused.
	Audience string `yaml:"audience"`
	// Tenant is a fixed tenant for every token from this issuer, used when
	// Claims.Tenant is unset or missing from a token.
	Tenant string `yaml:"tenant"`
	// Claims names the claims that carry each part of the caller's identity.
	Claims JWTClaims `yaml:"claims"`
	// GroupModels lists the models each group may use. A caller may use the
	// models of all its groups. When set, a caller in none of the groups may
	// use no model; when unset, groups do not limit models.
	GroupModels map[string][]string `yaml:"group_models"`
}

type JWTClaims struct {
	Tenant string `yaml:"tenant"`
	App    string `yaml:"app"`    // default azp, then client_id
	User   string `yaml:"user"`   // default email
	Groups string `yaml:"groups"` // default groups
	// Models is a claim listing the models the caller may use, as an array
	// or a space-separated string. Unset: the claim does not limit models.
	Models string `yaml:"models"`
}

// OIDC is any OpenID Connect provider: Google, Microsoft Entra ID, Okta,
// Auth0, Keycloak, Authentik, Dex, GitLab and so on. It is on when Issuer is
// set.
type OIDC struct {
	// Issuer is the provider's issuer URL; its discovery document is at
	// <issuer>/.well-known/openid-configuration.
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// RedirectURL is <dashboard origin>/admin/auth/callback, as registered
	// with the provider.
	RedirectURL string `yaml:"redirect_url"`
	// DisplayName labels the sign-in button ("Continue with <name>").
	DisplayName string   `yaml:"display_name"`
	Scopes      []string `yaml:"scopes"`

	// Who may sign in as a full admin: anyone matching any of these. Each
	// entry may hold several values separated by commas, so a list can come
	// from one environment variable.
	AllowedEmails  []string `yaml:"allowed_emails"`
	AllowedDomains []string `yaml:"allowed_domains"`
	AllowedGroups  []string `yaml:"allowed_groups"`
	// Roles let more people sign in with less access: read only, or limited
	// to some tenants. Someone matching several entries gets all of them.
	// Entries naming nobody are ignored.
	Roles []RoleGrant `yaml:"roles"`
	// GroupsClaim is the ID token claim holding the user's groups.
	GroupsClaim string `yaml:"groups_claim"`
}

// Dashboard and admin API roles.
const (
	RoleAdmin  = "admin"  // may change things
	RoleViewer = "viewer" // may only read
)

// RoleGrant gives a role to the people matching its emails, domains or
// groups, for every tenant or only the named ones.
type RoleGrant struct {
	Role    string   `yaml:"role"`
	Emails  []string `yaml:"emails"`
	Domains []string `yaml:"domains"`
	Groups  []string `yaml:"groups"`
	// Tenants are tenant names. Empty means every tenant, and with it the
	// settings no tenant owns: prices, new tenants and the audit log.
	Tenants []string `yaml:"tenants"`
}

// Enabled reports whether OIDC sign-in is configured.
func (o *OIDC) Enabled() bool { return o.Issuer != "" }

// Provider is an upstream that speaks the OpenAI API. Clients reach it at
// /<name>/v1/..., and the proxy forwards their requests unchanged.
type Provider struct {
	Name    string            `yaml:"name"`
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`
}

// Tenant, Application and keys to create at startup, so a fresh deployment
// can serve traffic without calling the admin API.
type Tenant struct {
	Tenant       string        `yaml:"tenant"`
	Applications []Application `yaml:"applications"`
}

type Application struct {
	Name           string   `yaml:"name"`
	CanAssertUsers *bool    `yaml:"can_assert_users"`
	Keys           []string `yaml:"keys"`
}

// Load reads a config file, expanding ${VAR} references from the environment.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse([]byte(os.ExpandEnv(string(raw))))
}

func Parse(raw []byte) (*Config, error) {
	c := &Config{}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return c, c.validate()
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 64 << 20
	}
	if c.ResponseHeaderTimeout == 0 {
		c.ResponseHeaderTimeout = 120 * time.Second
	}
	if c.Limits.ReloadInterval == 0 {
		c.Limits.ReloadInterval = 10 * time.Second
	}
	if c.Usage.QueueSize == 0 {
		c.Usage.QueueSize = 100_000
	}
	if c.Usage.BatchSize == 0 {
		c.Usage.BatchSize = 1000
	}
	if c.Usage.FlushInterval == 0 {
		c.Usage.FlushInterval = 250 * time.Millisecond
	}
	if c.Usage.RetentionDays == nil {
		days := 90
		c.Usage.RetentionDays = &days
	}
	if c.Admin.Token == "" {
		c.Admin.Token = c.AdminToken
	}
	if c.Admin.SessionTTL == 0 {
		c.Admin.SessionTTL = 12 * time.Hour
	}
	o := &c.Admin.OIDC
	o.AllowedEmails = splitList(o.AllowedEmails, true)
	o.AllowedDomains = splitList(o.AllowedDomains, true)
	o.AllowedGroups = splitList(o.AllowedGroups, false)
	roles := o.Roles[:0]
	for _, g := range o.Roles {
		g.Emails, g.Domains = splitList(g.Emails, true), splitList(g.Domains, true)
		g.Groups, g.Tenants = splitList(g.Groups, false), splitList(g.Tenants, false)
		// An entry naming nobody is dropped, so one filled from environment
		// variables can be left empty.
		if len(g.Emails)+len(g.Domains)+len(g.Groups) > 0 {
			roles = append(roles, g)
		}
	}
	o.Roles = roles
	// Scopes are space-separated in OAuth; accept either.
	o.Scopes = strings.Fields(strings.ReplaceAll(strings.Join(o.Scopes, " "), ",", " "))
	if len(o.Scopes) == 0 {
		o.Scopes = []string{"openid", "email", "profile"}
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
	if o.DisplayName == "" {
		o.DisplayName = "SSO"
	}
	c.Auth.CORSOrigins = splitList(c.Auth.CORSOrigins, false)
	for i := range c.Auth.JWT {
		j := &c.Auth.JWT[i]
		if j.Claims.User == "" {
			j.Claims.User = "email"
		}
		if j.Claims.Groups == "" {
			j.Claims.Groups = "groups"
		}
	}
	in := &c.Insights
	if in.CacheTTL == 0 {
		in.CacheTTL = 5 * time.Minute
	}
	if in.MinCacheableTokens == 0 {
		in.MinCacheableTokens = 1024
	}
	if in.Window == 0 {
		in.Window = 15 * time.Minute
	}
	if in.EvaluateInterval == 0 {
		in.EvaluateInterval = time.Minute
	}
	if in.MinRequests == 0 {
		in.MinRequests = 20
	}
	if in.UnstablePrefixRate == 0 {
		in.UnstablePrefixRate = 0.3
	}
	if in.UnexpectedMissRate == 0 {
		in.UnexpectedMissRate = 0.3
	}
	if in.ErrorRate == 0 {
		in.ErrorRate = 0.1
	}
	if in.TruncationRate == 0 {
		in.TruncationRate = 0.2
	}
}

// splitList splits comma-separated entries and drops empty ones, which an
// unset ${VAR} leaves behind.
func splitList(in []string, lower bool) []string {
	var out []string
	for _, e := range in {
		for _, v := range strings.Split(e, ",") {
			v = strings.TrimSpace(v)
			if lower {
				v = strings.ToLower(v)
			}
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

func (c *Config) validate() error {
	if *c.Usage.RetentionDays < 0 {
		return fmt.Errorf("usage.retention_days must be 0 (forever) or more")
	}
	if o := c.Admin.OIDC; o.Enabled() {
		if o.ClientID == "" || o.RedirectURL == "" {
			return fmt.Errorf("admin.oidc: client_id and redirect_url are required with issuer")
		}
		u, err := url.Parse(o.RedirectURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "/admin/auth/callback" {
			return fmt.Errorf("admin.oidc: redirect_url must be <dashboard origin>/admin/auth/callback, got %q", o.RedirectURL)
		}
		people := len(o.AllowedEmails) + len(o.AllowedDomains) + len(o.AllowedGroups)
		for i, g := range o.Roles {
			if g.Role != RoleAdmin && g.Role != RoleViewer {
				return fmt.Errorf("admin.oidc.roles[%d]: role must be admin or viewer, got %q", i, g.Role)
			}
			people += len(g.Emails) + len(g.Domains) + len(g.Groups)
		}
		if people == 0 {
			return fmt.Errorf("admin.oidc: set allowed_emails, allowed_domains, allowed_groups or roles, or every account at the provider could sign in")
		}
	}
	issuers := map[string]bool{}
	for _, j := range c.Auth.JWT {
		u, err := url.Parse(j.Issuer)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("auth.jwt: issuer must be an http(s) URL, got %q", j.Issuer)
		}
		if issuers[j.Issuer] {
			return fmt.Errorf("auth.jwt: issuer %q is listed twice", j.Issuer)
		}
		issuers[j.Issuer] = true
		if j.Audience == "" {
			return fmt.Errorf("auth.jwt %s: audience is required, or tokens meant for any other service would be accepted", j.Issuer)
		}
		if j.Tenant == "" && j.Claims.Tenant == "" {
			return fmt.Errorf("auth.jwt %s: set tenant (a fixed value) or claims.tenant", j.Issuer)
		}
	}
	if !c.Auth.APIKeysEnabled() && len(c.Auth.JWT) == 0 {
		return fmt.Errorf("auth: api_keys is off and no jwt issuer is set, so no request could authenticate")
	}
	names := map[string]bool{}
	for _, p := range c.Providers {
		if !validProviderName(p.Name) {
			return fmt.Errorf("provider name %q must be lowercase letters, digits, '-', '_' or '.', and not one of %s", p.Name, strings.Join(reservedPaths, ", "))
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		names[p.Name] = true
		if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q: base_url must be an http(s) URL, got %q", p.Name, p.BaseURL)
		}
	}
	return nil
}

// reservedPaths are first path segments the proxy serves itself, so no
// provider may take them.
var reservedPaths = []string{"admin", "v1", "healthz", "metrics"}

func validProviderName(n string) bool {
	if n == "" || slices.Contains(reservedPaths, n) {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
