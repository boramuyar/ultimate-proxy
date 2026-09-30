package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// jwksMinRefresh is the least time between two fetches of an issuer's keys.
// The oidc package's own key set refetches on every bad signature, which
// would let anyone make the proxy hammer the identity provider.
const jwksMinRefresh = 30 * time.Second

var joseAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512, jose.EdDSA,
}

// keySet is an issuer's signing keys, fetched on first use and again only
// when a token names a key ID it does not have, at most every
// jwksMinRefresh.
type keySet struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	keys    []jose.JSONWebKey
	fetched time.Time
}

func newKeySet(url string) *keySet {
	return &keySet{url: url, client: &http.Client{Timeout: 10 * time.Second}}
}

// VerifySignature implements oidc.KeySet.
func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, joseAlgs)
	if err != nil {
		return nil, err
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("want exactly one signature")
	}
	kid := jws.Signatures[0].Header.KeyID

	keys, err := k.get(ctx, kid)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if kid == "" || keys[i].KeyID == kid {
			if payload, err := jws.Verify(&keys[i]); err == nil {
				return payload, nil
			}
		}
	}
	return nil, errors.New("signature does not match the issuer's keys")
}

// get returns the keys, refreshing them first when kid is unknown and the
// last fetch is old enough.
func (k *keySet) get(ctx context.Context, kid string) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	known := k.keys != nil && (kid == "" || hasKey(k.keys, kid))
	if known || time.Since(k.fetched) < jwksMinRefresh {
		if k.keys == nil {
			return nil, errors.New("issuer keys unavailable")
		}
		return k.keys, nil
	}
	k.fetched = time.Now()
	keys, err := k.fetch(ctx)
	if err != nil {
		if k.keys != nil {
			return k.keys, nil // keep using the keys we have
		}
		return nil, err
	}
	k.keys = keys
	return keys, nil
}

func (k *keySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", k.url, resp.Status)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("fetching %s: %w", k.url, err)
	}
	var keys []jose.JSONWebKey
	for _, key := range set.Keys {
		if key.Use == "" || key.Use == "sig" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func hasKey(keys []jose.JSONWebKey, kid string) bool {
	for _, k := range keys {
		if k.KeyID == kid {
			return true
		}
	}
	return false
}
