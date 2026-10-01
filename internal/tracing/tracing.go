// Package tracing sends a span per model call over OTLP/HTTP, to an
// OpenTelemetry Collector or any backend that takes OTLP (Grafana Tempo,
// Jaeger, Langfuse, Honeycomb, Datadog). It is off unless an endpoint is
// configured.
//
// Spans carry metadata only: model, tokens, cost, cache status, tenant and
// application. Prompts and completions are never sent.
package tracing

import (
	"context"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// Tracer starts and ends model call spans.
type Tracer struct {
	tracer      trace.Tracer
	provider    *sdktrace.TracerProvider // nil when off
	includeUser bool
}

var propagator = propagation.TraceContext{}

// New sets up OTLP/HTTP export, or a tracer that does nothing when
// cfg.Endpoint is empty.
func New(cfg config.Tracing) (*Tracer, error) {
	if cfg.Endpoint == "" {
		return &Tracer{tracer: noop.NewTracerProvider().Tracer("")}, nil
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	// Headers left empty, such as from an unset environment variable, are
	// not sent.
	headers := map[string]string{}
	for k, v := range cfg.Headers {
		if v != "" {
			headers[k] = v
		}
	}
	// A bare collector address gets the standard path.
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	exp := newExporter(u.String(), headers)
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		attrs = append(attrs, semconv.ServiceVersion(bi.Main.Version))
	}
	res := resource.NewWithAttributes(semconv.SchemaURL, attrs...)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// Follow the caller's sampling decision when it sent one.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*cfg.SampleRatio))),
	)
	return &Tracer{tracer: tp.Tracer("github.com/boramuyar/ultimate-proxy"), provider: tp, includeUser: cfg.IncludeUser}, nil
}

// Shutdown sends the spans still queued.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t.provider == nil {
		return nil
	}
	return t.provider.Shutdown(ctx)
}

// Start begins a model call's span, as a child of the caller's trace when
// the request carries a traceparent header.
func (t *Tracer) Start(r *http.Request, start time.Time, provider, model string) trace.Span {
	ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	_, span := t.tracer.Start(ctx, "chat "+model,
		trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(start),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.provider.name", provider),
			attribute.String("gen_ai.request.model", model),
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
		))
	return span
}

// End records what the call came to and ends its span.
func (t *Tracer) End(span trace.Span, ev *store.UsageEvent, tenant, app string) {
	if !span.IsRecording() {
		span.End()
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("ultimate_proxy.request_id", ev.RequestID),
		attribute.Int("gen_ai.usage.input_tokens", ev.InputTokens),
		attribute.Int("gen_ai.usage.output_tokens", ev.OutputTokens),
		attribute.Int("gen_ai.usage.cache_read.input_tokens", ev.CachedInputTokens),
		attribute.Int("gen_ai.usage.cache_creation.input_tokens", ev.CacheWriteTokens),
		attribute.Int("http.response.status_code", ev.HTTPStatus),
		attribute.Bool("ultimate_proxy.stream", ev.Stream),
		attribute.String("ultimate_proxy.status", ev.Status),
		attribute.String("ultimate_proxy.tenant", tenant),
		attribute.String("ultimate_proxy.application", app),
		attribute.Float64("ultimate_proxy.cost_usd", ev.CostUSD),
	}
	if ev.CacheStatus != "" {
		attrs = append(attrs,
			attribute.String("ultimate_proxy.cache_status", ev.CacheStatus),
			attribute.Float64("ultimate_proxy.missed_cost_usd", ev.MissedCostUSD))
	}
	if ev.ReasoningTokens > 0 {
		attrs = append(attrs, attribute.Int("gen_ai.usage.reasoning.output_tokens", ev.ReasoningTokens))
	}
	if ev.TTFTMS != nil {
		attrs = append(attrs, attribute.Int("ultimate_proxy.ttft_ms", *ev.TTFTMS))
	}
	if t.includeUser && ev.UserEmail != "" {
		attrs = append(attrs, attribute.String("user.email", ev.UserEmail))
	}
	for k, v := range ev.Tags {
		attrs = append(attrs, attribute.String("ultimate_proxy.tag."+k, v))
	}
	span.SetAttributes(attrs...)
	if ev.Status == "failed" || ev.Status == "rejected" || ev.HTTPStatus >= 400 {
		span.SetAttributes(attribute.String("error.type", errorType(ev)))
		span.SetStatus(codes.Error, ev.ErrorCode)
	}
	span.End()
}

func errorType(ev *store.UsageEvent) string {
	if ev.ErrorCode != "" {
		return ev.ErrorCode
	}
	return strings.ToLower(http.StatusText(ev.HTTPStatus))
}
