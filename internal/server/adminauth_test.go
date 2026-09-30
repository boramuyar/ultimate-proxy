package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/fakeoidc"
	"github.com/boramuyar/ultimate-proxy/internal/meter"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

type authHarness struct {
	t     *testing.T
	proxy *httptest.Server
	idp   *fakeoidc.Server
}

// newAuthHarness runs the proxy with OIDC sign-in against a fake provider.
// extra is appended to the admin: section of the config.
func newAuthHarness(t *testing.T, extra string) *authHarness {
	t.Helper()
	idp := fakeoidc.New("proxy", "s3cret")
	idpSrv := httptest.NewServer(idp.Handler())
	t.Cleanup(idpSrv.Close)
	idp.Issuer = idpSrv.URL

	proxy := httptest.NewUnstartedServer(nil)
	base := "http://" + proxy.Listener.Addr().String()
	cfg, err := config.Parse([]byte(`
admin:
  token: break-glass
  session_secret: test-secret
  oidc:
    issuer: ` + idpSrv.URL + `
    client_id: proxy
    client_secret: s3cret
    redirect_url: ` + base + `/admin/auth/callback
    display_name: Example SSO
    allowed_emails: ["ada@example.com, grace@example.com"]
    allowed_groups: [proxy-admins]
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, st, meter.New(st, 100, 10, 10*time.Millisecond, log), log)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Config.Handler = srv.Handler()
	proxy.Start()
	t.Cleanup(proxy.Close)
	return &authHarness{t: t, proxy: proxy, idp: idp}
}

func newBrowser(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// signIn runs the whole browser flow and returns where it ended up.
func (h *authHarness) signIn(c *http.Client, next string) *url.URL {
	h.t.Helper()
	// The flow ends on the dashboard, which this test server doesn't serve.
	resp, err := c.Get(h.proxy.URL + "/admin/auth/login?next=" + url.QueryEscape(next))
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.Request.URL
}

func (h *authHarness) do(c *http.Client, method, path, body string, headers ...string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.proxy.URL+path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func TestOIDCSignIn(t *testing.T) {
	h := newAuthHarness(t, "")
	c := newBrowser(t)

	if resp := h.do(c, "GET", "/admin/tenants", ""); resp.StatusCode != 401 {
		t.Fatalf("before sign-in: %d", resp.StatusCode)
	}
	end := h.signIn(c, "/usage?x=1")
	if end.Path != "/usage" || end.Query().Get("login_error") != "" {
		t.Fatalf("ended at %s", end)
	}
	me := decode(t, h.do(c, "GET", "/admin/auth/me", ""))
	if me["email"] != "ada@example.com" || me["name"] != "Ada Lovelace" || me["method"] != "oidc" {
		t.Fatalf("me: %v", me)
	}
	if resp := h.do(c, "GET", "/admin/tenants", ""); resp.StatusCode != 200 {
		t.Fatalf("after sign-in: %d", resp.StatusCode)
	}

	// Writes need the CSRF header on top of the cookie.
	if resp := h.do(c, "POST", "/admin/tenants", `{"name":"acme"}`); resp.StatusCode != 401 {
		t.Fatalf("write without CSRF header: %d", resp.StatusCode)
	}
	if resp := h.do(c, "POST", "/admin/tenants", `{"name":"acme"}`, "X-Up-Admin", "1"); resp.StatusCode != 201 {
		t.Fatalf("write with CSRF header: %d", resp.StatusCode)
	}

	if resp := h.do(c, "POST", "/admin/auth/logout", "", "X-Up-Admin", "1"); resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp := h.do(c, "GET", "/admin/tenants", ""); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

func TestOIDCAllowLists(t *testing.T) {
	h := newAuthHarness(t, "    allowed_domains: [corp.example]\n")
	cases := []struct {
		name    string
		claims  map[string]any
		allowed bool
	}{
		{"listed email, any case", map[string]any{"sub": "1", "email": "Grace@Example.com"}, true},
		{"unlisted email", map[string]any{"sub": "2", "email": "eve@example.com", "email_verified": true}, false},
		{"allowed domain", map[string]any{"sub": "3", "email": "bob@corp.example", "email_verified": true}, true},
		{"allowed domain, unverified", map[string]any{"sub": "4", "email": "bob@corp.example", "email_verified": false}, false},
		{"lookalike domain", map[string]any{"sub": "5", "email": "bob@evilcorp.example", "email_verified": true}, false},
		{"allowed group", map[string]any{"sub": "6", "email": "eve@example.com", "groups": []string{"staff", "proxy-admins"}}, true},
		{"single group string", map[string]any{"sub": "7", "groups": "proxy-admins"}, true},
		{"other groups", map[string]any{"sub": "8", "groups": []string{"staff"}}, false},
		{"no email or groups", map[string]any{"sub": "9"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.idp.SetUser(tc.claims)
			c := newBrowser(t)
			end := h.signIn(c, "/")
			resp := h.do(c, "GET", "/admin/tenants", "")
			if tc.allowed != (resp.StatusCode == 200) {
				t.Fatalf("status %d, ended at %s", resp.StatusCode, end)
			}
			if !tc.allowed && end.Query().Get("login_error") != "not_allowed" {
				t.Fatalf("ended at %s", end)
			}
		})
	}
}

func TestOIDCCallbackRejectsForgedState(t *testing.T) {
	h := newAuthHarness(t, "")
	c := newBrowser(t)
	// A callback nobody started, as in a login CSRF attempt.
	resp := h.do(c, "GET", "/admin/auth/callback?code=x&state=y", "")
	if got := resp.Request.URL.Query().Get("login_error"); got != "login_expired" {
		t.Fatalf("login_error %q", got)
	}
	if resp := h.do(c, "GET", "/admin/auth/me", ""); resp.StatusCode != 401 {
		t.Fatalf("me: %d", resp.StatusCode)
	}
}

func TestSessionCookieTampering(t *testing.T) {
	h := newAuthHarness(t, "")
	c := newBrowser(t)
	h.signIn(c, "/")
	u, _ := url.Parse(h.proxy.URL)
	var session *http.Cookie
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "up_session" {
			session = ck
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}
	payload, sig, _ := strings.Cut(session.Value, ".")
	// Same session, claiming a different email.
	forged := strings.Replace(payload, "ZGFA", "ZWFA", 1) + "." + sig
	if forged == session.Value {
		t.Fatal("payload not changed")
	}
	for _, v := range []string{forged, payload, "", session.Value + "x"} {
		req, _ := http.NewRequest("GET", h.proxy.URL+"/admin/tenants", nil)
		req.AddCookie(&http.Cookie{Name: "up_session", Value: v})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 401 {
			t.Fatalf("cookie %q: %d", v, resp.StatusCode)
		}
	}
}

func TestBreakGlassToken(t *testing.T) {
	h := newAuthHarness(t, "")
	c := newBrowser(t)
	cfg := decode(t, h.do(c, "GET", "/admin/auth/config", ""))
	if cfg["token"] != true || cfg["oidc"].(map[string]any)["name"] != "Example SSO" {
		t.Fatalf("config: %v", cfg)
	}
	// As a bearer token, for scripts.
	if resp := h.do(http.DefaultClient, "GET", "/admin/tenants", "", "Authorization", "Bearer break-glass"); resp.StatusCode != 200 {
		t.Fatalf("bearer: %d", resp.StatusCode)
	}
	if resp := h.do(http.DefaultClient, "GET", "/admin/tenants", "", "Authorization", "Bearer nope"); resp.StatusCode != 401 {
		t.Fatalf("wrong bearer: %d", resp.StatusCode)
	}
	// Exchanged for a session, in the dashboard.
	if resp := h.do(c, "POST", "/admin/auth/token", `{"token":"nope"}`); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	if resp := h.do(c, "POST", "/admin/auth/token", `{"token":"break-glass"}`); resp.StatusCode != 200 {
		t.Fatalf("token: %d", resp.StatusCode)
	}
	if me := decode(t, h.do(c, "GET", "/admin/auth/me", "")); me["method"] != "token" {
		t.Fatalf("me: %v", me)
	}
}

func TestBreakGlassTokenDisabled(t *testing.T) {
	cfg, err := config.Parse([]byte("admin:\n  token: \"\"\n"))
	if err != nil || cfg.Admin.Token != "" {
		t.Fatalf("parse: %v %q", err, cfg.Admin.Token)
	}
	st := store.NewMemory()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, st, meter.New(st, 100, 10, 10*time.Millisecond, log), log)
	if err != nil {
		t.Fatal(err)
	}
	p := httptest.NewServer(srv.Handler())
	defer p.Close()
	for _, auth := range []string{"Bearer ", "Bearer x", ""} {
		req, _ := http.NewRequest("GET", p.URL+"/admin/tenants", nil)
		req.Header.Set("Authorization", auth)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != 401 {
			t.Fatalf("%q: %d", auth, resp.StatusCode)
		}
	}
	resp, _ := http.Post(p.URL+"/admin/auth/token", "application/json", strings.NewReader(`{"token":""}`))
	if resp.StatusCode != 401 {
		t.Fatalf("empty token exchange: %d", resp.StatusCode)
	}
}

func TestOIDCConfigValidation(t *testing.T) {
	base := "admin:\n  oidc:\n    issuer: https://idp.example\n    client_id: x\n"
	for name, tc := range map[string]string{
		"no allow list":     base + "    redirect_url: https://proxy.example/admin/auth/callback\n",
		"no redirect":       base + "    allowed_domains: [example.com]\n",
		"wrong path":        base + "    redirect_url: https://proxy.example/callback\n    allowed_domains: [example.com]\n",
		"empty from env":    base + "    redirect_url: https://proxy.example/admin/auth/callback\n    allowed_emails: [\"\", \" , \"]\n",
		"relative redirect": base + "    redirect_url: /admin/auth/callback\n    allowed_domains: [example.com]\n",
	} {
		if _, err := config.Parse([]byte(tc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	cfg, err := config.Parse([]byte(base + "    redirect_url: https://proxy.example/admin/auth/callback\n    allowed_domains: [\"Example.com, corp.example\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Admin.OIDC.AllowedDomains, " "); got != "example.com corp.example" {
		t.Fatalf("domains %q", got)
	}
	// The old top-level admin_token still works.
	cfg, _ = config.Parse([]byte("admin_token: old\n"))
	if cfg.Admin.Token != "old" {
		t.Fatalf("admin_token not carried over: %q", cfg.Admin.Token)
	}
}
