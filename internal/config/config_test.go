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
