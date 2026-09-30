package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestKeyPolicy(t *testing.T) {
	h := newHarness(t)
	resp := h.admin(http.MethodGet, "/admin/applications", "")
	var apps struct {
		Data []struct{ ID, Name string }
	}
	_ = json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()
	appID := apps.Data[0].ID

	resp = h.admin(http.MethodPost, "/admin/applications/"+appID+"/keys", `{"allowed_models":["gpt"],"expires_in":3600}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, decode(t, resp))
	}
	created := decode(t, resp)
	key, id := created["key"].(string), created["id"].(string)
	if exp, _ := time.Parse(time.RFC3339, created["expires_at"].(string)); time.Until(exp) < 59*time.Minute {
		t.Errorf("expires_at %v, want about an hour from now", created["expires_at"])
	}

	status := func(model string) int {
		r := h.post(key, `{"model":"`+model+`","input":"hi"}`)
		r.Body.Close()
		return r.StatusCode
	}
	if s := status("gpt"); s != 200 {
		t.Errorf("allowed model: %d", s)
	}
	if s := status("openai/gpt-fake"); s != http.StatusForbidden {
		t.Errorf("other model: %d, want 403", s)
	}

	// Lifting the allowlist applies at once.
	if r := h.admin(http.MethodPatch, "/admin/keys/"+id, `{"allowed_models":null}`); r.StatusCode != 200 {
		t.Fatalf("patch: %d", r.StatusCode)
	}
	if s := status("openai/gpt-fake"); s != 200 {
		t.Errorf("after lifting the allowlist: %d", s)
	}

	// Expiring it applies at once, with its own error code, and keeps the
	// allowlist change.
	r := h.admin(http.MethodPatch, "/admin/keys/"+id, `{"expires_at":"2020-01-01T00:00:00Z"}`)
	if body := decode(t, r); body["allowed_models"] != nil {
		t.Errorf("patching expiry changed allowed_models: %v", body)
	}
	r = h.post(key, `{"model":"gpt","input":"hi"}`)
	if body := decode(t, r); r.StatusCode != http.StatusUnauthorized || body["error"].(map[string]any)["code"] != "api_key_expired" {
		t.Errorf("expired key: %d %v", r.StatusCode, body)
	}

	for body, want := range map[string]int{
		`{"expires_in":-5}`:                       400,
		`{"expires_at":"soon"}`:                   400,
		`{"expires_in":5,"expires_at":null}`:      400,
		`{"allowed_models":"gpt"}`:                400,
		`{"expires_at":null,"allowed_models":[]}`: 200,
	} {
		if r := h.admin(http.MethodPatch, "/admin/keys/"+id, body); r.StatusCode != want {
			t.Errorf("patch %s: %d, want %d", body, r.StatusCode, want)
		}
	}
	if r := h.admin(http.MethodPatch, "/admin/keys/key_missing", `{}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("missing key: %d", r.StatusCode)
	}
	// An empty allowlist allows nothing.
	if s := status("gpt"); s != http.StatusForbidden {
		t.Errorf("empty allowlist: %d, want 403", s)
	}
}

func TestKeyStopsWorkingOnTime(t *testing.T) {
	h := newHarness(t)
	resp := h.admin(http.MethodGet, "/admin/applications", "")
	var apps struct{ Data []struct{ ID string } }
	_ = json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()
	resp = h.admin(http.MethodPost, "/admin/applications/"+apps.Data[0].ID+"/keys", `{"expires_in":1}`)
	key := decode(t, resp)["key"].(string)
	if r := h.post(key, `{"model":"gpt","input":"hi"}`); r.StatusCode != 200 {
		t.Fatalf("fresh key: %d", r.StatusCode)
	}
	time.Sleep(1100 * time.Millisecond)
	// The key is still in the 30 s auth cache, but the cache never outlives it.
	if r := h.post(key, `{"model":"gpt","input":"hi"}`); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired key: %d, want 401", r.StatusCode)
	}
}
