// Package config loads the proxy's YAML configuration.
package config

import (
	"fmt"
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
	// AdminToken guards /admin. When empty, the admin API is disabled.
	AdminToken string `yaml:"admin_token"`

	MaxRequestBytes       int64         `yaml:"max_request_bytes"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`

	Usage struct {
		QueueSize     int           `yaml:"queue_size"`
		BatchSize     int           `yaml:"batch_size"`
		FlushInterval time.Duration `yaml:"flush_interval"`
	} `yaml:"usage"`

	Providers []Provider `yaml:"providers"`
	Models    []Model    `yaml:"models"`
	Prices    []Price    `yaml:"prices"`
	Insights  Insights   `yaml:"insights"`
	Bootstrap []Tenant   `yaml:"bootstrap"`
}

// Price is USD per million tokens. Model matches a client-facing alias, an
// upstream model name, or "<provider>/<upstream model>". CachedInput and
// CacheWrite default to Input when unset.
type Price struct {
	Model       string   `yaml:"model"`
	Input       float64  `yaml:"input"`
	CachedInput *float64 `yaml:"cached_input"`
	CacheWrite  *float64 `yaml:"cache_write"`
	Output      float64  `yaml:"output"`
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

func (c *Config) validate() error {
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
	for _, p := range c.Prices {
		if p.Model == "" || p.Input < 0 || p.Output < 0 {
			return fmt.Errorf("price entries need a model and non-negative prices")
		}
	}
	return nil
}
