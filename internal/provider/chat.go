package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/sse"
)

// Chat translates Open Responses to the Chat Completions API, which most
// OpenAI-compatible servers speak (vLLM, Ollama, llama.cpp, LiteLLM, Groq,
// Together, OpenRouter and others), and translates the chunk stream back into
// Open Responses events.
type Chat struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
}

func NewChat(name, baseURL, apiKey string, headers map[string]string, client *http.Client) *Chat {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &Chat{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, headers: headers, client: client}
}

func (p *Chat) Name() string { return p.name }

// Chat Completions request types.

type chatRequest struct {
	Model             string          `json:"model"`
	Messages          []chatMessage   `json:"messages"`
	Tools             []chatTool      `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	PresencePenalty   *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty  *float64        `json:"frequency_penalty,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`
	ReasoningEffort   *string         `json:"reasoning_effort,omitempty"`
	PromptCacheKey    *string         `json:"prompt_cache_key,omitempty"`
	User              string          `json:"user,omitempty"`
	Stream            bool            `json:"stream"`
	StreamOptions     *chatStreamOpts `json:"stream_options,omitempty"`
}

type chatStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // string, []chatPart, or nil
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
	File     *chatFile     `json:"file,omitempty"`
}

type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type chatFile struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

func (p *Chat) buildRequest(call *Call, req *openresponses.Request) (*chatRequest, *openresponses.APIError) {
	out := &chatRequest{
		Model:            call.UpstreamModel,
		MaxTokens:        req.MaxOutputTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		PresencePenalty:  req.PresencePenalty,
		FrequencyPenalty: req.FrequencyPenalty,
		PromptCacheKey:   req.PromptCacheKey,
		Stream:           true,
		StreamOptions:    &chatStreamOpts{IncludeUsage: true},
	}
	if req.PreviousResponseID != nil && *req.PreviousResponseID != "" {
		return nil, openresponses.InvalidRequest("unsupported_parameter", "previous_response_id is not supported for this model; send the full conversation in input", "previous_response_id")
	}
	if req.Instructions != nil && *req.Instructions != "" {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: *req.Instructions})
	}
	if e := out.addInput(req); e != nil {
		return nil, e
	}
	if len(out.Messages) == 0 {
		return nil, openresponses.InvalidRequest("invalid_input", "input must contain at least one message", "input")
	}
	for i, t := range req.Tools {
		if t.Type != "function" {
			return nil, openresponses.InvalidRequest("unsupported_tool", "tool type "+t.Type+" is not supported for this model", fmt.Sprintf("tools[%d].type", i))
		}
		params := t.Parameters
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, chatTool{Type: "function", Function: chatFunction{Name: t.Name, Description: t.Description, Parameters: params, Strict: t.Strict}})
	}
	if e := out.applyToolChoice(req.ToolChoice); e != nil {
		return nil, e
	}
	if len(out.Tools) > 0 {
		out.ParallelToolCalls = req.ParallelToolCalls
	}
	if e := out.applyTextFormat(req.Text); e != nil {
		return nil, e
	}
	if req.Reasoning != nil && req.Reasoning.Effort != nil && *req.Reasoning.Effort != "" {
		out.ReasoningEffort = req.Reasoning.Effort
	}
	if req.SafetyIdentifier != nil {
		out.User = *req.SafetyIdentifier
	}
	return out, nil
}

func (r *chatRequest) addInput(req *openresponses.Request) *openresponses.APIError {
	items, err := req.InputItems()
	if err != nil {
		return openresponses.InvalidRequest("invalid_input", "input: "+err.Error(), "input")
	}
	for i, it := range items {
		param := fmt.Sprintf("input[%d]", i)
		switch it.Type {
		case "message":
			switch it.Role {
			case "system", "developer":
				text, e := plainText(it.Content, "input_text", param)
				if e != nil {
					return e
				}
				r.Messages = append(r.Messages, chatMessage{Role: "system", Content: text})
			case "user":
				parts, e := userParts(it.Content, param)
				if e != nil {
					return e
				}
				r.Messages = append(r.Messages, chatMessage{Role: "user", Content: textOrParts(parts)})
			case "assistant":
				text, e := plainText(it.Content, "output_text", param)
				if e != nil {
					return e
				}
				r.Messages = append(r.Messages, chatMessage{Role: "assistant", Content: text})
			default:
				return openresponses.InvalidRequest("invalid_input", "unknown message role "+it.Role, param+".role")
			}
		case "function_call":
			tc := chatToolCall{ID: it.CallID, Type: "function", Function: chatFunctionCall{Name: it.Name, Arguments: it.Arguments}}
			// Parallel calls, and the text before them, belong to one assistant message.
			if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == "assistant" {
				r.Messages[n-1].ToolCalls = append(r.Messages[n-1].ToolCalls, tc)
			} else {
				r.Messages = append(r.Messages, chatMessage{Role: "assistant", ToolCalls: []chatToolCall{tc}})
			}
		case "function_call_output":
			text, e := toolOutput(it.Output, param)
			if e != nil {
				return e
			}
			r.Messages = append(r.Messages, chatMessage{Role: "tool", ToolCallID: it.CallID, Content: text})
		case "reasoning":
			// Chat Completions has no way to replay reasoning.
		default:
			return openresponses.InvalidRequest("unsupported_input", "input item type "+it.Type+" is not supported for this model", param+".type")
		}
	}
	return nil
}

// plainText joins a message's text parts, for roles that only take text.
func plainText(content json.RawMessage, textType, param string) (string, *openresponses.APIError) {
	parts, err := openresponses.ContentParts(content, textType)
	if err != nil {
		return "", openresponses.InvalidRequest("invalid_input", err.Error(), param+".content")
	}
	var b strings.Builder
	for j, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			b.WriteString(part.Text)
		case "refusal":
			b.WriteString(part.Refusal)
		default:
			return "", openresponses.InvalidRequest("unsupported_input", "content type "+part.Type+" is not supported in this message", fmt.Sprintf("%s.content[%d].type", param, j))
		}
	}
	return b.String(), nil
}

func userParts(content json.RawMessage, param string) ([]chatPart, *openresponses.APIError) {
	parts, err := openresponses.ContentParts(content, "input_text")
	if err != nil {
		return nil, openresponses.InvalidRequest("invalid_input", err.Error(), param+".content")
	}
	out := make([]chatPart, 0, len(parts))
	for j, part := range parts {
		p, e := chatInputPart(part, fmt.Sprintf("%s.content[%d]", param, j))
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}

func chatInputPart(part openresponses.ContentPart, param string) (chatPart, *openresponses.APIError) {
	switch part.Type {
	case "input_text", "text", "output_text":
		return chatPart{Type: "text", Text: part.Text}, nil
	case "input_image":
		if part.ImageURL == nil || *part.ImageURL == "" {
			return chatPart{}, openresponses.InvalidRequest("invalid_input", "input_image requires image_url", param+".image_url")
		}
		img := &chatImageURL{URL: *part.ImageURL}
		if part.Detail != nil {
			img.Detail = *part.Detail
		}
		return chatPart{Type: "image_url", ImageURL: img}, nil
	case "input_file":
		if part.FileData == nil || *part.FileData == "" {
			return chatPart{}, openresponses.InvalidRequest("unsupported_input", "input_file needs file_data for this model; file_url is not supported", param)
		}
		f := &chatFile{FileData: *part.FileData}
		if part.Filename != nil {
			f.Filename = *part.Filename
		}
		return chatPart{Type: "file", File: f}, nil
	default:
		return chatPart{}, openresponses.InvalidRequest("unsupported_input", "content type "+part.Type+" is not supported for this model", param+".type")
	}
}

// textOrParts sends text-only content as a string, which servers without
// multimodal support also accept.
func textOrParts(parts []chatPart) any {
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return parts
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// toolOutput flattens a function_call_output to text, which is what every
// Chat Completions server accepts for tool messages.
func toolOutput(output json.RawMessage, param string) (string, *openresponses.APIError) {
	output = bytes.TrimSpace(output)
	if len(output) == 0 {
		return "", nil
	}
	if output[0] == '"' {
		var s string
		_ = json.Unmarshal(output, &s)
		return s, nil
	}
	return plainText(output, "input_text", param+".output")
}

func (r *chatRequest) applyToolChoice(raw json.RawMessage) *openresponses.APIError {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		switch s {
		case "auto", "required", "none":
			if len(r.Tools) > 0 {
				r.ToolChoice = s
			}
			return nil
		}
		return openresponses.InvalidRequest("invalid_tool_choice", "unknown tool_choice "+s, "tool_choice")
	}
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
		r.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": obj.Name}}
	case "allowed_tools":
		allowed := map[string]bool{}
		for _, t := range obj.Tools {
			allowed[t.Name] = true
		}
		kept := r.Tools[:0]
		for _, t := range r.Tools {
			if allowed[t.Function.Name] {
				kept = append(kept, t)
			}
		}
		r.Tools = kept
		if len(kept) > 0 {
			r.ToolChoice = "auto"
			if obj.Mode == "required" {
				r.ToolChoice = "required"
			}
		}
	default:
		return openresponses.InvalidRequest("invalid_tool_choice", "unsupported tool_choice type "+obj.Type, "tool_choice.type")
	}
	return nil
}

// applyTextFormat maps text.format to response_format.
func (r *chatRequest) applyTextFormat(raw json.RawMessage) *openresponses.APIError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var t struct {
		Format *struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description *string         `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return openresponses.InvalidRequest("invalid_type", "text: "+err.Error(), "text")
	}
	if t.Format == nil {
		return nil
	}
	var v any
	switch t.Format.Type {
	case "", "text":
		return nil
	case "json_object":
		v = map[string]string{"type": "json_object"}
	case "json_schema":
		v = map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": t.Format.Name, "description": t.Format.Description, "schema": t.Format.Schema, "strict": t.Format.Strict,
		}}
	default:
		return openresponses.InvalidRequest("unsupported_parameter", "text.format "+t.Format.Type+" is not supported for this model", "text.format.type")
	}
	r.ResponseFormat, _ = json.Marshal(v)
	return nil
}

// Chat Completions stream chunk.
type chatChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
			// Servers name streamed reasoning differently: vLLM and DeepSeek
			// use reasoning_content, OpenRouter and Ollama use reasoning.
			ReasoningContent *string `json:"reasoning_content"`
			Reasoning        *string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
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
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (p *Chat) Create(ctx context.Context, call *Call, sink openresponses.Sink) (*Result, error) {
	req, apiErr := call.Env.Request()
	if apiErr != nil {
		return nil, apiErr
	}
	creq, apiErr := p.buildRequest(call, req)
	if apiErr != nil {
		return nil, apiErr
	}
	body, err := json.Marshal(creq)
	if err != nil {
		return nil, openresponses.ServerError("proxy_error", err.Error())
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, openresponses.ServerError("proxy_error", err.Error())
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.apiKey)
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
		return nil, upstreamError(p.name, resp, openAIErrorMessage(resp.Body))
	}

	b := openresponses.NewBuilder(openresponses.NewResponse(call.ResponseID, call.Model, call.CreatedAt, req), sink)
	b.Start()
	var (
		rd        = sse.NewReader(resp.Body)
		usage     Usage
		finish    string
		reasoning = -1 // open output indexes, -1 when none
		message   = -1
		tools     = map[int]int{} // tool call index -> output index
		refusal   strings.Builder
	)
	result := func(r *openresponses.Response, status, code string) (*Result, error) {
		raw, _ := json.Marshal(r)
		return &Result{Response: raw, Status: status, Usage: usage, ErrorCode: code}, b.Err()
	}
	fail := func(e *openresponses.APIError) (*Result, error) {
		res, _ := result(b.Fail(e), "failed", e.CodeString())
		return res, e
	}
	endReasoning := func() {
		if reasoning >= 0 {
			b.EndReasoning(reasoning, nil)
			reasoning = -1
		}
	}
	endMessage := func(status string) {
		if message >= 0 {
			b.EndMessage(message, status)
			message = -1
		}
	}
	endTools := func(status string) {
		keys := make([]int, 0, len(tools))
		for k := range tools {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			b.EndFunctionCall(tools[k], status)
		}
		clear(tools)
	}

	done := false
	for !done {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(streamError(p.name, err))
		}
		if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
			done = true
			break
		}
		var c chatChunk
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			return fail(streamError(p.name, fmt.Errorf("bad chunk: %w", err)))
		}
		if c.Error != nil {
			return fail(openresponses.NewError(http.StatusBadGateway, openresponses.ErrModel, "upstream_error", p.name+": "+c.Error.Message, ""))
		}
		if c.Usage != nil {
			usage = Usage{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens, Reported: true}
			if d := c.Usage.PromptTokensDetails; d != nil {
				usage.CachedInputTokens = d.CachedTokens
			}
			if d := c.Usage.CompletionTokensDetails; d != nil {
				usage.ReasoningTokens = d.ReasoningTokens
			}
		}
		for _, ch := range c.Choices {
			if ch.Index != 0 {
				continue // n > 1 is never requested
			}
			d := ch.Delta
			if r := firstNonEmpty(d.ReasoningContent, d.Reasoning); r != "" {
				if reasoning < 0 {
					reasoning = b.StartReasoning()
				}
				b.ReasoningDelta(reasoning, r)
			}
			if d.Content != nil && *d.Content != "" {
				endReasoning()
				if message < 0 {
					endTools("completed")
					message = b.StartMessage()
				}
				b.TextDelta(message, *d.Content)
			}
			if d.Refusal != nil {
				refusal.WriteString(*d.Refusal)
			}
			for _, tc := range d.ToolCalls {
				idx, ok := tools[tc.Index]
				if !ok {
					endReasoning()
					endMessage("completed")
					id := tc.ID
					if id == "" {
						id = openresponses.NewID("call")
					}
					idx = b.StartFunctionCall(id, tc.Function.Name)
					tools[tc.Index] = idx
				}
				if tc.Function.Arguments != "" {
					b.ArgumentsDelta(idx, tc.Function.Arguments)
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
		if b.Err() != nil {
			// The client went away; stop reading so the upstream request is canceled.
			res, _ := result(b.Resp, "failed", "client_disconnected")
			return res, b.Err()
		}
	}
	// Some servers close the stream without [DONE]; a finish reason is enough.
	if !done && finish == "" {
		return fail(streamError(p.name, errors.New("stream ended before a finish reason")))
	}
	if refusal.Len() > 0 && message < 0 {
		message = b.StartMessage()
		b.TextDelta(message, refusal.String())
	}

	reason, itemStatus := "", "completed"
	switch finish {
	case "length":
		reason, itemStatus = "max_output_tokens", "incomplete"
	case "content_filter":
		reason, itemStatus = "content_filter", "incomplete"
	}
	endReasoning()
	endMessage(itemStatus)
	endTools(itemStatus)
	u := &openresponses.Usage{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		TotalTokens:         usage.InputTokens + usage.OutputTokens,
		InputTokensDetails:  openresponses.InputTokensDetails{CachedTokens: usage.CachedInputTokens},
		OutputTokensDetails: openresponses.OutputTokensDetails{ReasoningTokens: usage.ReasoningTokens},
	}
	r := b.Complete(u, reason)
	return result(r, r.Status, "")
}

func firstNonEmpty(ss ...*string) string {
	for _, s := range ss {
		if s != nil && *s != "" {
			return *s
		}
	}
	return ""
}
