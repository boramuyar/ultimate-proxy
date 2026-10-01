package server

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const chatBody = `{"model":"gpt","messages":[{"role":"system","content":"You are terse."},{"role":"user","content":"hello there"}]`

func TestChatNonStreaming(t *testing.T) {
	h := newHarness(t)
	resp := h.postTo("/openai/v1/chat/completions", trustedKey, chatBody+`}`)
	body := decode(t, resp)
	if resp.StatusCode != 200 || body["object"] != "chat.completion" || resp.Header.Get("X-Proxy-Request-Id") == "" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	// The request reached the upstream unchanged.
	if got := string(h.upstream.LastRequest()); got != chatBody+`}` {
		t.Errorf("upstream got %s", got)
	}
	ev := h.events(1)[0]
	if ev.Status != "completed" || ev.InputTokens == 0 || ev.OutputTokens == 0 || !ev.UsageReported || ev.Provider != "openai" || ev.Model != "gpt" || ev.CostUSD <= 0 {
		t.Errorf("usage event %+v", ev)
	}
}

// chatStream reads the data lines of a Chat Completions stream.
func chatStream(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestChatStreaming(t *testing.T) {
	h := newHarness(t)
	// The client did not ask for usage: the proxy asks for it upstream and
	// leaves the usage chunk out of what the client sees.
	resp := h.postTo("/openai/v1/chat/completions", trustedKey, chatBody+`,"stream":true}`)
	lines := chatStream(t, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("stream %v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, `"usage"`) {
			t.Errorf("usage chunk reached a client that did not ask for it: %s", l)
		}
	}
	if !strings.Contains(string(h.upstream.LastRequest()), `"stream_options":{"include_usage":true}`) {
		t.Errorf("usage not asked for upstream: %s", h.upstream.LastRequest())
	}
	ev := h.events(1)[0]
	if ev.Status != "completed" || ev.InputTokens == 0 || !ev.Stream || ev.TTFTMS == nil {
		t.Errorf("usage event %+v", ev)
	}

	// A client that asks for usage gets it, and its body goes up unchanged.
	asked := chatBody + `,"stream":true,"stream_options":{"include_usage":true}}`
	resp = h.postTo("/openai/v1/chat/completions", trustedKey, asked)
	lines = chatStream(t, resp.Body)
	resp.Body.Close()
	var last map[string]any
	_ = json.Unmarshal([]byte(lines[len(lines)-2]), &last)
	if last["usage"] == nil || string(h.upstream.LastRequest()) != asked {
		t.Errorf("usage chunk %v, upstream got %s", last, h.upstream.LastRequest())
	}
}

func TestChatErrorsPassThrough(t *testing.T) {
	h := newHarness(t)
	resp := h.postTo("/openai/v1/chat/completions", trustedKey, `{"model":"missing-model","messages":[{"role":"user","content":"hi"}]}`)
	body := decode(t, resp)
	if resp.StatusCode != 404 || body["error"].(map[string]any)["message"] != "The model does not exist." {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if ev := h.events(1)[0]; ev.Status != "failed" || ev.HTTPStatus != 404 {
		t.Errorf("usage event %+v", ev)
	}
	resp = h.postTo("/badkey/v1/chat/completions", trustedKey, chatBody+`}`)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("upstream auth failure: %d", resp.StatusCode)
	}
}

func TestChatLimits(t *testing.T) {
	h := newHarness(t)
	tenant, _ := h.app(t, "chat")
	h.addLimit(t, `{"tenant_id":"`+tenant+`","kind":"rpm","amount":1}`)
	if resp := h.postTo("/openai/v1/chat/completions", trustedKey, chatBody+`}`); resp.StatusCode != 200 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp := h.postTo("/openai/v1/chat/completions", trustedKey, chatBody+`}`)
	if body := decode(t, resp); resp.StatusCode != 429 || body["error"].(map[string]any)["code"] != "rate_limit_exceeded" {
		t.Fatalf("second: %d %v", resp.StatusCode, body)
	}
}
