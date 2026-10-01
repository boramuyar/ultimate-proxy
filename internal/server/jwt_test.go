package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/fakeoidc"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

func newJWTHarness(t *testing.T, authYAML string) (*harness, *fakeoidc.Server) {
	t.Helper()
	idp := fakeoidc.New("dashboard", "secret")
	srv := httptest.NewServer(idp.Handler())
	t.Cleanup(srv.Close)
	idp.Issuer = srv.URL
	return newHarnessWith(t, strings.ReplaceAll(authYAML, "ISSUER", srv.URL)), idp
}

const jwtAuth = `
auth:
  jwt:
    - issuer: ISSUER
      audience: ultimate-proxy
      claims: {tenant: org, models: llm_models}
`

func (h *harness) events(want int) []store.UsageEvent {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ev := h.store.Events()
		if len(ev) >= want || time.Now().After(deadline) {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestJWTAttributesUsageToClaims(t *testing.T) {
	h, idp := newJWTHarness(t, jwtAuth)
	token := idp.AccessToken(map[string]any{
		"sub": "u-42", "aud": "ultimate-proxy", "org": "initech", "azp": "coding-agent", "email": "Peter@Initech.com",
	})
	// The user named by the token wins over one the caller asserts.
	resp := h.post(token, `{"model":"gpt","input":"hi"}`, "X-Proxy-User-Email", "someone@else.com")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, decode(t, resp))
	}
	resp.Body.Close()
	// A second request is served from the verified-token cache.
	h.post(token, `{"model":"gpt","input":"again"}`).Body.Close()

	ev := h.events(2)
	if len(ev) != 2 {
		t.Fatalf("got %d usage events", len(ev))
	}
	e := ev[0]
	if e.AuthMethod != "jwt" || e.Subject != "u-42" || e.KeyID != "" || e.UserEmail != "peter@initech.com" || e.UserSource != "jwt" {
		t.Errorf("unexpected event %+v", e)
	}
	tenants, _ := h.store.ListTenants(t.Context())
	var tenantID string
	for _, tn := range tenants {
		if tn.Name == "initech" {
			tenantID = tn.ID
		}
	}
	if tenantID == "" || e.TenantID != tenantID || ev[1].TenantID != tenantID {
		t.Fatalf("tenant not registered or not used: %v, event tenant %q", tenants, e.TenantID)
	}
	apps, _ := h.store.ListApplications(t.Context(), tenantID)
	if len(apps) != 1 || apps[0].Name != "coding-agent" || apps[0].ID != e.AppID || apps[0].CanAssertUsers {
		t.Errorf("unexpected applications %+v", apps)
	}
}

func TestJWTRejections(t *testing.T) {
	h, idp := newJWTHarness(t, jwtAuth)
	good := map[string]any{"sub": "u", "aud": "ultimate-proxy", "org": "initech"}
	with := func(k string, v any) map[string]any {
		c := map[string]any{}
		for kk, vv := range good {
			c[kk] = vv
		}
		c[k] = v
		return c
	}
	other := fakeoidc.New("x", "y")
	other.Issuer = idp.Issuer // same issuer URL, different signing key

	valid := idp.AccessToken(good)
	cases := []struct {
		name, token, code string
	}{
		{"expired", idp.AccessToken(with("exp", time.Now().Add(-time.Minute).Unix())), "token_expired"},
		{"wrong audience", idp.AccessToken(with("aud", "some-other-service")), "invalid_token"},
		{"unknown issuer", idp.AccessToken(with("iss", "https://evil.example.com")), "invalid_token"},
		{"wrong key", other.AccessToken(good), "invalid_token"},
		{"tampered", valid[:len(valid)-4] + "AAAA", "invalid_token"},
		{"no tenant", idp.AccessToken(map[string]any{"sub": "u", "aud": "ultimate-proxy"}), "invalid_token"},
	}
	for _, c := range cases {
		resp := h.post(c.token, `{"model":"gpt","input":"hi"}`)
		body := decode(t, resp)
		if resp.StatusCode != http.StatusUnauthorized || body["error"].(map[string]any)["code"] != c.code {
			t.Errorf("%s: status %d, body %v; want 401 %s", c.name, resp.StatusCode, body, c.code)
		}
	}
	// API keys still work next to JWTs.
	if resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`); resp.StatusCode != 200 {
		t.Errorf("api key: status %d", resp.StatusCode)
	}
}

func TestJWTModelAllowlist(t *testing.T) {
	h, idp := newJWTHarness(t, jwtAuth+`      group_models:
        ml: [openai/*]
`)
	cases := []struct {
		name   string
		claims map[string]any
		model  string
		status int
	}{
		{"claim allows model", map[string]any{"llm_models": []any{"gpt"}}, "gpt", 200},
		{"claim refuses", map[string]any{"llm_models": "other-model"}, "gpt", 403},
		{"claim wildcard", map[string]any{"llm_models": "openai/*"}, "gpt-fake", 200},
		{"group allows", map[string]any{"groups": []any{"ml"}}, "gpt", 200},
		{"no group", map[string]any{"groups": []any{"sales"}}, "gpt", 403},
	}
	for _, c := range cases {
		claims := map[string]any{"sub": "u", "aud": "ultimate-proxy", "org": "initech"}
		for k, v := range c.claims {
			claims[k] = v
		}
		token := idp.AccessToken(claims)
		resp := h.post(token, `{"model":"`+c.model+`","input":"hi"}`)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.status)
		}
	}
}

func TestAPIKeysCanBeTurnedOff(t *testing.T) {
	h, idp := newJWTHarness(t, jwtAuth+"  api_keys: false\n")
	if resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("api key with api_keys off: status %d, want 401", resp.StatusCode)
	}
	token := idp.AccessToken(map[string]any{"sub": "u", "aud": "ultimate-proxy", "org": "initech"})
	if resp := h.post(token, `{"model":"gpt","input":"hi"}`); resp.StatusCode != 200 {
		t.Errorf("jwt: status %d", resp.StatusCode)
	}
}
