package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/sse"
)

const anthropicVersion = "2023-06-01"

// Anthropic translates Open Responses to the Anthropic Messages API and
// translates the Messages event stream back into Open Responses events.
type Anthropic struct {
	name             string
	baseURL          string
	apiKey           string
	headers          map[string]string
	defaultMaxTokens int
	client           *http.Client
}

func NewAnthropic(name, baseURL, apiKey string, headers map[string]string, defaultMaxTokens int, client *http.Client) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if defaultMaxTokens <= 0 {
		defaultMaxTokens = 8192
	}
	return &Anthropic{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, headers: headers, defaultMaxTokens: defaultMaxTokens, client: client}
}

func (p *Anthropic) Name() string { return p.name }

// Messages API request types.

type anthRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	System      []anthBlock     `json:"system,omitempty"`
	Messages    []anthMessage   `json:"messages"`
	Tools       []anthTool      `json:"tools,omitempty"`
	ToolChoice  *anthToolChoice `json:"tool_choice,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Thinking    *anthThinking   `json:"thinking,omitempty"`
	Metadata    *anthMetadata   `json:"metadata,omitempty"`
	Stream      bool            `json:"stream"`
}

type anthMessage struct {
	Role    string      `json:"role"`
	Content []anthBlock `json:"content"`
}

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *anthSource     `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

type anthSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type anthThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthMetadata struct {
	UserID string `json:"user_id"`
}

// redactedPrefix marks encrypted_content that carries Anthropic redacted
// thinking data rather than a thinking signature.
const redactedPrefix = "anthropic-redacted:"

var thinkingBudgets = map[string]int{"low": 1024, "medium": 4096, "high": 16384, "xhigh": 32768}

// buildAnthropicRequest translates an Open Responses request.
func (p *Anthropic) buildRequest(call *Call) (*anthRequest, *openresponses.APIError) {
	req := call.Req
	out := &anthRequest{Model: call.UpstreamModel, MaxTokens: p.defaultMaxTokens, Stream: true, Temperature: req.Temperature, TopP: req.TopP}
	if req.MaxOutputTokens != nil {
		out.MaxTokens = *req.MaxOutputTokens
	}
	if req.Instructions != nil && *req.Instructions != "" {
		out.System = append(out.System, anthBlock{Type: "text", Text: *req.Instructions})
	}
	if req.PreviousResponseID != nil && *req.PreviousResponseID != "" {
		return nil, openresponses.InvalidRequest("unsupported_parameter", "previous_response_id is not supported for this model yet; send the full conversation in input", "previous_response_id")
	}
	if len(req.Text) > 0 {
		var t struct {
			Format struct {
				Type string `json:"type"`
			} `json:"format"`
		}
		if json.Unmarshal(req.Text, &t) == nil && t.Format.Type != "" && t.Format.Type != "text" {
			return nil, openresponses.InvalidRequest("unsupported_parameter", "text.format "+t.Format.Type+" is not supported for this model yet", "text.format")
		}
	}

	items, err := req.InputItems()
	if err != nil {
		return nil, openresponses.InvalidRequest("invalid_input", "input: "+err.Error(), "input")
	}
	for i, it := range items {
		param := fmt.Sprintf("input[%d]", i)
		switch it.Type {
		case "message":
			switch it.Role {
			case "system", "developer":
				parts, err := openresponses.ContentParts(it.Content, "input_text")
				if err != nil {
					return nil, openresponses.InvalidRequest("invalid_input", err.Error(), param)
				}
				for _, part := range parts {
					if part.Text != "" {
						out.System = append(out.System, anthBlock{Type: "text", Text: part.Text})
					}
				}
			case "user":
				blocks, e := userBlocks(it.Content, param)
				if e != nil {
					return nil, e
				}
				out.appendBlocks("user", blocks...)
			case "assistant":
				parts, err := openresponses.ContentParts(it.Content, "output_text")
				if err != nil {
					return nil, openresponses.InvalidRequest("invalid_input", err.Error(), param)
				}
				for _, part := range parts {
					text := part.Text
					if part.Type == "refusal" {
						text = part.Refusal
					}
					if text != "" {
						out.appendBlocks("assistant", anthBlock{Type: "text", Text: text})
					}
				}
			default:
				return nil, openresponses.InvalidRequest("invalid_input", "unknown message role "+it.Role, param+".role")
			}
		case "function_call":
			args := json.RawMessage(it.Arguments)
			if !json.Valid(args) || len(bytes.TrimSpace(args)) == 0 || args[0] != '{' {
				args = json.RawMessage("{}")
			}
			out.appendBlocks("assistant", anthBlock{Type: "tool_use", ID: it.CallID, Name: it.Name, Input: args})
		case "function_call_output":
			content, e := toolResultContent(it.Output, param)
			if e != nil {
				return nil, e
			}
			out.appendBlocks("user", anthBlock{Type: "tool_result", ToolUseID: it.CallID, Content: content})
		case "reasoning":
			if it.EncryptedContent == nil {
				continue // reasoning from another provider cannot be replayed here
			}
			enc := *it.EncryptedContent
			if data, ok := strings.CutPrefix(enc, redactedPrefix); ok {
				out.appendBlocks("assistant", anthBlock{Type: "redacted_thinking", Data: data})
				continue
			}
			var text strings.Builder
			var content []openresponses.ContentPart
			_ = json.Unmarshal(it.Content, &content)
			for _, c := range content {
				text.WriteString(c.Text)
			}
			if text.Len() == 0 {
				for _, s := range it.Summary {
					text.WriteString(s.Text)
				}
			}
			out.appendBlocks("assistant", anthBlock{Type: "thinking", Thinking: text.String(), Signature: enc})
		default:
			return nil, openresponses.InvalidRequest("unsupported_input", "input item type "+it.Type+" is not supported for this model yet", param+".type")
		}
	}
	if len(out.Messages) == 0 {
		return nil, openresponses.InvalidRequest("invalid_input", "input must contain at least one user or assistant item", "input")
	}
	// The Messages API requires the conversation to start with a user turn.
	if out.Messages[0].Role != "user" {
		out.Messages = append([]anthMessage{{Role: "user", Content: []anthBlock{{Type: "text", Text: "(conversation start)"}}}}, out.Messages...)
	}

	for i, t := range req.Tools {
		if t.Type != "function" {
			return nil, openresponses.InvalidRequest("unsupported_tool", "tool type "+t.Type+" is not supported for this model", fmt.Sprintf("tools[%d].type", i))
		}
		schema := t.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		at := anthTool{Name: t.Name, InputSchema: schema}
		if t.Description != nil {
			at.Description = *t.Description
		}
		out.Tools = append(out.Tools, at)
	}
	if e := out.applyToolChoice(req); e != nil {
		return nil, e
	}

	if req.Reasoning != nil && req.Reasoning.Effort != nil {
		if budget, ok := thinkingBudgets[*req.Reasoning.Effort]; ok {
			out.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
			if out.MaxTokens <= budget {
				out.MaxTokens = budget + p.defaultMaxTokens
			}
			// Extended thinking requires default sampling parameters.
			out.Temperature, out.TopP = nil, nil
		}
	}
	if req.SafetyIdentifier != nil && *req.SafetyIdentifier != "" {
		// Anthropic asks for an opaque id, and safety identifiers are often emails.
		sum := sha256.Sum256([]byte(*req.SafetyIdentifier))
		out.Metadata = &anthMetadata{UserID: hex.EncodeToString(sum[:16])}
	}
	return out, nil
}

// appendBlocks adds blocks to the last message when it has the same role,
// since the Messages API expects alternating turns.
func (r *anthRequest) appendBlocks(role string, blocks ...anthBlock) {
	if len(blocks) == 0 {
		return
	}
	if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == role {
		r.Messages[n-1].Content = append(r.Messages[n-1].Content, blocks...)
		return
	}
	r.Messages = append(r.Messages, anthMessage{Role: role, Content: blocks})
}

func (r *anthRequest) applyToolChoice(req *openresponses.Request) *openresponses.APIError {
	disableParallel := req.ParallelToolCalls != nil && !*req.ParallelToolCalls
	raw := bytes.TrimSpace(req.ToolChoice)
	var choice anthToolChoice
	switch {
	case len(raw) == 0 || string(raw) == "null":
		if len(r.Tools) == 0 {
			return nil
		}
		choice.Type = "auto"
	case raw[0] == '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		switch s {
		case "auto":
			choice.Type = "auto"
		case "required":
			choice.Type = "any"
		case "none":
			choice.Type = "none"
		default:
			return openresponses.InvalidRequest("invalid_tool_choice", "unknown tool_choice "+s, "tool_choice")
		}
	default:
		var obj struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Mode  string `json:"mode"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return openresponses.InvalidRequest("invalid_tool_choice", err.Error(), "tool_choice")
		}
		switch obj.Type {
		case "function":
			choice = anthToolChoice{Type: "tool", Name: obj.Name}
		case "allowed_tools":
			allowed := map[string]bool{}
			for _, t := range obj.Tools {
				allowed[t.Name] = true
			}
			kept := r.Tools[:0]
			for _, t := range r.Tools {
				if allowed[t.Name] {
					kept = append(kept, t)
				}
			}
			r.Tools = kept
			choice.Type = "auto"
			if obj.Mode == "required" {
				choice.Type = "any"
			}
		default:
			return openresponses.InvalidRequest("invalid_tool_choice", "unsupported tool_choice type "+obj.Type, "tool_choice.type")
		}
	}
	if len(r.Tools) == 0 {
		return nil
	}
	if disableParallel && choice.Type != "none" {
		choice.DisableParallelToolUse = true
	}
	r.ToolChoice = &choice
	return nil
}

func userBlocks(content json.RawMessage, param string) ([]anthBlock, *openresponses.APIError) {
	parts, err := openresponses.ContentParts(content, "input_text")
	if err != nil {
		return nil, openresponses.InvalidRequest("invalid_input", err.Error(), param+".content")
	}
	blocks := make([]anthBlock, 0, len(parts))
	for j, part := range parts {
		b, e := inputPartBlock(part, fmt.Sprintf("%s.content[%d]", param, j))
		if e != nil {
			return nil, e
		}
		if b != nil {
			blocks = append(blocks, *b)
		}
	}
	return blocks, nil
}

func inputPartBlock(part openresponses.ContentPart, param string) (*anthBlock, *openresponses.APIError) {
	switch part.Type {
	case "input_text", "text", "output_text":
		if part.Text == "" {
			return nil, nil
		}
		return &anthBlock{Type: "text", Text: part.Text}, nil
	case "input_image":
		if part.ImageURL == nil || *part.ImageURL == "" {
			return nil, openresponses.InvalidRequest("invalid_input", "input_image requires image_url", param+".image_url")
		}
		src, ok := sourceFromURL(*part.ImageURL, "")
		if !ok {
			return nil, openresponses.InvalidRequest("invalid_input", "image_url must be an http(s) URL or a base64 data URL", param+".image_url")
		}
		return &anthBlock{Type: "image", Source: src}, nil
	case "input_file":
		switch {
		case part.FileData != nil && *part.FileData != "":
			src, ok := sourceFromURL(*part.FileData, "application/pdf")
			if !ok {
				return nil, openresponses.InvalidRequest("invalid_input", "file_data must be base64 data", param+".file_data")
			}
			return &anthBlock{Type: "document", Source: src}, nil
		case part.FileURL != nil && *part.FileURL != "":
			return &anthBlock{Type: "document", Source: &anthSource{Type: "url", URL: *part.FileURL}}, nil
		}
		return nil, openresponses.InvalidRequest("invalid_input", "input_file requires file_data or file_url", param)
	default:
		return nil, openresponses.InvalidRequest("unsupported_input", "content type "+part.Type+" is not supported for this model", param+".type")
	}
}

// sourceFromURL accepts a data URL, an http(s) URL, or (when defaultMedia is
// set) bare base64 data.
func sourceFromURL(u, defaultMedia string) (*anthSource, bool) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		media, isB64 := strings.CutSuffix(meta, ";base64")
		if !ok || !isB64 {
			return nil, false
		}
		return &anthSource{Type: "base64", MediaType: media, Data: data}, true
	}
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return &anthSource{Type: "url", URL: u}, true
	}
	if defaultMedia != "" {
		return &anthSource{Type: "base64", MediaType: defaultMedia, Data: u}, true
	}
	return nil, false
}

func toolResultContent(output json.RawMessage, param string) (any, *openresponses.APIError) {
	output = bytes.TrimSpace(output)
	if len(output) == 0 {
		return "", nil
	}
	if output[0] == '"' {
		var s string
		_ = json.Unmarshal(output, &s)
		return s, nil
	}
	var parts []openresponses.ContentPart
	if err := json.Unmarshal(output, &parts); err != nil {
		return nil, openresponses.InvalidRequest("invalid_input", err.Error(), param+".output")
	}
	blocks := make([]anthBlock, 0, len(parts))
	for j, part := range parts {
		b, e := inputPartBlock(part, fmt.Sprintf("%s.output[%d]", param, j))
		if e != nil {
			return nil, e
		}
		if b != nil {
			blocks = append(blocks, *b)
		}
	}
	return blocks, nil
}

// Messages API streaming event.
type anthEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage anthUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
}

func (u *anthUsage) mergeInto(dst *anthUsage) {
	if u.InputTokens != nil {
		dst.InputTokens = u.InputTokens
	}
	if u.OutputTokens != nil {
		dst.OutputTokens = u.OutputTokens
	}
	if u.CacheCreationInputTokens != nil {
		dst.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens != nil {
		dst.CacheReadInputTokens = u.CacheReadInputTokens
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (p *Anthropic) Create(ctx context.Context, call *Call, sink openresponses.Sink) (*Result, error) {
	areq, apiErr := p.buildRequest(call)
	if apiErr != nil {
		return nil, apiErr
	}
	body, err := json.Marshal(areq)
	if err != nil {
		return nil, openresponses.ServerError("proxy_error", err.Error())
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, openresponses.ServerError("proxy_error", err.Error())
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	hreq.Header.Set("anthropic-version", anthropicVersion)
	if p.apiKey != "" {
		hreq.Header.Set("x-api-key", p.apiKey)
	}
	for k, v := range p.headers {
		hreq.Header.Set(k, v)
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, networkError(p.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, upstreamError(p.name, resp.StatusCode, anthropicErrorMessage(resp.Body))
	}

	b := openresponses.NewBuilder(openresponses.NewResponse(call.ResponseID, call.Model, call.CreatedAt, call.Req), sink)
	b.Start()
	var (
		rd         = sse.NewReader(resp.Body)
		usage      anthUsage
		stopReason string
		blockIdx   = map[int]int{} // Anthropic content block index -> output index
		blockType  = map[int]string{}
		signatures = map[int]string{}
		finished   bool
	)
	result := func(r *openresponses.Response, status, code string) (*Result, error) {
		raw, _ := json.Marshal(r)
		u := Usage{
			InputTokens:       deref(usage.InputTokens) + deref(usage.CacheReadInputTokens) + deref(usage.CacheCreationInputTokens),
			CachedInputTokens: deref(usage.CacheReadInputTokens),
			CacheWriteTokens:  deref(usage.CacheCreationInputTokens),
			OutputTokens:      deref(usage.OutputTokens),
			Reported:          usage.InputTokens != nil || usage.OutputTokens != nil,
		}
		return &Result{Response: raw, Status: status, Usage: u, ErrorCode: code}, b.Err()
	}
	fail := func(e *openresponses.APIError) (*Result, error) {
		r := b.Fail(e)
		res, _ := result(r, "failed", e.CodeString())
		return res, e
	}

loop:
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(streamError(p.name, err))
		}
		var e anthEvent
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return fail(streamError(p.name, fmt.Errorf("bad event: %w", err)))
		}
		switch e.Type {
		case "message_start":
			if e.Message != nil {
				e.Message.Usage.mergeInto(&usage)
			}
		case "content_block_start":
			cb := e.ContentBlock
			if cb == nil {
				continue
			}
			blockType[e.Index] = cb.Type
			switch cb.Type {
			case "text":
				idx := b.StartMessage()
				blockIdx[e.Index] = idx
				if cb.Text != "" {
					b.TextDelta(idx, cb.Text)
				}
			case "tool_use":
				blockIdx[e.Index] = b.StartFunctionCall(cb.ID, cb.Name)
			case "thinking":
				idx := b.StartReasoning()
				blockIdx[e.Index] = idx
				if cb.Thinking != "" {
					b.ReasoningDelta(idx, cb.Thinking)
				}
				signatures[e.Index] = cb.Signature
			case "redacted_thinking":
				enc := redactedPrefix + cb.Data
				b.AddReasoning(&enc)
			}
		case "content_block_delta":
			idx, ok := blockIdx[e.Index]
			if !ok || e.Delta == nil {
				continue
			}
			switch e.Delta.Type {
			case "text_delta":
				b.TextDelta(idx, e.Delta.Text)
			case "input_json_delta":
				b.ArgumentsDelta(idx, e.Delta.PartialJSON)
			case "thinking_delta":
				b.ReasoningDelta(idx, e.Delta.Thinking)
			case "signature_delta":
				signatures[e.Index] += e.Delta.Signature
			}
		case "content_block_stop":
			idx, ok := blockIdx[e.Index]
			if !ok {
				continue
			}
			switch blockType[e.Index] {
			case "text":
				b.EndMessage(idx, "completed")
			case "tool_use":
				b.EndFunctionCall(idx, "completed")
			case "thinking":
				var enc *string
				if s := signatures[e.Index]; s != "" {
					enc = &s
				}
				b.EndReasoning(idx, enc)
			}
			delete(blockIdx, e.Index)
		case "message_delta":
			if e.Delta != nil && e.Delta.StopReason != "" {
				stopReason = e.Delta.StopReason
			}
			if e.Usage != nil {
				e.Usage.mergeInto(&usage)
			}
		case "message_stop":
			finished = true
			break loop
		case "error":
			msg := "upstream error"
			typ := "api_error"
			if e.Error != nil {
				msg, typ = e.Error.Message, e.Error.Type
			}
			if typ == "overloaded_error" {
				return fail(openresponses.NewError(http.StatusServiceUnavailable, openresponses.ErrServer, "upstream_overloaded", msg, ""))
			}
			return fail(openresponses.NewError(http.StatusBadGateway, openresponses.ErrModel, typ, msg, ""))
		}
		if b.Err() != nil {
			// The client went away; stop reading so the upstream request is canceled.
			res, _ := result(b.Resp, "failed", "client_disconnected")
			return res, b.Err()
		}
	}
	if !finished {
		return fail(streamError(p.name, errors.New("stream ended before message_stop")))
	}

	reason := ""
	itemStatus := "completed"
	switch stopReason {
	case "max_tokens":
		reason, itemStatus = "max_output_tokens", "incomplete"
	case "model_context_window_exceeded":
		reason, itemStatus = "max_input_tokens", "incomplete"
	case "pause_turn":
		reason = "pause_turn"
	}
	// Close any blocks the upstream left open.
	for ai, idx := range blockIdx {
		switch blockType[ai] {
		case "text":
			b.EndMessage(idx, itemStatus)
		case "tool_use":
			b.EndFunctionCall(idx, itemStatus)
		case "thinking":
			b.EndReasoning(idx, nil)
		}
	}
	u := &openresponses.Usage{
		InputTokens:        deref(usage.InputTokens) + deref(usage.CacheReadInputTokens) + deref(usage.CacheCreationInputTokens),
		OutputTokens:       deref(usage.OutputTokens),
		InputTokensDetails: openresponses.InputTokensDetails{CachedTokens: deref(usage.CacheReadInputTokens)},
	}
	u.TotalTokens = u.InputTokens + u.OutputTokens
	r := b.Complete(u, reason)
	return result(r, r.Status, "")
}

func anthropicErrorMessage(r io.Reader) string {
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
