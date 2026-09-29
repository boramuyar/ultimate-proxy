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
	Bootstrap []Tenant   `yaml:"bootstrap"`
}

type Provider struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"` // openai or anthropic
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`
	// DefaultMaxTokens is used when a request sets no max_output_tokens and
	// the upstream requires a limit (Anthropic).
	DefaultMaxTokens int `yaml:"default_max_tokens"`
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
		case "openai", "anthropic":
		default:
			return fmt.Errorf("provider %q: unknown type %q (want openai or anthropic)", p.Name, p.Type)
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
