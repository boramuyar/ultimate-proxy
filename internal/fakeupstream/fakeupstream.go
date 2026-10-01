// Package fakeupstream is a deterministic stand-in for the OpenAI Responses
// API and the Chat Completions API. Tests and the CI compliance run use it so they need no provider
// credentials.
package fakeupstream

import (
	"crypto/sha256"
	"encoding/json"
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
	// TriggerNoCache in the instructions makes the prompt cache always miss,
	// like a provider that evicted or never stored the prefix.
	TriggerNoCache = "NOCACHE"
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
	mux.HandleFunc("POST /v1/responses", s.responses)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-fake","object":"model","owned_by":"fake"}]}`))
	})
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
	if prefix == "" || strings.Contains(prefix, TriggerNoCache) {
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

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
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
	if strings.HasPrefix(req.Model, "missing") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"The model does not exist.","type":"invalid_request_error","code":"model_not_found"}}`))
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
	incomplete := ""
	if len(req.Tools) > 0 && !answeredTool {
		args, _ := json.Marshal(exampleArgs(req.Tools[0].Parameters))
		idx := b.StartFunctionCall(openresponses.NewID("call"), req.Tools[0].Name)
		b.ArgumentsDelta(idx, string(args))
		b.EndFunctionCall(idx, "completed")
		outputTokens = estimateTokens(string(args))
	} else {
		reply := replyText(lastText, answeredTool)
		idx := b.StartMessage()
		for i, word := range strings.SplitAfter(reply, " ") {
			if strings.Contains(lastText, TriggerFailMidstream) && i == 2 {
				b.Fail(openresponses.NewError(http.StatusInternalServerError, openresponses.ErrServer, "server_error", "The server had an error.", ""))
				if sw != nil {
					_ = sw.Done()
				}
				return
			}
			b.TextDelta(idx, word)
		}
		status := "completed"
		if strings.Contains(lastText, TriggerMaxTokens) {
			status, incomplete = "incomplete", "max_output_tokens"
		}
		b.EndMessage(idx, status)
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
	resp := b.Complete(u, incomplete)
	if sw != nil {
		_ = sw.Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// authorized checks the caller's key, answering 401 as OpenAI does if wrong.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") == "Bearer "+s.APIKey {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided.","type":"invalid_request_error","code":"invalid_api_key"}}`))
	return false
}
