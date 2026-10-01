package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// exporter sends spans as OTLP/HTTP JSON, which every OTLP receiver accepts.
// The SDK's own exporter speaks protobuf and brings gRPC with it, which
// would grow the binary by about two thirds for a feature that is off by
// default.
type exporter struct {
	url     string
	headers map[string]string
	client  *http.Client
}

func newExporter(url string, headers map[string]string) *exporter {
	return &exporter{url: url, headers: headers, client: &http.Client{Timeout: 10 * time.Second}}
}

func (e *exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(encode(spans))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("otlp export: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (e *exporter) Shutdown(context.Context) error { return nil }

// The OTLP JSON encoding: IDs in hex, 64-bit integers as strings, enums as
// numbers. https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
type (
	exportRequest struct {
		ResourceSpans []resourceSpans `json:"resourceSpans"`
	}
	resourceSpans struct {
		Resource   otlpResource `json:"resource"`
		ScopeSpans []scopeSpans `json:"scopeSpans"`
		SchemaURL  string       `json:"schemaUrl,omitempty"`
	}
	otlpResource struct {
		Attributes []keyValue `json:"attributes"`
	}
	scopeSpans struct {
		Scope scope  `json:"scope"`
		Spans []span `json:"spans"`
	}
	scope struct {
		Name    string `json:"name"`
		Version string `json:"version,omitempty"`
	}
	span struct {
		TraceID           string     `json:"traceId"`
		SpanID            string     `json:"spanId"`
		TraceState        string     `json:"traceState,omitempty"`
		ParentSpanID      string     `json:"parentSpanId,omitempty"`
		Flags             uint32     `json:"flags,omitempty"`
		Name              string     `json:"name"`
		Kind              int        `json:"kind"`
		StartTimeUnixNano string     `json:"startTimeUnixNano"`
		EndTimeUnixNano   string     `json:"endTimeUnixNano"`
		Attributes        []keyValue `json:"attributes,omitempty"`
		Status            *status    `json:"status,omitempty"`
	}
	status struct {
		Code    int    `json:"code"`
		Message string `json:"message,omitempty"`
	}
	keyValue struct {
		Key   string   `json:"key"`
		Value anyValue `json:"value"`
	}
	anyValue struct {
		String *string   `json:"stringValue,omitempty"`
		Bool   *bool     `json:"boolValue,omitempty"`
		Int    *string   `json:"intValue,omitempty"`
		Double *float64  `json:"doubleValue,omitempty"`
		Array  *arrayVal `json:"arrayValue,omitempty"`
	}
	arrayVal struct {
		Values []anyValue `json:"values"`
	}
)

// encode groups spans by resource and scope. The proxy has one of each, so
// in practice this is a single group.
func encode(spans []sdktrace.ReadOnlySpan) exportRequest {
	type groupKey struct {
		res   attribute.Distinct
		scope string
	}
	var req exportRequest
	index := map[groupKey]int{}
	for _, s := range spans {
		k := groupKey{s.Resource().Equivalent(), s.InstrumentationScope().Name}
		i, ok := index[k]
		if !ok {
			i = len(req.ResourceSpans)
			index[k] = i
			req.ResourceSpans = append(req.ResourceSpans, resourceSpans{
				Resource:   otlpResource{Attributes: attrs(s.Resource().Attributes())},
				SchemaURL:  s.Resource().SchemaURL(),
				ScopeSpans: []scopeSpans{{Scope: scope{Name: s.InstrumentationScope().Name, Version: s.InstrumentationScope().Version}}},
			})
		}
		g := &req.ResourceSpans[i].ScopeSpans[0]
		g.Spans = append(g.Spans, encodeSpan(s))
	}
	return req
}

func encodeSpan(s sdktrace.ReadOnlySpan) span {
	sc := s.SpanContext()
	out := span{
		TraceID:           sc.TraceID().String(),
		SpanID:            sc.SpanID().String(),
		TraceState:        sc.TraceState().String(),
		Flags:             uint32(sc.TraceFlags()),
		Name:              s.Name(),
		Kind:              int(s.SpanKind()),
		StartTimeUnixNano: strconv.FormatInt(s.StartTime().UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(s.EndTime().UnixNano(), 10),
		Attributes:        attrs(s.Attributes()),
	}
	if p := s.Parent(); p.SpanID().IsValid() {
		out.ParentSpanID = p.SpanID().String()
	}
	// OTLP status codes: 0 unset, 1 ok, 2 error. The Go SDK numbers them
	// 0 unset, 1 error, 2 ok.
	switch st := s.Status(); st.Code {
	case codes.Error:
		out.Status = &status{Code: 2, Message: st.Description}
	case codes.Ok:
		out.Status = &status{Code: 1}
	}
	return out
}

func attrs(kvs []attribute.KeyValue) []keyValue {
	out := make([]keyValue, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, keyValue{Key: string(kv.Key), Value: value(kv.Value)})
	}
	return out
}

func value(v attribute.Value) anyValue {
	switch v.Type() {
	case attribute.BOOL:
		b := v.AsBool()
		return anyValue{Bool: &b}
	case attribute.INT64:
		n := strconv.FormatInt(v.AsInt64(), 10)
		return anyValue{Int: &n}
	case attribute.FLOAT64:
		f := v.AsFloat64()
		return anyValue{Double: &f}
	case attribute.BOOLSLICE, attribute.INT64SLICE, attribute.FLOAT64SLICE, attribute.STRINGSLICE:
		var arr arrayVal
		switch v.Type() {
		case attribute.BOOLSLICE:
			for _, b := range v.AsBoolSlice() {
				arr.Values = append(arr.Values, value(attribute.BoolValue(b)))
			}
		case attribute.INT64SLICE:
			for _, n := range v.AsInt64Slice() {
				arr.Values = append(arr.Values, value(attribute.Int64Value(n)))
			}
		case attribute.FLOAT64SLICE:
			for _, f := range v.AsFloat64Slice() {
				arr.Values = append(arr.Values, value(attribute.Float64Value(f)))
			}
		default:
			for _, str := range v.AsStringSlice() {
				arr.Values = append(arr.Values, value(attribute.StringValue(str)))
			}
		}
		return anyValue{Array: &arr}
	default:
		str := v.String()
		return anyValue{String: &str}
	}
}
