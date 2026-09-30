// Package fakeoidc is a minimal OpenID Connect provider for tests. Its
// authorize endpoint signs in whoever is set as User without a login page.
package fakeoidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Server struct {
	Issuer       string
	ClientID     string
	ClientSecret string

	mu    sync.Mutex
	user  map[string]any
	codes map[string]grant
	key   *rsa.PrivateKey
}

type grant struct {
	nonce, challenge, redirect string
	claims                     map[string]any
}

// New makes a provider; set Issuer to the URL it is served at.
func New(clientID, clientSecret string) *Server {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return &Server{ClientID: clientID, ClientSecret: clientSecret, key: key, codes: map[string]grant{},
		user: map[string]any{"sub": "user-1", "email": "ada@example.com", "email_verified": true, "name": "Ada Lovelace"}}
}

// SetUser sets the claims of whoever signs in next.
func (s *Server) SetUser(claims map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.user = claims
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                s.Issuer,
			"authorization_endpoint":                s.Issuer + "/authorize",
			"token_endpoint":                        s.Issuer + "/token",
			"jwks_uri":                              s.Issuer + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(s.key.N.Bytes()), "e": b64(big.NewInt(int64(s.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET /authorize", s.authorize)
	mux.HandleFunc("POST /token", s.token)
	return mux
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	code := randomString()
	s.mu.Lock()
	s.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), claims: s.user}
	s.mu.Unlock()
	to, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := to.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	to.RawQuery = v.Encode()
	http.Redirect(w, r, to.String(), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id != s.ClientID || secret != s.ClientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	g, ok := s.codes[r.PostFormValue("code")]
	delete(s.codes, r.PostFormValue("code"))
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if !ok || g.redirect != r.PostFormValue("redirect_uri") || b64(sum[:]) != g.challenge {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": s.Issuer, "aud": s.ClientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": g.nonce}
	for k, v := range g.claims {
		claims[k] = v
	}
	writeJSON(w, map[string]any{"access_token": randomString(), "token_type": "Bearer", "expires_in": 3600, "id_token": s.sign(claims)})
}

// AccessToken signs a JWT with the given claims, adding iss, iat and a
// one-hour exp unless the claims set them.
func (s *Server) AccessToken(claims map[string]any) string {
	now := time.Now()
	c := map[string]any{"iss": s.Issuer, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	return s.sign(c)
}

func (s *Server) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(claims)
	input := b64(header) + "." + b64(body)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return input + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b64(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
