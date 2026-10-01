package fakeupstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// TriggerReasoning in the last user message makes the chat endpoint stream
// reasoning before its answer, as vLLM does for reasoning models.
const TriggerReasoning = "THINK_FIRST"

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []any           `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func chatError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error"}})
}

// chatText reads message content, which is a string or a list of parts.
func chatText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return "", fmt.Errorf("image_url.url is required")
			}
		case "file":
		default:
			return "", fmt.Errorf("unknown content part type %q", p.Type)
		}
	}
	return b.String(), nil
}

// chat serves Chat Completions, streamed or not.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		chatError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(raw)
	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		chatError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.HasPrefix(req.Model, "missing") {
		chatError(w, http.StatusNotFound, "The model does not exist.")
		return
	}
	if len(req.Messages) == 0 {
		chatError(w, http.StatusBadRequest, "messages is required")
		return
	}
	var system, lastText string
	answeredTool := false
	for i, m := range req.Messages {
		text, err := chatText(m.Content)
		if err != nil {
			chatError(w, http.StatusBadRequest, fmt.Sprintf("messages[%d].content: %v", i, err))
			return
		}
		switch m.Role {
		case "system":
			system += text
		case "user":
			lastText, answeredTool = text, false
		case "tool":
			if m.ToolCallID == "" {
				chatError(w, http.StatusBadRequest, fmt.Sprintf("messages[%d].tool_call_id is required", i))
				return
			}
			answeredTool = true
		case "assistant":
		default:
			chatError(w, http.StatusBadRequest, fmt.Sprintf("messages[%d].role %q is invalid", i, m.Role))
			return
		}
	}

	usage := func(output string) map[string]any {
		input, out := estimateTokens(string(raw)), estimateTokens(output)
		return map[string]any{
			"prompt_tokens": input, "completion_tokens": out, "total_tokens": input + out,
			"prompt_tokens_details": map[string]any{"cached_tokens": s.cached(system, estimateTokens(system))},
		}
	}
	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": nil}
		finish := "stop"
		if len(req.Tools) > 0 && !answeredTool && string(req.ToolChoice) != `"none"` {
			tool := req.Tools[0].Function
			args, _ := json.Marshal(exampleArgs(tool.Parameters))
			msg["tool_calls"] = []any{map[string]any{"id": "call_fake", "type": "function", "function": map[string]any{"name": tool.Name, "arguments": string(args)}}}
			finish = "tool_calls"
		} else {
			msg["content"] = replyText(lastText, answeredTool)
			if strings.Contains(lastText, TriggerMaxTokens) {
				finish = "length"
			}
		}
		out, _ := json.Marshal(msg)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   usage(string(out)),
		})
		return
	}

	sw := &dataWriter{w: w, f: w.(http.Flusher)}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, finish any) {
		_ = sw.Data(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	if strings.Contains(lastText, TriggerReasoning) {
		for _, word := range []string{"Let me ", "think."} {
			chunk(map[string]any{"reasoning_content": word}, nil)
		}
	}
	finish := "stop"
	output := ""
	if len(req.Tools) > 0 && !answeredTool && string(req.ToolChoice) != `"none"` {
		tool := req.Tools[0].Function
		args, _ := json.Marshal(exampleArgs(tool.Parameters))
		chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_fake", "type": "function", "function": map[string]any{"name": tool.Name, "arguments": ""}}}}, nil)
		half := len(args) / 2
		for _, part := range []string{string(args[:half]), string(args[half:])} {
			chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": part}}}}, nil)
		}
		finish, output = "tool_calls", string(args)
	} else {
		reply := replyText(lastText, answeredTool)
		for i, word := range strings.SplitAfter(reply, " ") {
			if strings.Contains(lastText, TriggerFailMidstream) && i == 2 {
				_ = sw.Data(map[string]any{"error": map[string]any{"message": "The server had an error.", "type": "server_error"}})
				return
			}
			chunk(map[string]any{"content": word}, nil)
		}
		if strings.Contains(lastText, TriggerMaxTokens) {
			finish = "length"
		}
		output = reply
	}
	chunk(map[string]any{}, finish)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		_ = sw.Data(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{},
			"usage": usage(output),
		})
	}
	_ = sw.Done()
}

// dataWriter writes unnamed SSE events, as Chat Completions servers do.
type dataWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (d *dataWriter) Data(v any) error {
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(d.w, "data: %s\n\n", b); err != nil {
		return err
	}
	d.f.Flush()
	return nil
}

func (d *dataWriter) Done() error {
	_, err := io.WriteString(d.w, "data: [DONE]\n\n")
	d.f.Flush()
	return err
}
