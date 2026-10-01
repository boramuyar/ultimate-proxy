package identity

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/omni-proxy/omni-proxy/internal/fakeoidc"
)

func TestKeySetRefetchesAtMostEveryInterval(t *testing.T) {
	idp := fakeoidc.New("c", "s")
	var hits atomic.Int32
	h := idp.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jwks" {
			hits.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	idp.Issuer = srv.URL

	ks := newKeySet(srv.URL + "/jwks")
	good := idp.AccessToken(map[string]any{"sub": "u"})
	if _, err := ks.VerifySignature(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	for i := 0; i < 50; i++ {
		// Unknown key IDs and bad signatures with a known one.
		forged := b64([]byte(`{"alg":"RS256","kid":"unknown`+string(rune('a'+i%26))+`"}`)) + "." + b64([]byte(`{"sub":"x"}`)) + "." + b64([]byte("sig"))
		if _, err := ks.VerifySignature(context.Background(), forged); err == nil {
			t.Fatal("forged token verified")
		}
		if _, err := ks.VerifySignature(context.Background(), good[:len(good)-4]+"AAAA"); err == nil {
			t.Fatal("tampered token verified")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("fetched the key set %d times, want 1", n)
	}
	if _, err := ks.VerifySignature(context.Background(), good); err != nil {
		t.Fatalf("good token after forgeries: %v", err)
	}
}
