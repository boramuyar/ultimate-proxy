package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/openresponses"
	"github.com/omni-proxy/omni-proxy/internal/sse"
)

// ChatResult is what the proxy accounts for from a relayed Chat Completions
// call.
type ChatResult struct {
	Result
	HTTPStatus int
	// FirstToken is when the first content chunk reached the client.
	FirstToken time.Time
}

// Chat relays a Chat Completions request to the upstream and its reply to w,
// unchanged: status, body and, for streams, each chunk as it arrives. It
// reads the usage, the finish reason and any error on the way. dropUsage
// leaves out the usage-only chunk at the end of a stream, for clients that
// did not ask for it.
//
// It returns an error only when nothing was written to w.
func (p *OpenAI) Chat(ctx context.Context, body []byte, stream, dropUsage bool, w http.ResponseWriter) (*ChatResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, openresponses.ServerError("proxy_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, networkError(p.name, err)
	}
	defer resp.Body.Close()

	res := &ChatResult{Result: Result{Status: "failed"}, HTTPStatus: resp.StatusCode}
	copyHeader(w.Header(), resp.Header, "Content-Type", "Retry-After", "X-Request-Id")
	if resp.StatusCode/100 != 2 || !stream {
		b, err := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
		if err != nil {
			res.ErrorCode = "upstream_stream_error"
			return res, nil
		}
		if resp.StatusCode/100 != 2 {
			res.ErrorCode = chatErrorCode(b)
			return res, nil
		}
		var c chatChunk
		if json.Unmarshal(b, &c) == nil {
			res.read(&c)
		}
		return res, nil
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	rd := sse.NewReader(resp.Body)
	done := false
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			res.Status, res.ErrorCode = "failed", "upstream_stream_error"
			return res, nil
		}
		if bytes.Equal(ev.Data, []byte("[DONE]")) {
			done = true
		} else {
			var c chatChunk
			if json.Unmarshal(ev.Data, &c) == nil {
				res.read(&c)
				if res.FirstToken.IsZero() && c.hasContent() {
					res.FirstToken = time.Now()
				}
				if dropUsage && c.Usage != nil && len(c.Choices) == 0 {
					continue
				}
			}
		}
		if _, err := w.Write(append(append([]byte("data: "), ev.Data...), '\n', '\n')); err != nil {
			return res, nil
		}
		if flusher != nil {
			flusher.Flush()
		}
		if done {
			break
		}
	}
	if !done && res.ErrorCode == "" {
		res.Status, res.ErrorCode = "failed", "upstream_stream_error"
	}
	return res, nil
}

type chatChunk struct {
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        *struct {
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []any   `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *chatChunk) hasContent() bool {
	for _, ch := range c.Choices {
		if d := ch.Delta; d != nil && ((d.Content != nil && *d.Content != "") || (d.ReasoningContent != nil && *d.ReasoningContent != "") || len(d.ToolCalls) > 0) {
			return true
		}
	}
	return false
}

// read takes the status, usage and error from a completion or a chunk.
func (r *ChatResult) read(c *chatChunk) {
	for _, ch := range c.Choices {
		if ch.FinishReason == nil {
			continue
		}
		switch *ch.FinishReason {
		case "length", "content_filter":
			r.Status = "incomplete"
		default:
			if r.Status != "incomplete" {
				r.Status = "completed"
			}
		}
	}
	if u := c.Usage; u != nil {
		r.Usage = Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, Reported: true}
		if u.PromptTokensDetails != nil {
			r.Usage.CachedInputTokens = u.PromptTokensDetails.CachedTokens
		}
		if u.CompletionTokensDetails != nil {
			r.Usage.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
		}
	}
	if e := c.Error; e != nil {
		r.Status, r.ErrorCode = "failed", e.Type
		if s, ok := e.Code.(string); ok && s != "" {
			r.ErrorCode = s
		}
	}
}

// chatErrorCode reads the code of an OpenAI-style error body.
func chatErrorCode(b []byte) string {
	var c chatChunk
	if json.Unmarshal(b, &c) == nil && c.Error != nil {
		if s, ok := c.Error.Code.(string); ok && s != "" {
			return s
		}
		if c.Error.Type != "" {
			return c.Error.Type
		}
	}
	return "upstream_error"
}

func copyHeader(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}
