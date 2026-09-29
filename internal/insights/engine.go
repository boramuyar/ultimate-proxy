package insights

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// Insight kinds.
const (
	KindPrefixUnstable = "cache_prefix_unstable"
	KindUnexpectedMiss = "cache_unexpected_miss"
	KindErrorRate      = "error_rate"
	KindTruncation     = "truncation"
)

// Observation is one finished request, as the engine sees it.
type Observation struct {
	TS                   time.Time
	TenantID, TenantName string
	AppID, AppName       string
	Model                string
	Status               string // completed, incomplete, failed
	ErrorCode            string
	CacheStatus          string
	ExpectedCachedTokens int
	// MissedCostUSD is what an unexpected miss cost over a hit.
	MissedCostUSD float64
}

type scope struct{ tenant, app, model string }

var unstableStatuses = []string{CacheInstructionsDyn, CacheInstructionsChg, CacheToolsReordered, CacheToolsChanged, CacheHistoryRewritten}

type bucket struct {
	minute       int64
	requests     int
	failed       int
	incomplete   int
	eligible     int // requests whose cache status says something about the prefix
	hits         int
	unexpected   int
	unstable     [5]int // by unstableStatuses index
	missedTokens int
	missedCost   float64
	errorCodes   map[string]int
}

type window struct {
	tenantName, appName string
	buckets             []bucket
}

func (w *window) at(minute int64) *bucket {
	b := &w.buckets[int(minute%int64(len(w.buckets)))]
	if b.minute != minute {
		codes := b.errorCodes
		clear(codes)
		*b = bucket{minute: minute, errorCodes: codes}
	}
	return b
}

// Engine keeps per-scope traffic counters over a sliding window, runs rules
// over them, and opens and resolves insights.
type Engine struct {
	cfg     config.Insights
	store   store.Store
	notify  Notifier
	log     *slog.Logger
	Tracker *Tracker

	mu     sync.Mutex
	scopes map[scope]*window
	open   map[string]*store.Insight // by kind|tenant|app|model
}

func NewEngine(cfg config.Insights, st store.Store, n Notifier, log *slog.Logger) *Engine {
	if n == nil {
		n = nopNotifier{}
	}
	return &Engine{
		cfg: cfg, store: st, notify: n, log: log,
		Tracker: NewTracker(cfg.CacheTTL, cfg.MinCacheableTokens),
		scopes:  map[scope]*window{},
		open:    map[string]*store.Insight{},
	}
}

// Load picks up insights left open by a previous run, so they can resolve.
func (e *Engine) Load(ctx context.Context) error {
	open, err := e.store.ListInsights(ctx, "open")
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range open {
		in := open[i]
		e.open[openKey(in.Kind, scope{in.TenantID, in.AppID, in.Model})] = &in
		metrics.InsightsOpen.WithLabelValues(in.Kind).Inc()
	}
	return nil
}

// Record adds a finished request to its scope's counters.
func (e *Engine) Record(o Observation) {
	minute := o.TS.Unix() / 60
	e.mu.Lock()
	defer e.mu.Unlock()
	sc := scope{o.TenantID, o.AppID, o.Model}
	w := e.scopes[sc]
	if w == nil {
		n := int(e.cfg.Window/time.Minute) + 1
		w = &window{buckets: make([]bucket, max(n, 2))}
		for i := range w.buckets {
			w.buckets[i].minute = -1
		}
		e.scopes[sc] = w
	}
	w.tenantName, w.appName = o.TenantName, o.AppName
	b := w.at(minute)
	b.requests++
	switch o.Status {
	case "failed":
		b.failed++
		if b.errorCodes == nil {
			b.errorCodes = map[string]int{}
		}
		b.errorCodes[o.ErrorCode]++
	case "incomplete":
		b.incomplete++
	}
	switch o.CacheStatus {
	case "", CacheTooShort, CacheUnknown:
		return
	case CacheHit:
		b.hits++
	case CacheUnexpectedMiss:
		b.unexpected++
		b.missedTokens += o.ExpectedCachedTokens
		b.missedCost += o.MissedCostUSD
	default:
		for i, s := range unstableStatuses {
			if s == o.CacheStatus {
				b.unstable[i]++
			}
		}
	}
	b.eligible++
}

// Run evaluates rules every EvaluateInterval until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.EvaluateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			e.Tracker.Prune(now)
			e.Evaluate(ctx, now)
		}
	}
}

type totals struct {
	bucket
	unstableTotal int
}

func (w *window) sum(now time.Time, span time.Duration) totals {
	var t totals
	t.errorCodes = map[string]int{}
	from := now.Add(-span).Unix() / 60
	to := now.Unix() / 60
	for i := range w.buckets {
		b := &w.buckets[i]
		if b.minute <= from || b.minute > to {
			continue
		}
		t.requests += b.requests
		t.failed += b.failed
		t.incomplete += b.incomplete
		t.eligible += b.eligible
		t.hits += b.hits
		t.unexpected += b.unexpected
		t.missedTokens += b.missedTokens
		t.missedCost += b.missedCost
		for j, n := range b.unstable {
			t.unstable[j] += n
			t.unstableTotal += n
		}
		for c, n := range b.errorCodes {
			t.errorCodes[c] += n
		}
	}
	return t
}

// finding is one rule's verdict for one scope.
type finding struct {
	kind      string
	rate      float64
	threshold float64
	enough    bool // enough traffic to judge
	title     string
	detail    string
	evidence  map[string]any
}

// Evaluate runs every rule over the window ending at now.
func (e *Engine) Evaluate(ctx context.Context, now time.Time) {
	type change struct {
		event string
		in    store.Insight
	}
	var changes []change

	e.mu.Lock()
	for sc, w := range e.scopes {
		t := w.sum(now, e.cfg.Window)
		if t.requests == 0 {
			delete(e.scopes, sc)
		}
		for _, f := range e.rules(sc, w, &t) {
			key := openKey(f.kind, sc)
			in := e.open[key]
			switch {
			case in == nil && f.enough && f.rate >= f.threshold:
				in = &store.Insight{
					ID: openresponses.NewID("ins"), Kind: f.kind, Status: "open",
					TenantID: sc.tenant, AppID: sc.app, Model: sc.model, FirstSeen: now.UTC(),
				}
				e.open[key] = in
				fill(in, &f, now)
				metrics.InsightsOpen.WithLabelValues(f.kind).Inc()
				changes = append(changes, change{"opened", *in})
			case in != nil && (t.requests == 0 || (f.enough && f.rate < f.threshold/2)):
				delete(e.open, key)
				// Keep the title and severity the problem had while open; the
				// evidence shows the rate it recovered to.
				in.Evidence = f.evidence
				in.LastSeen = now.UTC()
				in.Status = "resolved"
				at := now.UTC()
				in.ResolvedAt = &at
				metrics.InsightsOpen.WithLabelValues(f.kind).Dec()
				changes = append(changes, change{"resolved", *in})
			case in != nil && f.enough:
				fill(in, &f, now)
				changes = append(changes, change{"", *in})
			}
		}
	}
	// Open insights whose scope has gone quiet resolve too.
	for key, in := range e.open {
		if _, ok := e.scopes[scope{in.TenantID, in.AppID, in.Model}]; !ok {
			delete(e.open, key)
			in.Status = "resolved"
			at := now.UTC()
			in.ResolvedAt = &at
			in.LastSeen = at
			metrics.InsightsOpen.WithLabelValues(in.Kind).Dec()
			changes = append(changes, change{"resolved", *in})
		}
	}
	e.mu.Unlock()

	for _, c := range changes {
		if err := e.store.SaveInsight(ctx, &c.in); err != nil {
			e.log.Error("saving insight failed", "err", err, "kind", c.in.Kind)
		}
		if c.event != "" {
			e.log.Info("insight "+c.event, "kind", c.in.Kind, "tenant", c.in.TenantID, "application", c.in.AppID, "model", c.in.Model, "title", c.in.Title)
			e.notify.Notify(c.event, c.in)
		}
	}
}

func fill(in *store.Insight, f *finding, now time.Time) {
	in.Title, in.Detail, in.Evidence = f.title, f.detail, f.evidence
	in.Severity = "warning"
	if f.rate >= 2*f.threshold || f.rate >= 0.9 {
		in.Severity = "critical"
	}
	in.LastSeen = now.UTC()
}

func openKey(kind string, sc scope) string {
	return kind + "|" + sc.tenant + "|" + sc.app + "|" + sc.model
}

var unstableAdvice = map[string]string{
	CacheInstructionsDyn:  "The instructions differ between requests only in numbers or ids, such as a timestamp, a date or a request id. Move the changing value out of the instructions and into the last input message so the instructions stay byte-identical.",
	CacheInstructionsChg:  "The instructions change between requests. Put the part that never changes first and the per-request part after it, or move per-request content into the input.",
	CacheToolsReordered:   "The same tools are sent in a different order, or with their JSON keys in a different order. Serialize the tool list deterministically.",
	CacheToolsChanged:     "The tool list changes between requests. Send the same tools every time, or keep the tools that change out of the request.",
	CacheHistoryRewritten: "Earlier turns of the conversation change between requests, for example because history is summarized, trimmed from the front, or re-serialized. Append new turns instead of rewriting old ones.",
}

func (e *Engine) rules(sc scope, w *window, t *totals) []finding {
	name := w.appName
	if name == "" {
		name = sc.app
	}
	win := fmtDuration(e.cfg.Window)
	minReq := e.cfg.MinRequests
	var out []finding

	// Unstable prefix: the application keeps changing the start of its prompt.
	{
		f := finding{kind: KindPrefixUnstable, threshold: e.cfg.UnstablePrefixRate, enough: t.eligible >= minReq}
		if t.eligible > 0 {
			f.rate = float64(t.unstableTotal) / float64(t.eligible)
		}
		top, reasons := 0, map[string]int{}
		for i, n := range t.unstable {
			if n > 0 {
				reasons[unstableStatuses[i]] = n
			}
			if n > t.unstable[top] {
				top = i
			}
		}
		f.title = fmt.Sprintf("%s: %.0f%% of %s requests miss the prompt cache because the prompt prefix keeps changing", name, 100*f.rate, sc.model)
		f.detail = unstableAdvice[unstableStatuses[top]]
		f.evidence = map[string]any{
			"window": win, "requests": t.eligible, "unstable": t.unstableTotal, "rate": round(f.rate),
			"reasons": reasons, "top_reason": unstableStatuses[top],
		}
		out = append(out, f)
	}

	// Unexpected misses: the prefix was cached recently but the provider missed.
	{
		denom := t.hits + t.unexpected
		f := finding{kind: KindUnexpectedMiss, threshold: e.cfg.UnexpectedMissRate, enough: denom >= minReq}
		if denom > 0 {
			f.rate = float64(t.unexpected) / float64(denom)
		}
		f.title = fmt.Sprintf("%s: %.0f%% of %s requests that should hit the prompt cache miss it", name, 100*f.rate, sc.model)
		f.detail = fmt.Sprintf("These requests repeat a prefix sent within the last %s, yet the provider served none of it from cache. "+
			"Set prompt_cache_key to a value shared by requests with the same prefix so they are routed to the same cache, "+
			"and check that the traffic is not spread across accounts, regions or deployments.", fmtDuration(e.cfg.CacheTTL))
		f.evidence = map[string]any{
			"window": win, "requests": denom, "unexpected_misses": t.unexpected, "rate": round(f.rate),
			"missed_cached_tokens": t.missedTokens, "missed_savings_usd": round(t.missedCost),
		}
		out = append(out, f)
	}

	// Error rate.
	{
		f := finding{kind: KindErrorRate, threshold: e.cfg.ErrorRate, enough: t.requests >= minReq}
		if t.requests > 0 {
			f.rate = float64(t.failed) / float64(t.requests)
		}
		f.title = fmt.Sprintf("%s: %.0f%% of %s requests fail", name, 100*f.rate, sc.model)
		f.detail = "Most common errors: " + topCodes(t.errorCodes) + "."
		f.evidence = map[string]any{"window": win, "requests": t.requests, "failed": t.failed, "rate": round(f.rate), "error_codes": t.errorCodes}
		out = append(out, f)
	}

	// Truncation.
	{
		f := finding{kind: KindTruncation, threshold: e.cfg.TruncationRate, enough: t.requests >= minReq}
		if t.requests > 0 {
			f.rate = float64(t.incomplete) / float64(t.requests)
		}
		f.title = fmt.Sprintf("%s: %.0f%% of %s responses are cut off", name, 100*f.rate, sc.model)
		f.detail = "Responses end incomplete, usually at max_output_tokens. Raise max_output_tokens or ask the model for shorter answers."
		f.evidence = map[string]any{"window": win, "requests": t.requests, "incomplete": t.incomplete, "rate": round(f.rate)}
		out = append(out, f)
	}
	return out
}

func topCodes(codes map[string]int) string {
	type kv struct {
		code string
		n    int
	}
	var list []kv
	for c, n := range codes {
		if c == "" {
			c = "unknown"
		}
		list = append(list, kv{c, n})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].n > list[j].n || (list[i].n == list[j].n && list[i].code < list[j].code)
	})
	var parts []string
	for i, x := range list {
		if i == 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", x.code, x.n))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func round(f float64) float64 { return float64(int64(f*10000+0.5)) / 10000 }

// fmtDuration prints 15m rather than 15m0s.
func fmtDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
