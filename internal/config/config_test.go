package config

import (
	"strings"
	"testing"
)

func TestAuthValidation(t *testing.T) {
	cases := []struct {
		yaml, wantErr string
	}{
		{"auth: {jwt: [{issuer: https://idp.example.com, audience: proxy, tenant: acme}]}", ""},
		{"auth: {jwt: [{issuer: https://idp.example.com, audience: proxy, claims: {tenant: org}}]}", ""},
		{"auth: {jwt: [{issuer: https://idp.example.com, tenant: acme}]}", "audience is required"},
		{"auth: {jwt: [{issuer: https://idp.example.com, audience: proxy}]}", "claims.tenant"},
		{"auth: {jwt: [{issuer: idp.example.com, audience: proxy, tenant: acme}]}", "http(s) URL"},
		{"auth: {api_keys: false}", "no request could authenticate"},
	}
	for _, c := range cases {
		cfg, err := Parse([]byte(c.yaml))
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: %v", c.yaml, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: got %v, want an error containing %q", c.yaml, err, c.wantErr)
		case err == nil && cfg.Auth.JWT[0].Claims.User != "email":
			t.Errorf("%s: default user claim not applied", c.yaml)
		}
	}
}

func TestProviderValidation(t *testing.T) {
	for yaml, wantErr := range map[string]string{
		"providers: [{name: openai, base_url: https://api.openai.com/v1}]":                  "",
		"providers: [{name: my-vllm.2, base_url: http://vllm:8000/v1}]":                     "",
		"providers: [{name: admin, base_url: https://x/v1}]":                                "not one of",
		"providers: [{name: Open/AI, base_url: https://x/v1}]":                              "lowercase",
		"providers: [{name: openai}]":                                                       "base_url",
		"providers: [{name: a, base_url: https://x/v1}, {name: a, base_url: https://y/v1}]": "duplicate",
	} {
		_, err := Parse([]byte(yaml))
		if (wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), wantErr)) {
			t.Errorf("%s: got %v, want %q", yaml, err, wantErr)
		}
	}
}

func TestTracingValidation(t *testing.T) {
	cfg, err := Parse([]byte("tracing: {endpoint: http://otel:4318}"))
	if err != nil || cfg.Tracing.ServiceName != "omni-proxy" || *cfg.Tracing.SampleRatio != 1 {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	for yaml, want := range map[string]string{
		"tracing: {endpoint: otel:4318}": "tracing.endpoint",
		"tracing: {sample_ratio: 2}":     "sample_ratio",
	} {
		if _, err := Parse([]byte(yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error containing %q", yaml, err, want)
		}
	}
}
