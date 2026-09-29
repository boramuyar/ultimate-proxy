package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/fakeupstream"
	"github.com/boramuyar/ultimate-proxy/internal/meter"
	"github.com/boramuyar/ultimate-proxy/internal/sse"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

type harness struct {
	t           testing.TB
	proxy       *httptest.Server
	upstreamURL string
	upstream    *fakeupstream.Server
	store       *store.Memory
	meter       *meter.Meter
}

const (
	trustedKey   = "up_test_trusted"
	untrustedKey = "up_test_untrusted"
	adminToken   = "admin"
)

func newHarness(t testing.TB) *harness {
	t.Helper()
	up := fakeupstream.New("fake-key")
	upSrv := httptest.NewServer(up.Handler())
	t.Cleanup(upSrv.Close)

	no := false
	cfg, err := config.Parse([]byte(`
admin_token: admin
providers:
  - {name: anthropic, type: anthropic, base_url: "` + upSrv.URL + `", api_key: fake-key}
  - {name: openai, type: openai, base_url: "` + upSrv.URL + `/v1", api_key: fake-key}
  - {name: badkey, type: anthropic, base_url: "` + upSrv.URL + `", api_key: wrong}
models:
  - {name: claude, provider: anthropic, upstream_model: claude-fake}
  - {name: gpt, provider: openai, upstream_model: gpt-fake}
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bootstrap = []config.Tenant{
		{Tenant: "acme", Applications: []config.Application{{Name: "chat", Keys: []string{trustedKey}}}},
		{Tenant: "globex", Applications: []config.Application{{Name: "widget", CanAssertUsers: &no, Keys: []string{untrustedKey}}}},
	}
	st := store.NewMemory()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := meter.New(st, 1000, 10, 10*time.Millisecond, log)
	srv, err := New(cfg, st, m, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return &harness{t: t, proxy: proxy, upstreamURL: upSrv.URL, upstream: up, store: st, meter: m}
}

func (h *harness) post(key, body string, headers ...string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.proxy.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

type streamed struct {
	names  []string
	events []map[string]any
	done   bool
}

func readStream(t *testing.T, resp *http.Response) streamed {
	t.Helper()
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var s streamed
	rd := sse.NewReader(resp.Body)
	for {
		ev, err := rd.Next()
		if err == io.EOF {
			return s
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(ev.Data) == "[DONE]" {
			s.done = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			t.Fatal(err)
		}
		if m["type"] != ev.Name {
			t.Errorf("event name %q does not match type %v", ev.Name, m["type"])
		}
		s.names = append(s.names, ev.Name)
		s.events = append(s.events, m)
	}
}

// usage waits for the meter to write the expected number of events.
func (h *harness) usage(q store.UsageQuery, wantRequests int64) []store.UsageRow {
	h.t.Helper()
	if q.To.IsZero() {
		q.From, q.To = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, _ := h.store.QueryUsage(context.Background(), q)
		var total int64
		for _, r := range rows {
			total += r.Requests
		}
		if total >= wantRequests || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNonStreamingAttributesUsage(t *testing.T) {
	h := newHarness(t)
	resp := h.post(trustedKey, `{"model":"claude","input":"hi there"}`, "X-Proxy-User-Email", "Alice@Example.com")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, decode(t, resp))
	}
	body := decode(t, resp)
	if body["status"] != "completed" || body["object"] != "response" {
		t.Fatalf("unexpected response %v", body)
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"].(float64) <= 0 {
		t.Fatalf("no usage: %v", usage)
	}

	rows := h.usage(store.UsageQuery{GroupBy: []string{"email", "model"}}, 1)
	if len(rows) != 1 || rows[0].Group["email"] != "alice@example.com" || rows[0].Group["model"] != "claude" {
		t.Fatalf("unexpected usage rows %+v", rows)
	}
	if rows[0].InputTokens != int64(usage["input_tokens"].(float64)) {
		t.Errorf("metered %d input tokens, response says %v", rows[0].InputTokens, usage["input_tokens"])
	}
}

func TestUntrustedAppCannotAssertUser(t *testing.T) {
	h := newHarness(t)
	resp := h.post(untrustedKey, `{"model":"claude","input":"hi","metadata":{"user_email":"mallory@example.com"}}`, "X-Proxy-User-Email", "ceo@example.com")
	decode(t, resp)
	rows := h.usage(store.UsageQuery{GroupBy: []string{"tenant", "email"}}, 1)
	if len(rows) != 1 || rows[0].Group["email"] != "" {
		t.Fatalf("untrusted app's user assertion was accepted: %+v", rows)
	}
}

func TestUserFromMetadataAndSafetyIdentifier(t *testing.T) {
	h := newHarness(t)
	decode(t, h.post(trustedKey, `{"model":"claude","input":"hi","metadata":{"user_email":"meta@example.com"}}`))
	decode(t, h.post(trustedKey, `{"model":"claude","input":"hi","safety_identifier":"safe@example.com"}`))
	rows := h.usage(store.UsageQuery{GroupBy: []string{"email"}}, 2)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Group["email"]] = true
	}
	if !got["meta@example.com"] || !got["safe@example.com"] {
		t.Fatalf("users not resolved: %+v", rows)
	}
}

func TestStreamingToolCall(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"claude", "gpt"} {
		t.Run(model, func(t *testing.T) {
			resp := h.post(trustedKey, `{"model":"`+model+`","stream":true,"input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}]}`)
			s := readStream(t, resp)
			if !s.done {
				t.Fatal("stream did not end with [DONE]")
			}
			for i, ev := range s.events {
				if int(ev["sequence_number"].(float64)) != i {
					t.Fatalf("event %d has sequence_number %v", i, ev["sequence_number"])
				}
			}
			if s.names[0] != "response.created" || s.names[len(s.names)-1] != "response.completed" {
				t.Fatalf("unexpected event order %v", s.names)
			}
			final := s.events[len(s.events)-1]["response"].(map[string]any)
			item := final["output"].([]any)[0].(map[string]any)
			if item["type"] != "function_call" || item["name"] != "get_weather" || item["arguments"] != `{"location":"San Francisco, CA"}` {
				t.Fatalf("unexpected output item %v", item)
			}
		})
	}
}

func TestToolRoundTripTranslatesToAnthropic(t *testing.T) {
	h := newHarness(t)
	resp := h.post(trustedKey, `{"model":"claude","instructions":"Be brief.","input":[
		{"type":"message","role":"developer","content":"Use tools."},
		{"type":"message","role":"assistant","content":"Earlier answer."},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"location\":\"SF\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"}
	],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"tool_choice":"required","parallel_tool_calls":false}`)
	body := decode(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	var sent struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Source    struct {
					Type, MediaType string
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
		ToolChoice struct {
			Type                   string `json:"type"`
			DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(h.upstream.LastRequest(), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.System) != 2 || sent.System[0].Text != "Be brief." || sent.System[1].Text != "Use tools." {
		t.Errorf("system prompt not translated: %+v", sent.System)
	}
	roles := []string{}
	for _, m := range sent.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,user,assistant,user" {
		t.Errorf("unexpected roles %v", roles)
	}
	if last := sent.Messages[len(sent.Messages)-1].Content[0]; last.Type != "tool_result" || last.ToolUseID != "call_1" {
		t.Errorf("tool result not translated: %+v", last)
	}
	if sent.ToolChoice.Type != "any" || !sent.ToolChoice.DisableParallelToolUse {
		t.Errorf("tool_choice not translated: %+v", sent.ToolChoice)
	}
}

func TestReasoningRoundTrip(t *testing.T) {
	h := newHarness(t)
	body := decode(t, h.post(trustedKey, `{"model":"claude","input":"think","reasoning":{"effort":"low"},"max_output_tokens":100}`))
	out := body["output"].([]any)
	rs := out[0].(map[string]any)
	if rs["type"] != "reasoning" || rs["encrypted_content"] != "sig_fake" {
		t.Fatalf("unexpected reasoning item %v", rs)
	}
	// Replaying the reasoning item must send the thinking block and signature back.
	item, _ := json.Marshal(rs)
	decode(t, h.post(trustedKey, `{"model":"claude","reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":"think"},`+string(item)+`,{"type":"message","role":"user","content":"again"}]}`))
	if !strings.Contains(string(h.upstream.LastRequest()), `"signature":"sig_fake"`) {
		t.Fatalf("signature not replayed: %s", h.upstream.LastRequest())
	}
}

func TestMidstreamFailure(t *testing.T) {
	h := newHarness(t)
	s := readStream(t, h.post(trustedKey, `{"model":"claude","stream":true,"input":"please `+fakeupstream.TriggerFailMidstream+`"}`))
	n := len(s.names)
	if n < 2 || s.names[n-2] != "error" || s.names[n-1] != "response.failed" || !s.done {
		t.Fatalf("expected error then response.failed, got %v", s.names)
	}
	rows := h.usage(store.UsageQuery{GroupBy: []string{"model"}}, 1)
	if rows[0].FailedRequests != 1 {
		t.Fatalf("failure not metered: %+v", rows)
	}
}

func TestIncompleteOnMaxTokens(t *testing.T) {
	h := newHarness(t)
	body := decode(t, h.post(trustedKey, `{"model":"claude","input":"`+fakeupstream.TriggerMaxTokens+`"}`))
	if body["status"] != "incomplete" || body["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("unexpected %v", body)
	}
}

func TestPassthroughCapturesCachedTokens(t *testing.T) {
	h := newHarness(t)
	req := `{"model":"gpt","instructions":"You are a long, stable system prompt.","input":"hi"}`
	decode(t, h.post(trustedKey, req))
	body := decode(t, h.post(trustedKey, req))
	cached := body["usage"].(map[string]any)["input_tokens_details"].(map[string]any)["cached_tokens"].(float64)
	if cached == 0 {
		t.Fatal("expected a cache hit on the second call")
	}
	rows := h.usage(store.UsageQuery{GroupBy: []string{"model"}}, 2)
	if rows[0].CachedInputTokens != int64(cached) {
		t.Fatalf("metered cached tokens %d, want %v", rows[0].CachedInputTokens, cached)
	}
}

func TestErrors(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name, key, body string
		status          int
		code            string
	}{
		{"bad key", "up_nope", `{"model":"claude","input":"hi"}`, 401, "invalid_api_key"},
		{"unknown model", trustedKey, `{"model":"nope","input":"hi"}`, 400, "model_not_found"},
		{"upstream 404", trustedKey, `{"model":"anthropic/missing-model","input":"hi"}`, 400, "model_not_found"},
		{"upstream auth", trustedKey, `{"model":"badkey/claude-fake","input":"hi"}`, 502, "upstream_auth_failed"},
		{"bad json", trustedKey, `{`, 400, "invalid_json"},
		{"hosted tool", trustedKey, `{"model":"claude","input":"hi","tools":[{"type":"web_search"}]}`, 400, "unsupported_tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.post(c.key, c.body)
			body := decode(t, resp)
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d: %v", resp.StatusCode, c.status, body)
			}
			if code := body["error"].(map[string]any)["code"]; code != c.code {
				t.Fatalf("code %v, want %s", code, c.code)
			}
		})
	}
}

func TestAdminKeysAndUsage(t *testing.T) {
	h := newHarness(t)
	admin := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, h.proxy.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusNoContent {
			return resp.StatusCode, nil
		}
		return resp.StatusCode, decode(t, resp)
	}
	_, tenant := admin("POST", "/admin/tenants", `{"name":"initech"}`)
	_, app := admin("POST", "/admin/tenants/"+tenant["id"].(string)+"/applications", `{"name":"tps"}`)
	status, key := admin("POST", "/admin/applications/"+app["id"].(string)+"/keys", ``)
	if status != 201 || !strings.HasPrefix(key["key"].(string), "up_") {
		t.Fatalf("key not created: %d %v", status, key)
	}
	decode(t, h.post(key["key"].(string), `{"model":"claude","input":"hi"}`, "X-Proxy-User-Email", "peter@initech.com"))
	h.usage(store.UsageQuery{}, 1)

	_, usage := admin("GET", "/admin/usage?group_by=tenant,application,email", "")
	row := usage["data"].([]any)[0].(map[string]any)["group"].(map[string]any)
	if row["tenant_name"] != "initech" || row["application_name"] != "tps" || row["email"] != "peter@initech.com" {
		t.Fatalf("unexpected usage row %v", row)
	}

	if status, _ := admin("DELETE", "/admin/keys/"+key["id"].(string), ""); status != 204 {
		t.Fatalf("revoke status %d", status)
	}
	if resp := h.post(key["key"].(string), `{"model":"claude","input":"hi"}`); resp.StatusCode != 401 {
		t.Fatalf("revoked key still works: %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", h.proxy.URL+"/admin/tenants", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("admin API open without token: %d", resp.StatusCode)
	}
}
