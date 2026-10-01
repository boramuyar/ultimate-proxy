package server

import (
	"net/http"
	"testing"
)

// TestRoles signs in as people with each role and checks what they may do.
func TestRoles(t *testing.T) {
	h := newAuthHarness(t, `    roles:
      - role: viewer
        emails: [vera@example.com]
      - role: admin
        groups: [acme-admins]
        tenants: [acme]
      - role: viewer
        emails: [tom@example.com]
        tenants: [globex]
`)
	root := newBrowser(t) // ada, a full admin
	h.signIn(root, "/")
	csrf := []string{"X-Up-Admin", "1"}
	acme := decode(t, h.do(root, "POST", "/admin/tenants", `{"name":"acme"}`, csrf...))["id"].(string)
	globex := decode(t, h.do(root, "POST", "/admin/tenants", `{"name":"globex"}`, csrf...))["id"].(string)
	acmeApp := decode(t, h.do(root, "POST", "/admin/tenants/"+acme+"/applications", `{"name":"bot"}`, csrf...))["id"].(string)
	globexApp := decode(t, h.do(root, "POST", "/admin/tenants/"+globex+"/applications", `{"name":"bot"}`, csrf...))["id"].(string)
	globexKey := decode(t, h.do(root, "POST", "/admin/applications/"+globexApp+"/keys", ``, csrf...))["id"].(string)
	globexLimit := decode(t, h.do(root, "POST", "/admin/limits", `{"tenant_id":"`+globex+`","kind":"rpm","amount":10}`, csrf...))["id"].(string)

	signIn := func(claims map[string]any) *http.Client {
		h.idp.SetUser(claims)
		c := newBrowser(t)
		h.signIn(c, "/")
		return c
	}
	vera := signIn(map[string]any{"sub": "v", "email": "vera@example.com"})
	tenantAdmin := signIn(map[string]any{"sub": "a", "email": "al@example.com", "groups": []string{"acme-admins"}})
	tom := signIn(map[string]any{"sub": "t", "email": "tom@example.com"})

	me := decode(t, h.do(tenantAdmin, "GET", "/admin/auth/me", ""))
	if me["role"] != "tenant admin" || me["is_admin"] != false || me["reads_all"] != false {
		t.Fatalf("me: %v", me)
	}

	status := func(c *http.Client, method, path, body string) int {
		t.Helper()
		resp := h.do(c, method, path, body, csrf...)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, tc := range []struct {
		who          string
		c            *http.Client
		method, path string
		body         string
		want         int
	}{
		// A viewer reads everything and changes nothing.
		{"viewer", vera, "GET", "/admin/tenants", "", 200},
		{"viewer", vera, "GET", "/admin/audit", "", 200},
		{"viewer", vera, "GET", "/admin/applications/" + globexApp + "/keys", "", 200},
		{"viewer", vera, "POST", "/admin/tenants", `{"name":"x"}`, 403},
		{"viewer", vera, "POST", "/admin/applications/" + acmeApp + "/keys", "", 403},
		{"viewer", vera, "DELETE", "/admin/keys/" + globexKey, "", 403},
		{"viewer", vera, "POST", "/admin/prices", `{"model":"m","input":1,"output":1}`, 403},
		// A tenant admin runs its own tenant only.
		{"tenant admin", tenantAdmin, "POST", "/admin/tenants/" + acme + "/applications", `{"name":"two"}`, 201},
		{"tenant admin", tenantAdmin, "POST", "/admin/applications/" + acmeApp + "/keys", "", 201},
		{"tenant admin", tenantAdmin, "POST", "/admin/limits", `{"tenant_id":"` + acme + `","kind":"rpm","amount":5}`, 201},
		{"tenant admin", tenantAdmin, "POST", "/admin/limits", `{"tenant_id":"` + globex + `","kind":"rpm","amount":5}`, 403},
		{"tenant admin", tenantAdmin, "GET", "/admin/applications/" + globexApp + "/keys", "", 403},
		{"tenant admin", tenantAdmin, "POST", "/admin/tenants/" + globex + "/applications", `{"name":"x"}`, 403},
		{"tenant admin", tenantAdmin, "DELETE", "/admin/keys/" + globexKey, "", 403},
		{"tenant admin", tenantAdmin, "DELETE", "/admin/limits/" + globexLimit, "", 403},
		{"tenant admin", tenantAdmin, "POST", "/admin/tenants", `{"name":"x"}`, 403},
		{"tenant admin", tenantAdmin, "GET", "/admin/audit", "", 403},
		{"tenant admin", tenantAdmin, "GET", "/admin/usage?tenant_id=" + globex, "", 403},
		{"tenant admin", tenantAdmin, "GET", "/admin/prices", "", 200},
		// A tenant viewer reads its tenant only.
		{"tenant viewer", tom, "GET", "/admin/applications/" + globexApp + "/keys", "", 200},
		{"tenant viewer", tom, "GET", "/admin/applications/" + acmeApp + "/keys", "", 403},
		{"tenant viewer", tom, "DELETE", "/admin/keys/" + globexKey, "", 403},
		{"tenant viewer", tom, "DELETE", "/admin/keys/nope", "", 404},
	} {
		if got := status(tc.c, tc.method, tc.path, tc.body); got != tc.want {
			t.Errorf("%s: %s %s = %d, want %d", tc.who, tc.method, tc.path, got, tc.want)
		}
	}

	// Lists only show readable tenants.
	names := func(c *http.Client, path, field string) []string {
		var out []string
		for _, e := range decode(t, h.do(c, "GET", path, ""))["data"].([]any) {
			out = append(out, e.(map[string]any)[field].(string))
		}
		return out
	}
	if got := names(tom, "/admin/tenants", "name"); len(got) != 1 || got[0] != "globex" {
		t.Fatalf("tenant viewer's tenants: %v", got)
	}
	if got := names(tenantAdmin, "/admin/applications", "tenant_id"); len(got) != 2 || got[0] != acme || got[1] != acme {
		t.Fatalf("tenant admin's applications: %v", got)
	}
	if got := names(tenantAdmin, "/admin/limits", "tenant_id"); len(got) != 1 || got[0] != acme {
		t.Fatalf("tenant admin's limits: %v", got)
	}
	if got := names(vera, "/admin/limits", "tenant_id"); len(got) != 2 {
		t.Fatalf("viewer's limits: %v", got)
	}

	// The audit log names the role.
	for _, e := range decode(t, h.do(root, "GET", "/admin/audit", ""))["data"].([]any) {
		if e := e.(map[string]any); e["action"] == "key.create" && e["actor_email"] == "al@example.com" && e["actor_role"] != "tenant admin" {
			t.Fatalf("audit entry: %v", e)
		}
	}
}
