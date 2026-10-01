// Package adminauth decides who may use the admin API and the dashboard.
//
// People sign in through an OpenID Connect provider (authorization code flow
// with PKCE) and get a signed session cookie. A static break-glass token is
// also accepted, as a bearer token for scripts or exchanged for a session in
// the dashboard, when one is configured.
package adminauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/identity"
)

const (
	sessionCookie = "up_session"
	loginCookie   = "up_login"
	loginTTL      = 10 * time.Minute

	// CSRFHeader must be sent with cookie-authenticated requests that change
	// something. Browsers only let same-origin pages set custom headers.
	CSRFHeader = "X-Up-Admin"

	MethodOIDC  = "oidc"
	MethodToken = "token"
)

// Principal is whoever made an admin request, and what they may do.
type Principal struct {
	Email  string  `json:"email,omitempty"`
	Name   string  `json:"name,omitempty"`
	Method string  `json:"method"`
	Grants []Grant `json:"grants"`
}

// Grant is a role over every tenant (Tenants nil) or over the named ones.
type Grant struct {
	Role    string   `json:"role"`
	Tenants []string `json:"tenants,omitempty"`
}

var fullAdmin = []Grant{{Role: config.RoleAdmin}}

// IsAdmin reports whether p may change anything, tenants and prices included.
func (p *Principal) IsAdmin() bool {
	return slices.ContainsFunc(p.Grants, func(g Grant) bool { return g.Role == config.RoleAdmin && g.Tenants == nil })
}

// ReadsAll reports whether p may read every tenant, and the settings no
// tenant owns such as the audit log.
func (p *Principal) ReadsAll() bool {
	return slices.ContainsFunc(p.Grants, func(g Grant) bool { return g.Tenants == nil })
}

// CanRead reports whether p may see the named tenant.
func (p *Principal) CanRead(tenant string) bool {
	return slices.ContainsFunc(p.Grants, func(g Grant) bool { return g.Tenants == nil || slices.Contains(g.Tenants, tenant) })
}

// CanWrite reports whether p may change the named tenant's applications,
// keys and limits.
func (p *Principal) CanWrite(tenant string) bool {
	return slices.ContainsFunc(p.Grants, func(g Grant) bool {
		return g.Role == config.RoleAdmin && (g.Tenants == nil || slices.Contains(g.Tenants, tenant))
	})
}

// describe is a principal as the dashboard sees it, with its role summed up.
func describe(p *Principal) any {
	return struct {
		*Principal
		Role     string `json:"role"`
		IsAdmin  bool   `json:"is_admin"`
		ReadsAll bool   `json:"reads_all"`
	}{p, p.Role(), p.IsAdmin(), p.ReadsAll()}
}

// Role sums the grants up for display: admin, viewer, or either one for
// some tenants.
func (p *Principal) Role() string {
	switch {
	case p.IsAdmin():
		return "admin"
	case slices.ContainsFunc(p.Grants, func(g Grant) bool { return g.Role == config.RoleAdmin }):
		return "tenant admin"
	case p.ReadsAll():
		return "viewer"
	}
	return "tenant viewer"
}

type Auth struct {
	cfg    config.Admin
	secret []byte
	log    *slog.Logger
	// httpClient talks to the identity provider.
	httpClient *http.Client
	now        func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider

	// OnSignIn, when set, is told about every sign-in, and about people the
	// identity provider vouched for but the allow lists refused.
	OnSignIn func(p Principal, allowed bool)
}

func (a *Auth) signedIn(p Principal, allowed bool) {
	if a.OnSignIn != nil {
		a.OnSignIn(p, allowed)
	}
}

func New(cfg config.Admin, log *slog.Logger) *Auth {
	a := &Auth{cfg: cfg, log: log, httpClient: &http.Client{Timeout: 15 * time.Second}, now: time.Now}
	if cfg.SessionSecret != "" {
		a.secret = []byte(cfg.SessionSecret)
	} else {
		a.secret = make([]byte, 32)
		_, _ = rand.Read(a.secret)
		if cfg.OIDC.Enabled() {
			log.Warn("admin.session_secret is empty; dashboard sessions end when the proxy restarts")
		}
	}
	return a
}

// Enabled reports whether any sign-in method is configured.
func (a *Auth) Enabled() bool { return a.cfg.Token != "" || a.cfg.OIDC.Enabled() }

// Authenticate returns who made the request, or nil.
func (a *Auth) Authenticate(r *http.Request) *Principal {
	if tok := identity.BearerToken(r); tok != "" {
		if a.tokenOK(tok) {
			return &Principal{Method: MethodToken, Grants: fullAdmin}
		}
		return nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var s session
	if !a.open("session", c.Value, &s) || a.now().Unix() > s.Expires {
		return nil
	}
	switch s.Method {
	case MethodToken:
		// A token session outlives its token only until the token changes.
		if a.cfg.Token == "" || s.TokenMAC != a.tokenMAC() {
			return nil
		}
	case MethodOIDC:
	default:
		return nil
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(CSRFHeader) == "" {
		return nil
	}
	p := &Principal{Email: s.Email, Name: s.Name, Method: s.Method, Grants: fullAdmin}
	if s.Method == MethodOIDC {
		// Roles are worked out again on every request, so a change to the
		// allow lists or roles applies to sessions already open.
		if p.Grants = a.grants(s.Email, s.Groups); p.Grants == nil || !a.cfg.OIDC.Enabled() {
			return nil
		}
	}
	return p
}

func (a *Auth) tokenOK(tok string) bool {
	return a.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) == 1
}

// tokenMAC ties token sessions to the current token without storing it.
func (a *Auth) tokenMAC() string { return a.mac("token", []byte(a.cfg.Token))[:16] }

// grants applies the allow lists and roles: nil when nothing matches. A
// full admin needs no other grant.
func (a *Auth) grants(email string, groups []string) []Grant {
	o := a.cfg.OIDC
	if matches(email, groups, o.AllowedEmails, o.AllowedDomains, o.AllowedGroups) {
		return fullAdmin
	}
	var out []Grant
	for _, r := range o.Roles {
		if matches(email, groups, r.Emails, r.Domains, r.Groups) {
			out = append(out, Grant{Role: r.Role, Tenants: r.Tenants})
		}
	}
	return out
}

// matches reports whether someone is on any of the lists.
func matches(email string, groups, emails, domains, groupList []string) bool {
	email = strings.ToLower(email)
	if email != "" {
		if slices.Contains(emails, email) {
			return true
		}
		if _, domain, ok := strings.Cut(email, "@"); ok && slices.Contains(domains, domain) {
			return true
		}
	}
	return slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(groupList, g) })
}

// knownGroup reports whether any allow list or role names the group.
func (a *Auth) knownGroup(g string) bool {
	o := a.cfg.OIDC
	return slices.Contains(o.AllowedGroups, g) || slices.ContainsFunc(o.Roles, func(r config.RoleGrant) bool { return slices.Contains(r.Groups, g) })
}

// Handler serves /admin/auth/*.
func (a *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/auth/config", a.handleConfig)
	mux.HandleFunc("GET /admin/auth/me", a.handleMe)
	mux.HandleFunc("GET /admin/auth/login", a.handleLogin)
	mux.HandleFunc("GET /admin/auth/callback", a.handleCallback)
	mux.HandleFunc("POST /admin/auth/token", a.handleToken)
	mux.HandleFunc("POST /admin/auth/logout", a.handleLogout)
	return mux
}

// handleConfig tells the sign-in page which methods to offer.
func (a *Auth) handleConfig(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"token": a.cfg.Token != ""}
	if a.cfg.OIDC.Enabled() {
		out["oidc"] = map[string]string{"name": a.cfg.OIDC.DisplayName}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Auth) handleMe(w http.ResponseWriter, r *http.Request) {
	p := a.Authenticate(r)
	if p == nil {
		writeError(w, http.StatusUnauthorized, "not_signed_in", "Not signed in.")
		return
	}
	writeJSON(w, http.StatusOK, describe(p))
}

// handleToken exchanges the break-glass token for a session cookie, so the
// dashboard never keeps the token itself.
func (a *Auth) handleToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || !a.tokenOK(strings.TrimSpace(body.Token)) {
		// Not recorded: anyone can send a wrong token, as often as they like.
		a.log.Warn("admin sign-in refused", "method", MethodToken, "remote", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "invalid_admin_token", "Missing or invalid admin token.")
		return
	}
	a.log.Info("admin sign-in", "method", MethodToken, "remote", r.RemoteAddr)
	a.signedIn(Principal{Method: MethodToken, Grants: fullAdmin}, true)
	a.setSession(w, r, session{Method: MethodToken, TokenMAC: a.tokenMAC()})
	writeJSON(w, http.StatusOK, describe(&Principal{Method: MethodToken, Grants: fullAdmin}))
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// handleLogin starts the authorization code flow.
func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.OIDC.Enabled() {
		http.NotFound(w, r)
		return
	}
	oc, _, err := a.oauth(r.Context())
	if err != nil {
		a.log.Error("oidc discovery failed", "issuer", a.cfg.OIDC.Issuer, "err", err)
		redirectError(w, r, "provider_unavailable")
		return
	}
	st := login{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Expires:  a.now().Add(loginTTL).Unix(),
	}
	// Lax, not Strict: the provider's redirect back is a cross-site navigation.
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: a.seal("login", st), Path: "/admin/auth/",
		MaxAge: int(loginTTL.Seconds()), HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oc.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.OIDC.Enabled() {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: "", Path: "/admin/auth/", MaxAge: -1, HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	var st login
	c, err := r.Cookie(loginCookie)
	if err != nil || !a.open("login", c.Value, &st) || a.now().Unix() > st.Expires ||
		subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		redirectError(w, r, "login_expired")
		return
	}
	if e := q.Get("error"); e != "" {
		a.log.Warn("oidc provider returned an error", "error", e, "description", q.Get("error_description"))
		redirectError(w, r, "provider_error")
		return
	}
	oc, verifier, err := a.oauth(r.Context())
	if err != nil {
		a.log.Error("oidc discovery failed", "issuer", a.cfg.OIDC.Issuer, "err", err)
		redirectError(w, r, "provider_unavailable")
		return
	}
	ctx := oidc.ClientContext(r.Context(), a.httpClient)
	tok, err := oc.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		a.log.Warn("oidc code exchange failed", "err", err)
		redirectError(w, r, "provider_error")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(ctx, raw)
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		a.log.Warn("oidc id token rejected", "err", err)
		redirectError(w, r, "provider_error")
		return
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		redirectError(w, r, "provider_error")
		return
	}
	email, _ := claims["email"].(string)
	name, _ := claims["name"].(string)
	groups := stringList(claims[a.cfg.OIDC.GroupsClaim])
	// Some providers leave email_verified out; an explicit false is refused.
	if v, ok := claims["email_verified"].(bool); ok && !v {
		email = ""
	}
	if a.grants(email, groups) == nil {
		a.log.Warn("admin sign-in refused", "method", MethodOIDC, "email", email, "subject", idt.Subject)
		a.signedIn(Principal{Method: MethodOIDC, Email: email, Name: name}, false)
		redirectError(w, r, "not_allowed")
		return
	}
	a.log.Info("admin sign-in", "method", MethodOIDC, "email", email, "subject", idt.Subject)
	a.signedIn(Principal{Method: MethodOIDC, Email: email, Name: name, Grants: a.grants(email, groups)}, true)
	// Keep only the groups that matter, so the cookie stays small.
	groups = slices.DeleteFunc(groups, func(g string) bool { return !a.knownGroup(g) })
	a.setSession(w, r, session{Method: MethodOIDC, Email: email, Name: name, Groups: groups})
	http.Redirect(w, r, st.Next, http.StatusFound)
}

// oauth discovers the provider on first use, so the proxy still starts and
// serves traffic while the provider is unreachable.
func (a *Auth) oauth(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider == nil {
		p, err := oidc.NewProvider(oidc.ClientContext(ctx, a.httpClient), a.cfg.OIDC.Issuer)
		if err != nil {
			return nil, nil, err
		}
		a.provider = p
	}
	o := a.cfg.OIDC
	oc := &oauth2.Config{
		ClientID: o.ClientID, ClientSecret: o.ClientSecret, RedirectURL: o.RedirectURL,
		Endpoint: a.provider.Endpoint(), Scopes: o.Scopes,
	}
	// The verifier fetches signing keys with its own background context, so
	// give it the client there too.
	kctx := oidc.ClientContext(context.Background(), a.httpClient)
	return oc, a.provider.VerifierContext(kctx, &oidc.Config{ClientID: o.ClientID, Now: a.now}), nil
}

type session struct {
	Method   string   `json:"m"`
	Email    string   `json:"e,omitempty"`
	Name     string   `json:"n,omitempty"`
	Groups   []string `json:"g,omitempty"`
	TokenMAC string   `json:"t,omitempty"`
	Expires  int64    `json:"x"`
}

type login struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"r"`
	Expires  int64  `json:"x"`
}

func (a *Auth) setSession(w http.ResponseWriter, r *http.Request, s session) {
	s.Expires = a.now().Add(a.cfg.SessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: a.seal("session", s), Path: "/",
		MaxAge: int(a.cfg.SessionTTL.Seconds()), HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteStrictMode,
	})
}

// secure reports whether cookies should be HTTPS-only.
func (a *Auth) secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || strings.HasPrefix(a.cfg.OIDC.RedirectURL, "https://")
}

// seal signs v as base64(json).base64(hmac). purpose keeps one kind of
// cookie from being replayed as another.
func (a *Auth) seal(purpose string, v any) string {
	body, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + a.mac(purpose, []byte(payload))
}

func (a *Auth) open(purpose, sealed string, v any) bool {
	payload, sig, ok := strings.Cut(sealed, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.mac(purpose, []byte(payload)))) {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	return err == nil && json.Unmarshal(body, v) == nil
}

func (a *Auth) mac(purpose string, data []byte) string {
	h := hmac.New(sha256.New, a.secret)
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write(data)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// redirectError sends the browser back to the sign-in page with a reason it
// can show.
func redirectError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?login_error="+code, http.StatusFound)
}

func stringList(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": code, "message": msg}})
}
