package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAuditLog(t *testing.T) {
	h := newAuthHarness(t, "")
	c := newBrowser(t)
	h.signIn(c, "/")
	csrf := []string{"X-Up-Admin", "1"}

	tenant := decode(t, h.do(c, "POST", "/admin/tenants", `{"name":"acme"}`, csrf...))
	app := decode(t, h.do(c, "POST", "/admin/tenants/"+tenant["id"].(string)+"/applications", `{"name":"bot"}`, csrf...))
	key := decode(t, h.do(c, "POST", "/admin/applications/"+app["id"].(string)+"/keys", `{"allowed_models":["gpt"]}`, csrf...))
	if resp := h.do(c, "DELETE", "/admin/keys/"+key["id"].(string), "", csrf...); resp.StatusCode != 204 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if resp := h.do(c, "POST", "/admin/limits", `{"kind":"nope"}`, csrf...); resp.StatusCode != 400 {
		t.Fatalf("bad limit: %d", resp.StatusCode)
	}
	// Reads are not recorded.
	h.do(c, "GET", "/admin/tenants", "").Body.Close()

	// The break-glass token can read the log too.
	resp := h.do(http.DefaultClient, "GET", "/admin/audit", "", "Authorization", "Bearer break-glass")
	got := decode(t, resp)["data"].([]any)
	var actions []string
	for _, e := range got {
		actions = append(actions, e.(map[string]any)["action"].(string))
	}
	if strings.Join(actions, " ") != "limit.create key.revoke key.create application.create tenant.create sign_in" {
		t.Fatalf("actions, newest first: %v", actions)
	}
	byAction := map[string]map[string]any{}
	for _, e := range got {
		byAction[e.(map[string]any)["action"].(string)] = e.(map[string]any)
	}
	k := byAction["key.create"]
	if k["actor_email"] != "ada@example.com" || k["actor_method"] != "oidc" || k["target_id"] != key["id"] || k["status"].(float64) != 201 {
		t.Fatalf("key.create entry: %v", k)
	}
	if req := k["request"].(map[string]any); req["allowed_models"].([]any)[0] != "gpt" {
		t.Fatalf("request body: %v", k["request"])
	}
	if strings.Contains(asJSON(t, got), key["key"].(string)) {
		t.Fatal("the new key's secret reached the audit log")
	}
	if byAction["key.revoke"]["target_id"] != key["id"] || byAction["limit.create"]["status"].(float64) != 400 {
		t.Fatalf("revoke or failed create: %v %v", byAction["key.revoke"], byAction["limit.create"])
	}
	if byAction["sign_in"]["actor_email"] != "ada@example.com" {
		t.Fatalf("sign-in: %v", byAction["sign_in"])
	}

	page := decode(t, h.do(http.DefaultClient, "GET", "/admin/audit?limit=2&before="+k["ts"].(string), "", "Authorization", "Bearer break-glass"))["data"].([]any)
	if len(page) != 2 || page[0].(map[string]any)["action"] != "application.create" {
		t.Fatalf("second page: %v", page)
	}
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
