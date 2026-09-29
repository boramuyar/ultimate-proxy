// Package openresponses holds the wire types of the Open Responses API
// (https://www.openresponses.org, spec 2026-04-24) that the proxy needs to
// read requests and to build spec-compliant responses and streaming events.
package openresponses

import (
	"bytes"
	"encoding/json"
)

// Request is the body of POST /v1/responses. Fields the proxy only needs to
// echo back or pass through are kept as raw JSON.
type Request struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	PreviousResponseID *string           `json:"previous_response_id"`
	Include            []string          `json:"include"`
	Tools              []Tool            `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"`
	Metadata           map[string]string `json:"metadata"`
	Text               json.RawMessage   `json:"text"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	PresencePenalty    *float64          `json:"presence_penalty"`
	FrequencyPenalty   *float64          `json:"frequency_penalty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
	Stream             bool              `json:"stream"`
	Background         *bool             `json:"background"`
	MaxOutputTokens    *int              `json:"max_output_tokens"`
	MaxToolCalls       *int              `json:"max_tool_calls"`
	Reasoning          *ReasoningParam   `json:"reasoning"`
	SafetyIdentifier   *string           `json:"safety_identifier"`
	PromptCacheKey     *string           `json:"prompt_cache_key"`
	Truncation         *string           `json:"truncation"`
	Instructions       *string           `json:"instructions"`
	Store              *bool             `json:"store"`
	ServiceTier        *string           `json:"service_tier"`
	TopLogprobs        *int              `json:"top_logprobs"`
}

type ReasoningParam struct {
	Effort  *string `json:"effort"`
	Summary *string `json:"summary"`
}

// Tool is a function tool. Hosted tool types keep their type so adapters can
// reject what they cannot serve.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// Item is the union of every input item type. Only the fields relevant to
// Type are set.
type Item struct {
	Type             string          `json:"type"`
	ID               string          `json:"id,omitempty"`
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	Phase            string          `json:"phase,omitempty"`
	Status           string          `json:"status,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        string          `json:"arguments,omitempty"`
	Output           json.RawMessage `json:"output,omitempty"`
	Summary          []ContentPart   `json:"summary,omitempty"`
	EncryptedContent *string         `json:"encrypted_content,omitempty"`
}

// ContentPart is the union of input and output content parts.
type ContentPart struct {
	Type     string  `json:"type"`
	Text     string  `json:"text,omitempty"`
	Refusal  string  `json:"refusal,omitempty"`
	ImageURL *string `json:"image_url,omitempty"`
	Detail   *string `json:"detail,omitempty"`
	Filename *string `json:"filename,omitempty"`
	FileData *string `json:"file_data,omitempty"`
	FileURL  *string `json:"file_url,omitempty"`
}

// InputItems normalizes Request.Input, which may be a bare string or a list of
// items, into a list of items.
func (r *Request) InputItems() ([]Item, error) {
	in := bytes.TrimSpace(r.Input)
	if len(in) == 0 || bytes.Equal(in, []byte("null")) {
		return nil, nil
	}
	if in[0] == '"' {
		var s string
		if err := json.Unmarshal(in, &s); err != nil {
			return nil, err
		}
		content, _ := json.Marshal(s)
		return []Item{{Type: "message", Role: "user", Content: content}}, nil
	}
	var items []Item
	if err := json.Unmarshal(in, &items); err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Type == "" && items[i].Role != "" {
			items[i].Type = "message"
		}
	}
	return items, nil
}

// ContentParts normalizes message content, which may be a string or a list of
// parts. A string becomes a single part of type textType.
func ContentParts(raw json.RawMessage, textType string) ([]ContentPart, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []ContentPart{{Type: textType, Text: s}}, nil
	}
	var parts []ContentPart
	err := json.Unmarshal(raw, &parts)
	return parts, err
}

// Usage is the spec usage object.
type Usage struct {
	InputTokens         int                 `json:"input_tokens"`
	OutputTokens        int                 `json:"output_tokens"`
	TotalTokens         int                 `json:"total_tokens"`
	InputTokensDetails  InputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails OutputTokensDetails `json:"output_tokens_details"`
}

type InputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type IncompleteDetails struct {
	Reason string `json:"reason"`
}

type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Reasoning struct {
	Effort  *string `json:"effort"`
	Summary *string `json:"summary"`
}

// Response is the spec ResponseResource. Every required field is always
// serialized, using null where the spec allows it.
type Response struct {
	ID                 string             `json:"id"`
	Object             string             `json:"object"`
	CreatedAt          int64              `json:"created_at"`
	CompletedAt        *int64             `json:"completed_at"`
	Status             string             `json:"status"`
	IncompleteDetails  *IncompleteDetails `json:"incomplete_details"`
	Model              string             `json:"model"`
	PreviousResponseID *string            `json:"previous_response_id"`
	Instructions       *string            `json:"instructions"`
	Output             []any              `json:"output"`
	Error              *ResponseError     `json:"error"`
	Tools              []Tool             `json:"tools"`
	ToolChoice         json.RawMessage    `json:"tool_choice"`
	Truncation         string             `json:"truncation"`
	ParallelToolCalls  bool               `json:"parallel_tool_calls"`
	Text               json.RawMessage    `json:"text"`
	TopP               float64            `json:"top_p"`
	PresencePenalty    float64            `json:"presence_penalty"`
	FrequencyPenalty   float64            `json:"frequency_penalty"`
	TopLogprobs        int                `json:"top_logprobs"`
	Temperature        float64            `json:"temperature"`
	Reasoning          *Reasoning         `json:"reasoning"`
	Usage              *Usage             `json:"usage"`
	MaxOutputTokens    *int               `json:"max_output_tokens"`
	MaxToolCalls       *int               `json:"max_tool_calls"`
	Store              bool               `json:"store"`
	Background         bool               `json:"background"`
	ServiceTier        string             `json:"service_tier"`
	Metadata           map[string]string  `json:"metadata"`
	SafetyIdentifier   *string            `json:"safety_identifier"`
	PromptCacheKey     *string            `json:"prompt_cache_key"`
}

var (
	defaultToolChoice = json.RawMessage(`"auto"`)
	defaultText       = json.RawMessage(`{"format":{"type":"text"}}`)
)

// NewResponse creates an in-progress response that echoes the request's
// configuration, as the spec requires.
func NewResponse(id, model string, createdAt int64, req *Request) *Response {
	r := &Response{
		ID:                 id,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		PreviousResponseID: req.PreviousResponseID,
		Instructions:       req.Instructions,
		Output:             []any{},
		ToolChoice:         req.ToolChoice,
		Truncation:         "disabled",
		ParallelToolCalls:  true,
		Text:               req.Text,
		TopP:               1,
		Temperature:        1,
		Reasoning:          &Reasoning{},
		MaxOutputTokens:    req.MaxOutputTokens,
		MaxToolCalls:       req.MaxToolCalls,
		Store:              req.Store == nil || *req.Store,
		ServiceTier:        "default",
		Metadata:           req.Metadata,
		SafetyIdentifier:   req.SafetyIdentifier,
		PromptCacheKey:     req.PromptCacheKey,
	}
	r.Tools = make([]Tool, len(req.Tools))
	for i, t := range req.Tools {
		if len(t.Parameters) == 0 {
			t.Parameters = json.RawMessage("null")
		}
		r.Tools[i] = t
	}
	if len(r.ToolChoice) == 0 || string(r.ToolChoice) == "null" {
		r.ToolChoice = defaultToolChoice
	}
	if len(r.Text) == 0 || string(r.Text) == "null" {
		r.Text = defaultText
	}
	if req.Truncation != nil {
		r.Truncation = *req.Truncation
	}
	if req.ParallelToolCalls != nil {
		r.ParallelToolCalls = *req.ParallelToolCalls
	}
	if req.TopP != nil {
		r.TopP = *req.TopP
	}
	if req.Temperature != nil {
		r.Temperature = *req.Temperature
	}
	if req.PresencePenalty != nil {
		r.PresencePenalty = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		r.FrequencyPenalty = *req.FrequencyPenalty
	}
	if req.TopLogprobs != nil {
		r.TopLogprobs = *req.TopLogprobs
	}
	if req.Reasoning != nil {
		r.Reasoning = &Reasoning{Effort: req.Reasoning.Effort, Summary: req.Reasoning.Summary}
	}
	if req.ServiceTier != nil && *req.ServiceTier != "auto" {
		r.ServiceTier = *req.ServiceTier
	}
	if req.Background != nil {
		r.Background = *req.Background
	}
	if r.Metadata == nil {
		r.Metadata = map[string]string{}
	}
	return r
}

// Output item types.

type MessageItem struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Status  string       `json:"status"`
	Role    string       `json:"role"`
	Content []OutputText `json:"content"`
	Phase   string       `json:"phase,omitempty"`
}

type OutputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

type FunctionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type ReasoningItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Content          []ReasoningText `json:"content,omitempty"`
	Summary          []ReasoningText `json:"summary"`
	EncryptedContent *string         `json:"encrypted_content,omitempty"`
}

// ReasoningText is used for both reasoning_text and summary_text parts.
type ReasoningText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
