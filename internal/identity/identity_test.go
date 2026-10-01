package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/omni-proxy/omni-proxy/internal/openresponses"
	"github.com/omni-proxy/omni-proxy/internal/store"
)

// stub accepts tokens with a given prefix.
type stub struct {
	prefix string
	id     *Identity
}

func (s stub) Accepts(t string) bool { return len(t) >= len(s.prefix) && t[:len(s.prefix)] == s.prefix }
func (s stub) Authenticate(context.Context, string) (*Identity, *openresponses.APIError) {
	return s.id, nil
}

func request(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestChainFirstAcceptingAuthenticatorDecides(t *testing.T) {
	a := &Identity{Method: "a"}
	b := &Identity{Method: "b"}
	c := Chain{stub{"x.", a}, stub{"", b}}
	for token, want := range map[string]string{"x.token": "a", "other": "b"} {
		id, err := c.Authenticate(context.Background(), request(token))
		if err != nil || id.Method != want {
			t.Errorf("%q: got %v, %v; want method %q", token, id, err, want)
		}
	}
	if _, err := c.Authenticate(context.Background(), request("")); err == nil || err.Status != http.StatusUnauthorized {
		t.Errorf("missing token: got %v, want 401", err)
	}
	if _, err := (Chain{stub{"x.", a}}).Authenticate(context.Background(), request("nope")); err == nil {
		t.Error("a token no authenticator accepts must be refused")
	}
}

func TestAPIKeys(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	ten, _ := st.EnsureTenant(ctx, "acme")
	app, _ := st.EnsureApplication(ctx, ten.ID, "bot", true)
	key, hash := NewKey()
	k, _ := st.CreateKey(ctx, app.ID, hash, DisplayPrefix(key))

	a := NewAPIKeys(st)
	id, err := Chain{a}.Authenticate(ctx, request(key))
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Method: MethodAPIKey, Subject: k.ID, TenantID: ten.ID, TenantName: "acme", AppID: app.ID, AppName: "bot", CanAssertUsers: true}
	if !reflect.DeepEqual(*id, want) {
		t.Fatalf("got %+v, want %+v", *id, want)
	}
	if id.KeyID() != k.ID {
		t.Errorf("KeyID() = %q, want %q", id.KeyID(), k.ID)
	}

	// Revocation shows once the cache entry is dropped.
	if err := st.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, key); err != nil {
		t.Fatal("cached key should still authenticate before Forget")
	}
	a.ForgetAll()
	if _, err := a.Authenticate(ctx, key); err == nil {
		t.Fatal("revoked key authenticated")
	}
}

func TestResolveUser(t *testing.T) {
	env, apiErr := openresponses.ParseEnvelope([]byte(`{"model":"m","metadata":{"user_email":"Meta@Example.com"}}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	r := request("k")
	r.Header.Set(UserHeader, "Header@Example.com")

	cases := []struct {
		id                  Identity
		wantEmail, wantFrom string
	}{
		{Identity{Method: "jwt", User: "Token@Example.com", CanAssertUsers: true}, "token@example.com", "jwt"},
		{Identity{Method: MethodAPIKey, CanAssertUsers: true}, "header@example.com", "header"},
		{Identity{Method: MethodAPIKey}, "", "none"},
	}
	for _, c := range cases {
		email, from := ResolveUser(&c.id, r, env)
		if email != c.wantEmail || from != c.wantFrom {
			t.Errorf("%+v: got %q from %q, want %q from %q", c.id, email, from, c.wantEmail, c.wantFrom)
		}
	}
}
