package openresponses

import (
	"strings"
	"time"
)

// Sink receives streaming events in order. Event values are serialized
// synchronously, so callers may mutate them afterwards.
type Sink interface {
	Event(typ string, v any) error
	// RawEvent forwards an already-encoded event (used by passthrough adapters).
	RawEvent(typ string, data []byte) error
}

// Builder produces a spec-compliant event sequence and the final response for
// adapters that translate from a non-Open-Responses upstream. When the sink is
// nil, it only accumulates the final response.
type Builder struct {
	Resp  *Response
	sink  Sink
	seq   int
	texts []*strings.Builder // per output index: message text, reasoning text or call arguments
	err   error
}

func NewBuilder(resp *Response, sink Sink) *Builder {
	return &Builder{Resp: resp, sink: sink}
}

// Err returns the first sink error (for example, the client disconnected).
func (b *Builder) Err() error { return b.err }

func (b *Builder) emit(typ string, v any) {
	if b.sink == nil || b.err != nil {
		b.seq++
		return
	}
	b.err = b.sink.Event(typ, v)
	b.seq++
}

type responseEvent struct {
	Type     string    `json:"type"`
	Seq      int       `json:"sequence_number"`
	Response *Response `json:"response"`
}

type itemEvent struct {
	Type        string `json:"type"`
	Seq         int    `json:"sequence_number"`
	OutputIndex int    `json:"output_index"`
	Item        any    `json:"item"`
}

type partEvent struct {
	Type         string `json:"type"`
	Seq          int    `json:"sequence_number"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Part         any    `json:"part"`
}

type textDeltaEvent struct {
	Type         string `json:"type"`
	Seq          int    `json:"sequence_number"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
	Logprobs     []any  `json:"logprobs,omitempty"`
}

type textDoneEvent struct {
	Type         string `json:"type"`
	Seq          int    `json:"sequence_number"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
	Logprobs     []any  `json:"logprobs,omitempty"`
}

type argsDeltaEvent struct {
	Type        string `json:"type"`
	Seq         int    `json:"sequence_number"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
}

type argsDoneEvent struct {
	Type        string `json:"type"`
	Seq         int    `json:"sequence_number"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Arguments   string `json:"arguments"`
}

// ErrorPayload is the error object inside a streaming error event.
type ErrorPayload struct {
	Type    string  `json:"type"`
	Code    *string `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

type errorEvent struct {
	Type  string       `json:"type"`
	Seq   int          `json:"sequence_number"`
	Error ErrorPayload `json:"error"`
}

// Start emits response.created and response.in_progress.
func (b *Builder) Start() {
	b.emit("response.created", &responseEvent{"response.created", b.seq, b.Resp})
	b.emit("response.in_progress", &responseEvent{"response.in_progress", b.seq, b.Resp})
}

func (b *Builder) add(item any) int {
	idx := len(b.Resp.Output)
	b.Resp.Output = append(b.Resp.Output, item)
	b.texts = append(b.texts, &strings.Builder{})
	b.emit("response.output_item.added", &itemEvent{"response.output_item.added", b.seq, idx, item})
	return idx
}

func (b *Builder) done(idx int) {
	b.emit("response.output_item.done", &itemEvent{"response.output_item.done", b.seq, idx, b.Resp.Output[idx]})
}

// StartMessage adds an assistant message with one output_text part and
// returns its output index.
func (b *Builder) StartMessage() int {
	m := &MessageItem{Type: "message", ID: NewID("msg"), Status: "in_progress", Role: "assistant", Content: []OutputText{}}
	idx := b.add(m)
	b.emit("response.content_part.added", &partEvent{"response.content_part.added", b.seq, m.ID, idx, 0, &OutputText{Type: "output_text", Annotations: []any{}, Logprobs: []any{}}})
	return idx
}

func (b *Builder) TextDelta(idx int, delta string) {
	m := b.Resp.Output[idx].(*MessageItem)
	b.texts[idx].WriteString(delta)
	b.emit("response.output_text.delta", &textDeltaEvent{"response.output_text.delta", b.seq, m.ID, idx, 0, delta, []any{}})
}

// EndMessage finalizes a message with status "completed" or "incomplete".
func (b *Builder) EndMessage(idx int, status string) {
	m := b.Resp.Output[idx].(*MessageItem)
	text := b.texts[idx].String()
	part := OutputText{Type: "output_text", Text: text, Annotations: []any{}, Logprobs: []any{}}
	b.emit("response.output_text.done", &textDoneEvent{"response.output_text.done", b.seq, m.ID, idx, 0, text, []any{}})
	b.emit("response.content_part.done", &partEvent{"response.content_part.done", b.seq, m.ID, idx, 0, &part})
	m.Content = []OutputText{part}
	m.Status = status
	b.done(idx)
}

// StartFunctionCall adds a function_call item and returns its output index.
func (b *Builder) StartFunctionCall(callID, name string) int {
	return b.add(&FunctionCallItem{Type: "function_call", ID: NewID("fc"), CallID: callID, Name: name, Status: "in_progress"})
}

func (b *Builder) ArgumentsDelta(idx int, delta string) {
	fc := b.Resp.Output[idx].(*FunctionCallItem)
	b.texts[idx].WriteString(delta)
	b.emit("response.function_call_arguments.delta", &argsDeltaEvent{"response.function_call_arguments.delta", b.seq, fc.ID, idx, delta})
}

func (b *Builder) EndFunctionCall(idx int, status string) {
	fc := b.Resp.Output[idx].(*FunctionCallItem)
	fc.Arguments = b.texts[idx].String()
	if fc.Arguments == "" {
		fc.Arguments = "{}"
	}
	b.emit("response.function_call_arguments.done", &argsDoneEvent{"response.function_call_arguments.done", b.seq, fc.ID, idx, fc.Arguments})
	fc.Status = status
	b.done(idx)
}

// StartReasoning adds a reasoning item with one reasoning_text part.
func (b *Builder) StartReasoning() int {
	r := &ReasoningItem{Type: "reasoning", ID: NewID("rs"), Summary: []ReasoningText{}}
	idx := b.add(r)
	b.emit("response.content_part.added", &partEvent{"response.content_part.added", b.seq, r.ID, idx, 0, &ReasoningText{Type: "reasoning_text"}})
	return idx
}

func (b *Builder) ReasoningDelta(idx int, delta string) {
	r := b.Resp.Output[idx].(*ReasoningItem)
	b.texts[idx].WriteString(delta)
	b.emit("response.reasoning.delta", &textDeltaEvent{"response.reasoning.delta", b.seq, r.ID, idx, 0, delta, nil})
}

// EndReasoning finalizes a reasoning item. encrypted carries opaque provider
// state needed to replay the reasoning in later turns (may be nil).
func (b *Builder) EndReasoning(idx int, encrypted *string) {
	r := b.Resp.Output[idx].(*ReasoningItem)
	text := b.texts[idx].String()
	part := ReasoningText{Type: "reasoning_text", Text: text}
	b.emit("response.reasoning.done", &textDoneEvent{"response.reasoning.done", b.seq, r.ID, idx, 0, text, nil})
	b.emit("response.content_part.done", &partEvent{"response.content_part.done", b.seq, r.ID, idx, 0, &part})
	r.Content = []ReasoningText{part}
	r.EncryptedContent = encrypted
	b.done(idx)
}

// AddReasoning adds a finished reasoning item with no visible text, such as
// redacted thinking.
func (b *Builder) AddReasoning(encrypted *string) {
	r := &ReasoningItem{Type: "reasoning", ID: NewID("rs"), Summary: []ReasoningText{}, EncryptedContent: encrypted}
	idx := b.add(r)
	b.done(idx)
}

// Complete sets usage and the terminal status and emits the terminal event.
// incompleteReason is empty for a completed response.
func (b *Builder) Complete(usage *Usage, incompleteReason string) *Response {
	now := time.Now().Unix()
	b.Resp.Usage = usage
	typ := "response.completed"
	if incompleteReason != "" {
		b.Resp.Status = "incomplete"
		b.Resp.IncompleteDetails = &IncompleteDetails{Reason: incompleteReason}
		typ = "response.incomplete"
	} else {
		b.Resp.Status = "completed"
		b.Resp.CompletedAt = &now
	}
	b.emit(typ, &responseEvent{typ, b.seq, b.Resp})
	return b.Resp
}

// Fail emits an error event followed by response.failed.
func (b *Builder) Fail(e *APIError) *Response {
	b.emit("error", &errorEvent{"error", b.seq, ErrorPayload{Type: e.Type, Code: e.Code, Message: e.Message, Param: e.Param}})
	b.Resp.Status = "failed"
	b.Resp.Error = &ResponseError{Code: e.CodeString(), Message: e.Message}
	b.emit("response.failed", &responseEvent{"response.failed", b.seq, b.Resp})
	return b.Resp
}

// Seq returns the next sequence number.
func (b *Builder) Seq() int { return b.seq }
