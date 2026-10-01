package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/sse"
)

// OpenAI forwards requests to any upstream that already speaks Open Responses
// (OpenAI's Responses API, or another compliant server). Events are relayed
// byte for byte; the adapter only peeks at each event's type to capture the
// terminal response and its usage.
type OpenAI struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
}

func NewOpenAI(name, baseURL, apiKey string, headers map[string]string, client *http.Client) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAI{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, headers: headers, client: client}
}

func (p *OpenAI) Name() string { return p.name }

func (p *OpenAI) Create(ctx context.Context, call *Call, sink openresponses.Sink) (*Result, error) {
	body := call.Env.UpstreamBody(call.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, openresponses.ServerError("proxy_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
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
	if resp.StatusCode/100 != 2 {
		return nil, upstreamError(p.name, resp, openAIErrorMessage(resp.Body))
	}

	var (
		rd      = sse.NewReader(resp.Body)
		res     = &Result{Status: "failed"}
		lastSeq = -1
		done    bool
	)
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, p.failStream(sink, lastSeq, streamError(p.name, err))
		}
		if bytes.Equal(ev.Data, []byte("[DONE]")) {
			break
		}
		var head struct {
			Type     string          `json:"type"`
			Seq      *int            `json:"sequence_number"`
			Response json.RawMessage `json:"response"`
			Error    *struct {
				Code    *string `json:"code"`
				Type    string  `json:"type"`
				Message string  `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(ev.Data, &head); err != nil {
			return res, p.failStream(sink, lastSeq, streamError(p.name, fmt.Errorf("bad event: %w", err)))
		}
		if head.Seq != nil {
			lastSeq = *head.Seq
		}
		name := ev.Name
		if name == "" {
			name = head.Type
		}
		if sink != nil {
			if err := sink.RawEvent(name, ev.Data); err != nil {
				return res, err
			}
		}
		switch head.Type {
		case "response.completed", "response.incomplete", "response.failed":
			done = true
			res.Response = append(json.RawMessage(nil), head.Response...)
			var final struct {
				Status string                       `json:"status"`
				Usage  *openresponses.Usage         `json:"usage"`
				Error  *openresponses.ResponseError `json:"error"`
			}
			if err := json.Unmarshal(head.Response, &final); err == nil {
				res.Status = final.Status
				if final.Usage != nil {
					res.Usage = fromSpecUsage(final.Usage)
				}
				if final.Error != nil && res.ErrorCode == "" {
					res.ErrorCode = final.Error.Code
				}
			}
		case "error":
			if head.Error != nil {
				res.ErrorCode = head.Error.Type
				if head.Error.Code != nil {
					res.ErrorCode = *head.Error.Code
				}
			}
		}
	}
	if !done {
		return res, p.failStream(sink, lastSeq, streamError(p.name, errors.New("stream ended before a terminal event")))
	}
	return res, nil
}

// failStream reports a mid-stream failure to a streaming client.
func (p *OpenAI) failStream(sink openresponses.Sink, lastSeq int, e *openresponses.APIError) error {
	if sink != nil && lastSeq >= 0 {
		_ = sink.Event("error", map[string]any{
			"type":            "error",
			"sequence_number": lastSeq + 1,
			"error":           openresponses.ErrorPayload{Type: e.Type, Code: e.Code, Message: e.Message, Param: e.Param},
		})
	}
	return e
}

func fromSpecUsage(u *openresponses.Usage) Usage {
	return Usage{
		InputTokens:       u.InputTokens,
		CachedInputTokens: u.InputTokensDetails.CachedTokens,
		OutputTokens:      u.OutputTokens,
		ReasoningTokens:   u.OutputTokensDetails.ReasoningTokens,
		Reported:          true,
	}
}

func openAIErrorMessage(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 64*1024))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(b))
}

// Get forwards a GET, such as /models, and returns the upstream's reply for
// the caller to relay. The caller closes its body.
func (p *OpenAI) Get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return nil, openresponses.ServerError("proxy_error", err.Error())
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
	return resp, nil
}
