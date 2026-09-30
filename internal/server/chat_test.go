package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/boramuyar/ultimate-proxy/internal/fakeupstream"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// The upstream request a Chat Completions provider received.
type sentChat struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice     json.RawMessage `json:"tool_choice"`
	MaxTokens      *int            `json:"max_tokens"`
	ResponseFormat json.RawMessage `json:"response_format"`
	ReasoningEff   *string         `json:"reasoning_effort"`
	User           string          `json:"user"`
	StreamOptions  map[string]bool `json:"stream_options"`
}

func (h *harness) sentChat() sentChat {
	h.t.Helper()
	var c sentChat
	if err := json.Unmarshal(h.upstream.LastRequest(), &c); err != nil {
		h.t.Fatal(err)
	}
	return c
}

func TestChatTranslatesRequest(t *testing.T) {
	h := newHarness(t)
	resp := h.post(trustedKey, `{
		"model": "llama",
		"instructions": "Be brief.",
		"max_output_tokens": 50,
		"reasoning": {"effort": "low"},
		"safety_identifier": "user-1",
		"text": {"format": {"type": "json_schema", "name": "answer", "schema": {"type": "object"}}},
		"tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}],
		"tool_choice": {"type": "function", "name": "get_weather"},
		"input": [
			{"role": "user", "content": "weather?"},
			{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{}"},
			{"type": "function_call", "call_id": "c2", "name": "get_weather", "arguments": "{\"x\":1}"},
			{"type": "function_call_output", "call_id": "c1", "output": "sunny"},
			{"type": "function_call_output", "call_id": "c2", "output": [{"type": "input_text", "text": "rainy"}]},
			{"type": "reasoning", "summary": []},
			{"role": "assistant", "content": [{"type": "output_text", "text": "Mixed."}]},
			{"role": "user", "content": [{"type": "input_text", "text": "look"}, {"type": "input_image", "image_url": "https://example.com/a.png"}]}
		]
	}`)
	body := decode(t, resp)
	if resp.StatusCode != 200 || body["status"] != "completed" {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	c := h.sentChat()
	if c.Model != "llama-fake" || *c.MaxTokens != 50 || *c.ReasoningEff != "low" || c.User != "user-1" || !c.StreamOptions["include_usage"] {
		t.Errorf("request fields %+v", c)
	}
	var roles []string
	for _, m := range c.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,tool,assistant,user" {
		t.Fatalf("roles %s", got)
	}
	if n := len(c.Messages[2].ToolCalls); n != 2 || c.Messages[2].ToolCalls[1].Function.Arguments != `{"x":1}` {
		t.Errorf("parallel calls should share one assistant message: %+v", c.Messages[2])
	}
	if string(c.Messages[4].Content) != `"rainy"` || c.Messages[4].ToolCallID != "c2" {
		t.Errorf("tool output %s", c.Messages[4].Content)
	}
	if string(c.Messages[1].Content) != `"weather?"` || !strings.Contains(string(c.Messages[6].Content), `"image_url"`) {
		t.Errorf("user content %s / %s", c.Messages[1].Content, c.Messages[6].Content)
	}
	if string(c.ToolChoice) != `{"function":{"name":"get_weather"},"type":"function"}` {
		t.Errorf("tool choice %s", c.ToolChoice)
	}
	if !strings.Contains(string(c.ResponseFormat), `"json_schema":{`) {
		t.Errorf("response format %s", c.ResponseFormat)
	}
	// The response echoes the client's request, not the translated one.
	if body["model"] != "llama" || body["instructions"] != "Be brief." {
		t.Errorf("response %v", body)
	}
}

func TestChatStreamsToolCall(t *testing.T) {
	h := newHarness(t)
	resp := h.post(trustedKey, `{"model":"llama","stream":true,"input":"weather in SF?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}]}`)
	s := readStream(t, resp)
	if !s.done || s.names[0] != "response.created" || s.names[len(s.names)-1] != "response.completed" {
		t.Fatalf("events %v", s.names)
	}
	for i, ev := range s.events {
		if int(ev["sequence_number"].(float64)) != i {
			t.Fatalf("event %d has sequence number %v", i, ev["sequence_number"])
		}
	}
	final := s.events[len(s.events)-1]["response"].(map[string]any)
	out := final["output"].([]any)
	fc := out[0].(map[string]any)
	if len(out) != 1 || fc["type"] != "function_call" || fc["call_id"] != "call_fake" || fc["arguments"] != `{"location":"San Francisco, CA"}` {
		t.Fatalf("output %v", out)
	}
}

func TestChatReasoningAndUsage(t *testing.T) {
	h := newHarness(t)
	body := `{"model":"llama","instructions":"You are a helpful assistant with a long system prompt.","input":"` + fakeupstream.TriggerReasoning + `"}`
	decode(t, h.post(trustedKey, body))
	resp := decode(t, h.post(trustedKey, body))
	out := resp["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "reasoning" || out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output %v", out)
	}
	if text := out[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "Let me think." {
		t.Errorf("reasoning %v", text)
	}
	usage := resp["usage"].(map[string]any)
	cached := usage["input_tokens_details"].(map[string]any)["cached_tokens"].(float64)
	if cached == 0 {
		t.Errorf("second request should hit the cache: %v", usage)
	}
	rows := h.usage(store.UsageQuery{GroupBy: []string{"model", "provider"}}, 2)
	if len(rows) != 1 || rows[0].Group["provider"] != "local" || rows[0].CachedInputTokens != int64(cached) {
		t.Errorf("rows %+v", rows)
	}
}

func TestChatIncompleteAndFailure(t *testing.T) {
	h := newHarness(t)
	resp := decode(t, h.post(trustedKey, `{"model":"llama","input":"`+fakeupstream.TriggerMaxTokens+`"}`))
	if resp["status"] != "incomplete" || resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Errorf("response %v", resp)
	}

	s := readStream(t, h.post(trustedKey, `{"model":"llama","stream":true,"input":"`+fakeupstream.TriggerFailMidstream+`"}`))
	if n := len(s.names); n < 2 || s.names[n-2] != "error" || s.names[n-1] != "response.failed" {
		t.Errorf("events %v", s.names)
	}
}

func TestChatRejectsWhatItCannotTranslate(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{
		`{"model":"llama","input":"hi","previous_response_id":"resp_1"}`,
		`{"model":"llama","input":"hi","tools":[{"type":"web_search"}]}`,
		`{"model":"llama","input":[{"type":"item_reference","id":"x"}]}`,
	} {
		resp := h.post(trustedKey, body)
		if b := decode(t, resp); resp.StatusCode != 400 {
			t.Errorf("%s: status %d %v", body, resp.StatusCode, b)
		}
	}
	resp := h.post(trustedKey, `{"model":"local/missing-model","input":"hi"}`)
	if b := decode(t, resp); resp.StatusCode != 400 || b["error"].(map[string]any)["code"] != "model_not_found" {
		t.Errorf("status %d %v", resp.StatusCode, b)
	}
}
