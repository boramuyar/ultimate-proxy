package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/identity"
	"github.com/boramuyar/ultimate-proxy/internal/insights"
	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/pricing"
	"github.com/boramuyar/ultimate-proxy/internal/provider"
	"github.com/boramuyar/ultimate-proxy/internal/sse"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()

	principal, apiErr := s.auth.Authenticate(ctx, r)
	if apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err != nil {
		openresponses.WriteError(w, openresponses.NewError(http.StatusRequestEntityTooLarge, openresponses.ErrInvalidRequest, "request_too_large", err.Error(), ""))
		return
	}
	var req openresponses.Request
	if err := json.Unmarshal(body, &req); err != nil {
		openresponses.WriteError(w, openresponses.InvalidRequest("invalid_json", "Request body is not valid JSON: "+err.Error(), ""))
		return
	}
	if req.Model == "" {
		openresponses.WriteError(w, openresponses.InvalidRequest("missing_required_parameter", "model is required.", "model"))
		return
	}
	if req.Background != nil && *req.Background {
		openresponses.WriteError(w, openresponses.InvalidRequest("unsupported_parameter", "background responses are not supported yet.", "background"))
		return
	}
	route, ok := s.router.Resolve(req.Model)
	if !ok {
		openresponses.WriteError(w, openresponses.InvalidRequest("model_not_found", "The requested model '"+req.Model+"' does not exist.", "model"))
		return
	}

	// Fingerprint the prompt prefix and note what the cache should hold for
	// it. Caches are per upstream model, so that is the scope, per application.
	var (
		fp         *insights.Fingerprint
		exp        insights.Expectation
		cacheScope string
	)
	if s.insights != nil {
		fp = insights.Compute(body)
		cacheScope = principal.AppID + "\x00" + route.Provider.Name() + "/" + route.UpstreamModel
		exp = s.insights.Tracker.Before(cacheScope, fp, start)
	}

	email, source := identity.ResolveUser(principal, r, &req)
	call := &provider.Call{
		Req:           &req,
		Body:          body,
		Model:         req.Model,
		UpstreamModel: route.UpstreamModel,
		ResponseID:    openresponses.NewID("resp"),
		CreatedAt:     start.Unix(),
	}
	w.Header().Set("X-Proxy-Request-Id", call.ResponseID)

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

	res, err := route.Provider.Create(ctx, call, sink)

	ev := store.UsageEvent{
		TS:            start.UTC(),
		RequestID:     call.ResponseID,
		TenantID:      principal.TenantID,
		AppID:         principal.AppID,
		KeyID:         principal.KeyID,
		UserEmail:     email,
		UserSource:    source,
		Model:         req.Model,
		Provider:      route.Provider.Name(),
		UpstreamModel: route.UpstreamModel,
		Stream:        req.Stream,
		HTTPStatus:    http.StatusOK,
	}
	if req.PromptCacheKey != nil {
		ev.PromptCacheKey = *req.PromptCacheKey
	}
	if res != nil {
		ev.Status = res.Status
		ev.ErrorCode = res.ErrorCode
		ev.InputTokens = res.Usage.InputTokens
		ev.CachedInputTokens = res.Usage.CachedInputTokens
		ev.CacheWriteTokens = res.Usage.CacheWriteTokens
		ev.OutputTokens = res.Usage.OutputTokens
		ev.ReasoningTokens = res.Usage.ReasoningTokens
		ev.UsageReported = res.Usage.Reported
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

	cost, priced := s.prices.Cost(start, ev.Model, ev.Provider, ev.UpstreamModel, pricing.Usage{
		InputTokens: ev.InputTokens, CachedInputTokens: ev.CachedInputTokens,
		CacheWriteTokens: ev.CacheWriteTokens, OutputTokens: ev.OutputTokens,
	})
	ev.CostUSD = cost
	var missedCost float64
	if fp != nil && ev.UsageReported && ev.Status != "failed" {
		ev.CacheStatus, ev.ExpectedCachedTokens = s.insights.Tracker.Classify(fp, exp, ev.InputTokens, ev.CachedInputTokens)
		s.insights.Tracker.After(cacheScope, fp, time.Now())
		if ev.CacheStatus == insights.CacheUnexpectedMiss && priced {
			missedCost = float64(ev.ExpectedCachedTokens) * s.prices.SavingsPerCachedToken(start, ev.Model, ev.Provider, ev.UpstreamModel)
		}
	}

	elapsed := time.Since(start)
	ev.LatencyMS = int(elapsed.Milliseconds())
	if ts != nil && !ts.first.IsZero() {
		ms := int(ts.first.Sub(start).Milliseconds())
		ev.TTFTMS = &ms
		metrics.TTFT.WithLabelValues(ev.Provider, ev.Model).Observe(ts.first.Sub(start).Seconds())
	}
	s.meter.Record(ev)
	s.observe(&ev, principal, elapsed)
	if s.insights != nil {
		s.insights.Record(insights.Observation{
			TS: start, TenantID: principal.TenantID, TenantName: principal.TenantName,
			AppID: principal.AppID, AppName: principal.AppName, Model: ev.Model,
			Status: ev.Status, ErrorCode: ev.ErrorCode, CacheStatus: ev.CacheStatus,
			ExpectedCachedTokens: ev.ExpectedCachedTokens, MissedCostUSD: missedCost,
		})
	}
}

func (s *Server) observe(ev *store.UsageEvent, p *store.Principal, elapsed time.Duration) {
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

// handleCompact serves POST /v1/responses/compact, which is not built yet.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	if _, apiErr := s.auth.Authenticate(r.Context(), r); apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return
	}
	openresponses.WriteError(w, openresponses.NewError(http.StatusNotImplemented, openresponses.ErrServer, "not_implemented", "/v1/responses/compact is not supported yet.", ""))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, apiErr := s.auth.Authenticate(r.Context(), r); apiErr != nil {
		openresponses.WriteError(w, apiErr)
		return
	}
	type model struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	data := []model{}
	for _, m := range s.router.Models() {
		data = append(data, model{ID: m, Object: "model"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
