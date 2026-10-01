package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func (h *harness) mint(credential, body string) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.proxy.URL+"/v1/tokens", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, decode(h.t.(*testing.T), resp)
}

func TestMintedTokenFromAPIKey(t *testing.T) {
	h := newHarness(t)
	status, body := h.mint(trustedKey, `{"user":"Alice@Acme.com","models":["gpt"],"expires_in":120}`)
	if status != http.StatusCreated {
		t.Fatalf("mint: %d %v", status, body)
	}
	token := body["token"].(string)
	if !strings.HasPrefix(token, "opt_") || body["user"] != "alice@acme.com" {
		t.Fatalf("unexpected mint response %v", body)
	}
	exp, _ := time.Parse(time.RFC3339, body["expires_at"].(string))
	if d := time.Until(exp); d < 100*time.Second || d > 121*time.Second {
		t.Errorf("expires in %v, want about 120s", d)
	}

	// The token works for the granted model, as the named user; the user
	// can't be changed by the caller.
	resp := h.post(token, `{"model":"gpt","input":"hi"}`, "X-Proxy-User-Email", "mallory@acme.com")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, decode(t, resp))
	}
	resp.Body.Close()
	ev := h.events(1)
	if e := ev[0]; e.AuthMethod != "proxy_token" || e.Subject != body["id"] || e.UserEmail != "alice@acme.com" || e.KeyID != "" {
		t.Errorf("unexpected usage event %+v", e)
	}
	// Other models are refused.
	if resp := h.post(token, `{"model":"gpt-fake","input":"hi"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("other model: status %d, want 403", resp.StatusCode)
	}
	// A token cannot mint more tokens.
	if status, _ := h.mint(token, `{}`); status != http.StatusForbidden {
		t.Errorf("mint with a token: status %d, want 403", status)
	}
	// Revoking it stops it at once.
	if resp := h.admin(http.MethodDelete, "/admin/tokens/"+body["id"].(string), ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status %d", resp.StatusCode)
	}
	if resp := h.post(token, `{"model":"gpt","input":"hi"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: status %d, want 401", resp.StatusCode)
	}
}

func TestMintRules(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name, key, body string
		status          int
	}{
		{"untrusted app names a user", untrustedKey, `{"user":"bob@globex.com"}`, 403},
		{"untrusted app, no user", untrustedKey, `{}`, 201},
		{"too short", trustedKey, `{"expires_in":10}`, 400},
		{"too long", trustedKey, `{"expires_in":90000}`, 400},
		{"bad key", "op_nope", `{}`, 401},
	}
	for _, c := range cases {
		if status, body := h.mint(c.key, c.body); status != c.status {
			t.Errorf("%s: status %d (%v), want %d", c.name, status, body, c.status)
		}
	}
}

func TestMintedTokenFromJWT(t *testing.T) {
	h, idp := newJWTHarness(t, jwtAuth)
	exp := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	jwt := idp.AccessToken(map[string]any{
		"sub": "u-1", "aud": "omni-proxy", "org": "initech", "email": "peter@initech.com",
		"llm_models": []any{"gpt"}, "exp": exp.Unix(),
	})
	if status, _ := h.mint(jwt, `{"user":"someone@else.com"}`); status != http.StatusForbidden {
		t.Errorf("naming another user: status %d, want 403", status)
	}
	if status, _ := h.mint(jwt, `{"models":["openai/*"]}`); status != http.StatusForbidden {
		t.Errorf("widening models: status %d, want 403", status)
	}
	status, body := h.mint(jwt, `{"expires_in":3600}`)
	if status != http.StatusCreated {
		t.Fatalf("mint: %d %v", status, body)
	}
	got, _ := time.Parse(time.RFC3339, body["expires_at"].(string))
	if !got.Equal(exp) || body["user"] != "peter@initech.com" {
		t.Errorf("got expiry %v user %v; want the JWT's expiry %v and user", got, body["user"], exp)
	}
	if models := body["models"].([]any); len(models) != 1 || models[0] != "gpt" {
		t.Errorf("models %v, want the JWT's [gpt]", models)
	}
	resp := h.post(body["token"].(string), `{"model":"gpt","input":"hi"}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("using the minted token: status %d", resp.StatusCode)
	}
	if e := h.events(1)[0]; e.UserEmail != "peter@initech.com" || e.AuthMethod != "proxy_token" {
		t.Errorf("unexpected usage event %+v", e)
	}
}

func TestCORS(t *testing.T) {
	h := newHarnessWith(t, "auth: {cors_origins: [https://app.example.com]}\n")
	for origin, allowed := range map[string]bool{"https://app.example.com": true, "https://evil.example.com": false} {
		req, _ := http.NewRequest(http.MethodOptions, h.proxy.URL+"/openai/v1/responses", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := resp.Header.Get("Access-Control-Allow-Origin")
		if resp.StatusCode != http.StatusNoContent || (got == origin) != allowed {
			t.Errorf("%s: status %d, allow-origin %q", origin, resp.StatusCode, got)
		}
		if allowed && !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Errorf("preflight does not allow the Authorization header")
		}
	}
	resp := h.admin(http.MethodGet, "/admin/tenants", "")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS must not apply to /admin")
	}
}
