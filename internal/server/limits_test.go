package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// app returns the tenant and application IDs of the application named name.
func (h *harness) app(t *testing.T, name string) (tenantID, appID string) {
	t.Helper()
	resp := h.admin(http.MethodGet, "/admin/applications", "")
	var apps struct {
		Data []struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
			Name     string `json:"name"`
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()
	for _, a := range apps.Data {
		if a.Name == name {
			return a.TenantID, a.ID
		}
	}
	t.Fatalf("no application %s", name)
	return "", ""
}

func (h *harness) addLimit(t *testing.T, body string) map[string]any {
	t.Helper()
	resp := h.admin(http.MethodPost, "/admin/limits", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create limit: %d %v", resp.StatusCode, out)
	}
	return out
}

func TestRateLimitRefusesAndLogs(t *testing.T) {
	h := newHarness(t)
	tenant, app := h.app(t, "chat")
	h.addLimit(t, `{"tenant_id":"`+tenant+`","application_id":"`+app+`","kind":"rpm","amount":2}`)

	for i := range 2 {
		resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`)
		decode(t, resp)
		if resp.StatusCode != 200 || resp.Header.Get("X-Ratelimit-Limit-Requests") != "2" {
			t.Fatalf("request %d: %d, headers %v", i, resp.StatusCode, resp.Header)
		}
	}
	resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`)
	body := decode(t, resp)
	e := body["error"].(map[string]any)
	if resp.StatusCode != 429 || e["code"] != "rate_limit_exceeded" || !strings.Contains(e["message"].(string), "application") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d %v %v", resp.StatusCode, body, resp.Header)
	}
	evs := h.events(3)
	last := evs[2]
	if last.Status != "rejected" || last.HTTPStatus != 429 || last.InputTokens != 0 || last.Model != "gpt" {
		t.Errorf("rejected event %+v", last)
	}
	if len(h.upstream.Requests) != 2 {
		t.Errorf("upstream got %d requests", len(h.upstream.Requests))
	}
	// Other tenants are not affected.
	if resp := h.post(untrustedKey, `{"model":"gpt","input":"hi"}`); resp.StatusCode != 200 {
		t.Errorf("other tenant: %d", resp.StatusCode)
	}
}

func TestPerUserLimit(t *testing.T) {
	h := newHarness(t)
	tenant, app := h.app(t, "chat")
	h.addLimit(t, `{"tenant_id":"`+tenant+`","application_id":"`+app+`","user":"*","kind":"rpm","amount":1}`)
	as := func(email string) int {
		resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`, "X-Proxy-User-Email", email)
		decode(t, resp)
		return resp.StatusCode
	}
	if as("a@x.com") != 200 || as("b@x.com") != 200 || as("A@x.com") != 429 {
		t.Fatal("each user should get one request a minute")
	}
	resp := h.admin(http.MethodGet, "/admin/limits/status?user=a@x.com", "")
	st := decode(t, resp)["data"].([]any)[0].(map[string]any)
	if st["used"].(float64) < 1 {
		t.Errorf("status %v", st)
	}
}

func TestTokenLimitAfterUsage(t *testing.T) {
	h := newHarness(t)
	tenant, _ := h.app(t, "chat")
	h.addLimit(t, `{"tenant_id":"`+tenant+`","kind":"tpm","amount":10}`)
	resp := h.post(trustedKey, `{"model":"gpt","input":"a long enough prompt to use more than ten tokens in the fake upstream"}`)
	decode(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("first request: %d", resp.StatusCode)
	}
	// Tokens are charged just after the response.
	for range 100 {
		resp = h.post(trustedKey, `{"model":"gpt","input":"hi"}`)
		decode(t, resp)
		if resp.StatusCode == 429 {
			return
		}
	}
	t.Fatal("token limit never applied")
}

func TestLimitAdmin(t *testing.T) {
	h := newHarness(t)
	tenant, app := h.app(t, "chat")
	_, otherApp := h.app(t, "widget")
	for body, want := range map[string]int{
		`{"tenant_id":"` + tenant + `","kind":"rpm"}`:                                                       400,
		`{"tenant_id":"` + tenant + `","kind":"rph","amount":1}`:                                            400,
		`{"tenant_id":"` + tenant + `","kind":"rpm","amount":0}`:                                            400,
		`{"tenant_id":"nope","kind":"rpm","amount":1}`:                                                      400,
		`{"tenant_id":"` + tenant + `","application_id":"` + otherApp + `","kind":"rpm","amount":1}`:        400,
		`{"tenant_id":"` + tenant + `","kind":"rpm","amount":1,"enforcement":"soft"}`:                       400,
		`{"tenant_id":"` + tenant + `","kind":"rpm","amount":1,"period":"day"}`:                             400,
		`{"tenant_id":"` + tenant + `","kind":"budget_usd","amount":1}`:                                     400,
		`{"tenant_id":"` + tenant + `","kind":"budget_usd","amount":1,"period":"year"}`:                     400,
		`{"tenant_id":"` + tenant + `","kind":"budget_usd","amount":1,"period":"day","enforcement":"warn"}`: 400,
	} {
		resp := h.admin(http.MethodPost, "/admin/limits", body)
		if b := decode(t, resp); resp.StatusCode != want {
			t.Errorf("%s: %d %v", body, resp.StatusCode, b)
		}
	}
	l := h.addLimit(t, `{"tenant_id":"`+tenant+`","application_id":"`+app+`","kind":"rpm","amount":1}`)
	id := l["id"].(string)
	resp := h.admin(http.MethodPatch, "/admin/limits/"+id, `{"amount":5}`)
	if b := decode(t, resp); resp.StatusCode != 200 || b["amount"].(float64) != 5 {
		t.Fatalf("patch: %d %v", resp.StatusCode, b)
	}
	for range 5 {
		if resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`); resp.StatusCode != 200 {
			t.Fatalf("raised limit not applied: %d", resp.StatusCode)
		}
	}
	resp = h.admin(http.MethodGet, "/admin/limits?application_id="+app, "")
	if n := len(decode(t, resp)["data"].([]any)); n != 1 {
		t.Errorf("listed %d", n)
	}
	if resp := h.admin(http.MethodDelete, "/admin/limits/"+id, ""); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`); resp.StatusCode != 200 {
		t.Errorf("deleted limit still applied: %d", resp.StatusCode)
	}
}

func TestTokenBudget(t *testing.T) {
	h := newHarness(t)
	tenant, _ := h.app(t, "chat")
	l := h.addLimit(t, `{"tenant_id":"`+tenant+`","kind":"budget_tokens","amount":10,"period":"day"}`)
	resp := h.post(trustedKey, `{"model":"gpt","input":"a long enough prompt to use more than ten tokens in the fake upstream"}`)
	decode(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("first request: %d", resp.StatusCode)
	}
	var body map[string]any
	for range 100 {
		resp = h.post(trustedKey, `{"model":"gpt","input":"hi"}`)
		if body = decode(t, resp); resp.StatusCode == 429 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	e, _ := body["error"].(map[string]any)
	if resp.StatusCode != 429 || e["code"] != "budget_exceeded" || !strings.Contains(e["message"].(string), "per day") {
		t.Fatalf("budget never applied: %d %v", resp.StatusCode, body)
	}
	if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); ra <= 0 || ra > 86400 {
		t.Errorf("Retry-After %q should run to midnight UTC", resp.Header.Get("Retry-After"))
	}
	// The crossing opened an insight.
	resp = h.admin(http.MethodGet, "/admin/insights", "")
	ins := decode(t, resp)["data"].([]any)
	if len(ins) != 1 || ins[0].(map[string]any)["kind"] != "budget_threshold" || ins[0].(map[string]any)["severity"] != "critical" {
		t.Fatalf("insights %v", ins)
	}
	// Soft budgets warn but allow.
	resp = h.admin(http.MethodPatch, "/admin/limits/"+l["id"].(string), `{"enforcement":"soft"}`)
	decode(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	if resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`); resp.StatusCode != 200 {
		t.Fatalf("soft budget refused: %d", resp.StatusCode)
	}
	resp = h.admin(http.MethodGet, "/admin/limits/status", "")
	st := decode(t, resp)["data"].([]any)[0].(map[string]any)
	if st["used"].(float64) <= 10 || st["resets_at"] == nil {
		t.Errorf("status %v", st)
	}
}
