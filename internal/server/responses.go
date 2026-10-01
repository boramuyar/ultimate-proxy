package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/identity"
	"github.com/boramuyar/ultimate-proxy/internal/insights"
	"github.com/boramuyar/ultimate-proxy/internal/limits"
	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/pricing"
	"github.com/boramuyar/ultimate-proxy/internal/provider"
	"github.com/boramuyar/ultimate-proxy/internal/sse"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// handleProvider serves /<provider>/v1/...: the provider's Responses and
// Chat Completions APIs and its model list.
func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request) {
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	p, ok := s.providers[name]
	if !ok || !strings.HasPrefix(rest, "v1/") {
		openresponses.WriteError(w, openresponses.NewError(http.StatusNotFound, openresponses.ErrInvalidRequest, "not_found",
			"No such endpoint. Requests go to /<provider>/v1/..., where <provider> is a provider in the proxy's config.", ""))
		return
	}
	switch op := r.Method + " " + strings.TrimPrefix(rest, "v1"); op {
	case "POST /responses":
		s.handleResponses(w, r, p)
	case "POST /chat/completions":
		s.handleChat(w, r, p)
	case "POST /responses/compact":
		s.handleCompact(w, r)
	case "GET /models":
		s.handleModels(w, r, p)
	default:
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		openresponses.WriteError(w, openresponses.NewError(http.StatusNotFound, openresponses.ErrInvalidRequest, "not_found",
			"The proxy does not serve "+r.Method+" /"+name+"/"+rest+".", ""))
	}
}

// exchange is one model call through the proxy, from the caller's request to
// its usage event.
type exchange struct {
	start time.Time
	id    *identity.Identity
	req   *openresponses.Envelope
	p     *provider.OpenAI
	dec   *limits.Decision
	ev    store.UsageEvent
}

// admit authenticates a model call, reads its body and checks the model
// allowlist and the limits. On refusal it answers the caller, logs a
// rejected request when limits refused it, and returns nil.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, p *provider.OpenAI) *exchange {
	x := &exchange{start: time.Now(), p: p}
	var apiErr *openresponses.APIError
	if x.id, apiErr = s.auth.Authenticate(r.Context(), r); apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err != nil {
		openresponses.WriteError(w, openresponses.NewError(http.StatusRequestEntityTooLarge, openresponses.ErrInvalidRequest, "request_too_large", err.Error(), ""))
		return nil
	}
	// One pass over the body; the prompt itself is never decoded.
	if x.req, apiErr = openresponses.ParseEnvelope(body); apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return nil
	}
	req, id := x.req, x.id
	if req.Model == "" {
		openresponses.WriteError(w, openresponses.InvalidRequest("missing_required_parameter", "model is required.", "model"))
		return nil
	}
	if req.Background {
		openresponses.WriteError(w, openresponses.InvalidRequest("unsupported_parameter", "background responses are not supported yet.", "background"))
		return nil
	}
	if !id.AllowsModel(req.Model, x.qualified()) {
		openresponses.WriteError(w, errModelNotAllowed(req.Model))
		return nil
	}
	email, source := identity.ResolveUser(id, r, req)
	x.ev = store.UsageEvent{
		TS: x.start.UTC(), RequestID: openresponses.NewID("resp"), TenantID: id.TenantID, AppID: id.AppID, KeyID: id.KeyID(),
		AuthMethod: id.Method, Subject: id.Subject, UserEmail: email, UserSource: source,
		Model: req.Model, Provider: p.Name(), UpstreamModel: req.Model, Stream: req.Stream, HTTPStatus: http.StatusOK,
	}
	if req.PromptCacheKey != nil {
		x.ev.PromptCacheKey = *req.PromptCacheKey
	}
	w.Header().Set("X-Proxy-Request-Id", x.ev.RequestID)

	dec, limitErr := s.limits.Check(r.Context(), limitSubject(id.TenantID, id.AppID, email))
	dec.Headers(w.Header())
	x.dec = dec
	if !dec.Allowed {
		ae := openresponses.NewError(http.StatusTooManyRequests, openresponses.ErrTooManyRequests, dec.Code(), dec.Message(), "")
		if limitErr != nil {
			ae = openresponses.NewError(http.StatusServiceUnavailable, openresponses.ErrServer, "limits_unavailable", dec.Message(), "")
		}
		openresponses.WriteError(w, ae)
		// Refused requests are logged too, with no tokens, so they show up
		// in usage.
		x.ev.Status, x.ev.ErrorCode, x.ev.HTTPStatus = "rejected", ae.CodeString(), ae.Status
		x.ev.LatencyMS = int(time.Since(x.start).Milliseconds())
		s.meter.Record(x.ev)
		s.observe(&x.ev, id, time.Since(x.start))
		return nil
	}
	return x
}

// qualified is "<provider>/<model>", as model allowlists may name it.
func (x *exchange) qualified() string { return x.p.Name() + "/" + x.req.Model }

func (x *exchange) setUsage(u provider.Usage) {
	x.ev.InputTokens = u.InputTokens
	x.ev.CachedInputTokens = u.CachedInputTokens
	x.ev.CacheWriteTokens = u.CacheWriteTokens
	x.ev.OutputTokens = u.OutputTokens
	x.ev.ReasoningTokens = u.ReasoningTokens
	x.ev.UsageReported = u.Reported
}

// finish prices the call, logs its usage event and charges its limits.
// firstToken is when the first output reached a streaming client, if known.
func (s *Server) finish(x *exchange, firstToken time.Time, cache *cacheCheck) {
	ev := &x.ev
	cost, priced := s.prices.Cost(x.start, ev.Model, ev.Provider, ev.UpstreamModel, pricing.Usage{
		InputTokens: ev.InputTokens, CachedInputTokens: ev.CachedInputTokens,
		CacheWriteTokens: ev.CacheWriteTokens, OutputTokens: ev.OutputTokens,
	})
	ev.CostUSD = cost
	var missedCost float64
	if cache != nil && ev.UsageReported && ev.Status != "failed" {
		ev.CacheStatus, ev.ExpectedCachedTokens = s.insights.Tracker.Classify(cache.fp, cache.exp, ev.InputTokens, ev.CachedInputTokens)
		s.insights.Tracker.After(cache.scope, cache.fp, time.Now())
		if ev.CacheStatus == insights.CacheUnexpectedMiss && priced {
			missedCost = float64(ev.ExpectedCachedTokens) * s.prices.SavingsPerCachedToken(x.start, ev.Model, ev.Provider, ev.UpstreamModel)
		}
	}

	elapsed := time.Since(x.start)
	ev.LatencyMS = int(elapsed.Milliseconds())
	if !firstToken.IsZero() {
		ms := int(firstToken.Sub(x.start).Milliseconds())
		ev.TTFTMS = &ms
		metrics.TTFT.WithLabelValues(ev.Provider, ev.Model).Observe(firstToken.Sub(x.start).Seconds())
	}
	s.meter.Record(*ev)
	s.observe(ev, x.id, elapsed)
	// Charge token limits and budgets off the request path: the response
	// only completes when the handler returns.
	if n := ev.InputTokens + ev.OutputTokens; (n > 0 || ev.CostUSD > 0) && x.dec.Charges() {
		dec, usd := x.dec, ev.CostUSD
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s.limits.Charge(cctx, dec, n, usd)
		}()
	}
	if s.insights != nil {
		id := x.id
		s.insights.Record(insights.Observation{
			TS: x.start, TenantID: id.TenantID, TenantName: id.TenantName,
			AppID: id.AppID, AppName: id.AppName, Model: ev.Model,
			Status: ev.Status, ErrorCode: ev.ErrorCode, CacheStatus: ev.CacheStatus,
			ExpectedCachedTokens: ev.ExpectedCachedTokens, MissedCostUSD: missedCost,
		})
	}
}

// cacheCheck is the prompt prefix fingerprint taken before a call, and what
// the cache was expected to hold for it.
type cacheCheck struct {
	fp    *insights.Fingerprint
	exp   insights.Expectation
	scope string
}

// handleResponses serves POST /<provider>/v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request, p *provider.OpenAI) {
	ctx := r.Context()
	x := s.admit(w, r, p)
	if x == nil {
		return
	}
	req := x.req

	// Fingerprint the prompt prefix and note what the cache should hold for
	// it. Caches are per upstream model, so that is the scope, per application.
	var cache *cacheCheck
	if s.insights != nil {
		cache = &cacheCheck{fp: insights.FromEnvelope(req), scope: x.id.AppID + "\x00" + x.qualified()}
		cache.exp = s.insights.Tracker.Before(cache.scope, cache.fp, x.start)
	}

	var (
		sw   *sse.Writer
		ts   *timingSink
		sink openresponses.Sink
	)
	if req.Stream {
		sw = sse.NewWriter(w)
		ts = &timingSink{inner: sw}
		sink = ts
	}
	call := &provider.Call{Env: req, Model: req.Model, ResponseID: x.ev.RequestID, CreatedAt: x.start.Unix()}
	res, err := p.Create(ctx, call, sink)

	ev := &x.ev
	if res != nil {
		ev.Status, ev.ErrorCode = res.Status, res.ErrorCode
		x.setUsage(res.Usage)
	}
	switch {
	case err != nil:
		ev.Status = "failed"
		var ae *openresponses.APIError
		if !errors.As(err, &ae) {
			ae = openresponses.ServerError("proxy_error", err.Error())
		}
		if ctx.Err() != nil {
			ae = openresponses.NewError(499, openresponses.ErrInvalidRequest, "client_disconnected", "The client closed the connection.", "")
		}
		ev.ErrorCode = ae.CodeString()
		ev.HTTPStatus = ae.Status
		if ctx.Err() == nil {
			if sw != nil && sw.Started() {
				_ = sw.Done() // the adapter already streamed the error events
			} else {
				openresponses.WriteError(w, ae)
			}
		}
	case sw != nil:
		_ = sw.Done()
	case res.Status == "failed":
		// A non-streaming client gets an error status rather than a 200 with a failed body.
		ae := openresponses.NewError(http.StatusInternalServerError, openresponses.ErrModel, res.ErrorCode, "The model failed to generate a response.", "")
		ev.HTTPStatus = ae.Status
		openresponses.WriteError(w, ae)
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(res.Response)
	}
	var first time.Time
	if ts != nil {
		first = ts.first
	}
	s.finish(x, first, cache)
}

func (s *Server) observe(ev *store.UsageEvent, p *identity.Identity, elapsed time.Duration) {
	metrics.Requests.WithLabelValues(p.TenantName, p.AppName, ev.Model, ev.Provider, ev.Status).Inc()
	metrics.Latency.WithLabelValues(ev.Provider, ev.Model).Observe(elapsed.Seconds())
	if ev.Status == "failed" {
		metrics.UpstreamErrors.WithLabelValues(ev.Provider, ev.ErrorCode).Inc()
	}
	addTokens := func(kind string, n int) {
		if n > 0 {
			metrics.Tokens.WithLabelValues(p.TenantName, p.AppName, ev.Model, kind).Add(float64(n))
		}
	}
	addTokens("input", ev.InputTokens)
	addTokens("cached_input", ev.CachedInputTokens)
	addTokens("cache_write", ev.CacheWriteTokens)
	addTokens("output", ev.OutputTokens)
	addTokens("reasoning", ev.ReasoningTokens)
	if ev.CacheStatus != "" {
		metrics.CacheRequests.WithLabelValues(p.TenantName, p.AppName, ev.Model, ev.CacheStatus).Inc()
	}
	if ev.CacheStatus == insights.CacheUnexpectedMiss && ev.ExpectedCachedTokens > 0 {
		metrics.CacheMissedTokens.WithLabelValues(p.TenantName, p.AppName, ev.Model).Add(float64(ev.ExpectedCachedTokens))
	}
	if ev.CostUSD > 0 {
		metrics.CostUSD.WithLabelValues(p.TenantName, p.AppName, ev.Model).Add(ev.CostUSD)
	}
}

// timingSink records when the first output delta reaches the client.
type timingSink struct {
	inner openresponses.Sink
	first time.Time
}

func (t *timingSink) mark(typ string) {
	if t.first.IsZero() && strings.HasSuffix(typ, ".delta") {
		t.first = time.Now()
	}
}

func (t *timingSink) Event(typ string, v any) error {
	t.mark(typ)
	return t.inner.Event(typ, v)
}

func (t *timingSink) RawEvent(typ string, data []byte) error {
	t.mark(typ)
	return t.inner.RawEvent(typ, data)
}

// handleCompact serves POST /<provider>/v1/responses/compact, which is not
// built yet.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	if _, apiErr := s.auth.Authenticate(r.Context(), r); apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return
	}
	openresponses.WriteError(w, openresponses.NewError(http.StatusNotImplemented, openresponses.ErrServer, "not_implemented", "/v1/responses/compact is not supported yet.", ""))
}

// handleModels serves GET /<provider>/v1/models: the provider's own list,
// passed through.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request, p *provider.OpenAI) {
	if _, apiErr := s.auth.Authenticate(r.Context(), r); apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return
	}
	resp, err := p.Get(r.Context(), "/models")
	if err != nil {
		var ae *openresponses.APIError
		if !errors.As(err, &ae) {
			ae = openresponses.ServerError("proxy_error", err.Error())
		}
		openresponses.WriteError(w, ae)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func errModelNotAllowed(model string) *openresponses.APIError {
	return openresponses.NewError(http.StatusForbidden, openresponses.ErrInvalidRequest, "model_not_allowed",
		"This credential may not use the model '"+model+"'.", "model")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
