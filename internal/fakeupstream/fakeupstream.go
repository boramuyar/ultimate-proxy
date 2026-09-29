// Package fakeupstream is a deterministic stand-in for the Anthropic Messages
// API and the OpenAI Responses API. Tests and the CI compliance run use it so
// they need no provider credentials. It validates requests the way the real
// APIs do for the rules the proxy's adapters must follow.
package fakeupstream

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/sse"
)

// Triggers placed in the last user message.
const (
	TriggerFailMidstream = "FAIL_MIDSTREAM"
	TriggerMaxTokens     = "HIT_MAX_TOKENS"
)

type Server struct {
	APIKey string

	mu       sync.Mutex
	prefixes map[[32]byte]bool // system prompts seen, to simulate prompt caching
	Requests []json.RawMessage // raw upstream request bodies, for assertions
}

func New(apiKey string) *Server {
	return &Server{APIKey: apiKey, prefixes: map[[32]byte]bool{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.messages)
	mux.HandleFunc("POST /v1/responses", s.responses)
	return mux
}

func (s *Server) record(body []byte) {
	s.mu.Lock()
	s.Requests = append(s.Requests, append(json.RawMessage(nil), body...))
	s.mu.Unlock()
}

// LastRequest returns the most recent upstream request body.
func (s *Server) LastRequest() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Requests) == 0 {
		return nil
	}
	return s.Requests[len(s.Requests)-1]
}

// cached reports how many prompt tokens a real prompt cache would have served.
func (s *Server) cached(prefix string, tokens int) int {
	if prefix == "" {
		return 0
	}
	h := sha256.Sum256([]byte(prefix))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefixes[h] {
		return tokens
	}
	s.prefixes[h] = true
	return 0
}

func estimateTokens(s string) int { return len(s)/4 + 1 }

// Anthropic Messages API.

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Source    *struct {
		Type string `json:"type"`
	} `json:"source"`
}

type anthRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	System    []struct {
		Text string `json:"text"`
	} `json:"system"`
	Messages []struct {
		Role    string      `json:"role"`
		Content []anthBlock `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice *struct {
		Type string `json:"type"`
	} `json:"tool_choice"`
	Thinking *struct {
		BudgetTokens int `json:"budget_tokens"`
	} `json:"thinking"`
	Stream bool `json:"stream"`
}

func anthError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") != s.APIKey {
		anthError(w, http.StatusUnauthorized, "authentication_error", "invalid x-api-key")
		return
	}
	if r.Header.Get("anthropic-version") == "" {
		anthError(w, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required")
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		anthError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.record(raw)
	var req anthRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		anthError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if msg := validateAnthropic(&req); msg != "" {
		anthError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	if strings.HasPrefix(req.Model, "missing") {
		anthError(w, http.StatusNotFound, "not_found_error", "model: "+req.Model)
		return
	}

	last := req.Messages[len(req.Messages)-1]
	lastText := ""
	answeredTool := false
	for _, b := range last.Content {
		switch b.Type {
		case "text":
			lastText = b.Text
		case "tool_result":
			answeredTool = true
		}
	}
	var system strings.Builder
	for _, b := range req.System {
		system.WriteString(b.Text)
	}
	inputTokens := estimateTokens(string(raw))
	cacheRead := s.cached(system.String(), estimateTokens(system.String()))

	sw := sse.NewWriter(w)
	send := func(typ string, v map[string]any) {
		v["type"] = typ
		_ = sw.Event(typ, v)
	}
	send("message_start", map[string]any{"message": map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"usage": map[string]any{"input_tokens": inputTokens - cacheRead, "cache_read_input_tokens": cacheRead, "cache_creation_input_tokens": 0, "output_tokens": 1},
	}})
	index := 0
	if req.Thinking != nil {
		send("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
		send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": "Let me think."}})
		send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "signature_delta", "signature": "sig_fake"}})
		send("content_block_stop", map[string]any{"index": index})
		index++
	}
	stop := "end_turn"
	outputTokens := 0
	if len(req.Tools) > 0 && !answeredTool && (req.ToolChoice == nil || req.ToolChoice.Type != "none") {
		tool := req.Tools[0]
		args, _ := json.Marshal(exampleArgs(tool.InputSchema))
		send("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "tool_use", "id": "toolu_fake", "name": tool.Name, "input": map[string]any{}}})
		mid := len(args) / 2
		send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args[:mid])}})
		send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args[mid:])}})
		send("content_block_stop", map[string]any{"index": index})
		stop = "tool_use"
		outputTokens = estimateTokens(string(args))
	} else {
		reply := replyText(lastText, answeredTool)
		send("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "text", "text": ""}})
		for i, word := range strings.SplitAfter(reply, " ") {
			if strings.Contains(lastText, TriggerFailMidstream) && i == 2 {
				send("error", map[string]any{"error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}})
				return
			}
			send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "text_delta", "text": word}})
		}
		send("content_block_stop", map[string]any{"index": index})
		outputTokens = estimateTokens(reply)
		if strings.Contains(lastText, TriggerMaxTokens) {
			stop = "max_tokens"
		}
	}
	send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": outputTokens}})
	send("message_stop", map[string]any{})
}

func validateAnthropic(req *anthRequest) string {
	if req.Model == "" {
		return "model: field required"
	}
	if req.MaxTokens <= 0 {
		return "max_tokens: field required"
	}
	if req.Thinking != nil && req.MaxTokens <= req.Thinking.BudgetTokens {
		return "max_tokens must be greater than thinking.budget_tokens"
	}
	if len(req.Messages) == 0 {
		return "messages: at least one message is required"
	}
	if req.Messages[0].Role != "user" {
		return "messages: first message must use the \"user\" role"
	}
	toolUses := map[string]bool{}
	for i, m := range req.Messages {
		if i > 0 && req.Messages[i-1].Role == m.Role {
			return fmt.Sprintf("messages.%d: roles must alternate between \"user\" and \"assistant\"", i)
		}
		if len(m.Content) == 0 {
			return fmt.Sprintf("messages.%d: content must be non-empty", i)
		}
		for j, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text == "" {
					return fmt.Sprintf("messages.%d.content.%d: text content blocks must be non-empty", i, j)
				}
			case "tool_use":
				toolUses[b.ID] = true
			case "tool_result":
				if !toolUses[b.ToolUseID] {
					return fmt.Sprintf("messages.%d.content.%d: unexpected tool_use_id %q", i, j, b.ToolUseID)
				}
			case "image", "document":
				if b.Source == nil || b.Source.Type == "" {
					return fmt.Sprintf("messages.%d.content.%d: source is required", i, j)
				}
			case "thinking", "redacted_thinking":
			default:
				return fmt.Sprintf("messages.%d.content.%d: unknown block type %q", i, j, b.Type)
			}
		}
	}
	return ""
}

func replyText(lastText string, answeredTool bool) string {
	if answeredTool {
		return "Thanks, I used the tool result."
	}
	if len(lastText) > 60 {
		lastText = lastText[:60]
	}
	return "Hello from the fake upstream. You said: " + lastText
}

// exampleArgs builds arguments that satisfy a JSON schema's required string
// properties.
func exampleArgs(schema json.RawMessage) map[string]any {
	var s struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(schema, &s)
	args := map[string]any{}
	for _, name := range s.Required {
		switch s.Properties[name].Type {
		case "number", "integer":
			args[name] = 1
		case "boolean":
			args[name] = true
		default:
			args[name] = "San Francisco, CA"
		}
	}
	return args
}

// OpenAI Responses API, built with the proxy's own event builder.

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.APIKey {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided.","type":"invalid_request_error","code":"invalid_api_key"}}`))
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_json", err.Error(), ""))
		return
	}
	s.record(raw)
	var req openresponses.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_json", err.Error(), ""))
		return
	}
	items, err := req.InputItems()
	if err != nil || len(items) == 0 {
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_input", "input is required", "input"))
		return
	}
	lastText := ""
	answeredTool := false
	for _, it := range items {
		switch it.Type {
		case "message":
			if it.Role == "user" {
				parts, _ := openresponses.ContentParts(it.Content, "input_text")
				for _, p := range parts {
					if p.Text != "" {
						lastText = p.Text
					}
				}
				answeredTool = false
			}
		case "function_call_output":
			answeredTool = true
		}
	}

	var sink openresponses.Sink
	var sw *sse.Writer
	if req.Stream {
		sw = sse.NewWriter(w)
		sink = sw
	}
	b := openresponses.NewBuilder(openresponses.NewResponse(openresponses.NewID("resp"), req.Model, time.Now().Unix(), &req), sink)
	b.Start()
	outputTokens := 0
	if len(req.Tools) > 0 && !answeredTool {
		args, _ := json.Marshal(exampleArgs(req.Tools[0].Parameters))
		idx := b.StartFunctionCall(openresponses.NewID("call"), req.Tools[0].Name)
		b.ArgumentsDelta(idx, string(args))
		b.EndFunctionCall(idx, "completed")
		outputTokens = estimateTokens(string(args))
	} else {
		reply := replyText(lastText, answeredTool)
		idx := b.StartMessage()
		for _, word := range strings.SplitAfter(reply, " ") {
			b.TextDelta(idx, word)
		}
		b.EndMessage(idx, "completed")
		outputTokens = estimateTokens(reply)
	}
	prefix := ""
	if req.Instructions != nil {
		prefix = *req.Instructions
	}
	input := estimateTokens(string(raw))
	u := &openresponses.Usage{
		InputTokens:        input,
		OutputTokens:       outputTokens,
		TotalTokens:        input + outputTokens,
		InputTokensDetails: openresponses.InputTokensDetails{CachedTokens: s.cached(prefix, estimateTokens(prefix))},
	}
	resp := b.Complete(u, "")
	if sw != nil {
		_ = sw.Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
