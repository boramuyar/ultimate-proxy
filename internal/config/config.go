// Package config loads the proxy's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
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
	// AdminToken is the old name of Admin.Token, still accepted.
	AdminToken string `yaml:"admin_token"`
	Admin      Admin  `yaml:"admin"`

	MaxRequestBytes       int64         `yaml:"max_request_bytes"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`

	Usage struct {
		QueueSize     int           `yaml:"queue_size"`
		BatchSize     int           `yaml:"batch_size"`
		FlushInterval time.Duration `yaml:"flush_interval"`
	} `yaml:"usage"`

	Providers []Provider `yaml:"providers"`
	Models    []Model    `yaml:"models"`
	Insights  Insights   `yaml:"insights"`
	Bootstrap []Tenant   `yaml:"bootstrap"`
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

	// Who may sign in: anyone matching any of these. At least one is required.
	// Each entry may hold several values separated by commas, so a list can
	// come from one environment variable.
	AllowedEmails  []string `yaml:"allowed_emails"`
	AllowedDomains []string `yaml:"allowed_domains"`
	AllowedGroups  []string `yaml:"allowed_groups"`
	// GroupsClaim is the ID token claim holding the user's groups.
	GroupsClaim string `yaml:"groups_claim"`
}

// Enabled reports whether OIDC sign-in is configured.
func (o *OIDC) Enabled() bool { return o.Issuer != "" }

type Provider struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"` // openai
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`
}

// Model maps a client-facing model name to a provider and upstream model.
// Requests may also name "<provider>/<upstream model>" directly.
type Model struct {
	Name          string `yaml:"name"`
	Provider      string `yaml:"provider"`
	UpstreamModel string `yaml:"upstream_model"`
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
	if c.Usage.QueueSize == 0 {
		c.Usage.QueueSize = 100_000
	}
	if c.Usage.BatchSize == 0 {
		c.Usage.BatchSize = 1000
	}
	if c.Usage.FlushInterval == 0 {
		c.Usage.FlushInterval = 250 * time.Millisecond
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
	if o := c.Admin.OIDC; o.Enabled() {
		if o.ClientID == "" || o.RedirectURL == "" {
			return fmt.Errorf("admin.oidc: client_id and redirect_url are required with issuer")
		}
		u, err := url.Parse(o.RedirectURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "/admin/auth/callback" {
			return fmt.Errorf("admin.oidc: redirect_url must be <dashboard origin>/admin/auth/callback, got %q", o.RedirectURL)
		}
		if len(o.AllowedEmails)+len(o.AllowedDomains)+len(o.AllowedGroups) == 0 {
			return fmt.Errorf("admin.oidc: set allowed_emails, allowed_domains or allowed_groups, or every account at the provider could sign in")
		}
	}
	names := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || strings.Contains(p.Name, "/") {
			return fmt.Errorf("provider name %q must be non-empty and contain no '/'", p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		names[p.Name] = true
		switch p.Type {
		case "openai":
		default:
			return fmt.Errorf("provider %q: unknown type %q (want openai)", p.Name, p.Type)
		}
	}
	for _, m := range c.Models {
		if m.Name == "" || m.UpstreamModel == "" {
			return fmt.Errorf("model entries need name and upstream_model")
		}
		if !names[m.Provider] {
			return fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider)
		}
	}
	return nil
}
