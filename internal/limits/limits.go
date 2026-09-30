// Package limits enforces rate limits per tenant, application and end user.
// Rules come from the store and are cached in each proxy, so the store is
// never on the request path; counters live in Valkey when it is configured,
// otherwise in process.
package limits

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// Subject is who a request counts against.
type Subject struct {
	TenantID string
	AppID    string
	User     string // email, "" when unknown
}

type Engine struct {
	store      store.Store
	counter    Counter
	failClosed bool
	log        *slog.Logger
	now        func() time.Time

	mu       sync.RWMutex
	byTenant map[string][]store.Limit

	lastErrLog atomic.Int64 // unix seconds
}

func New(st store.Store, counter Counter, failClosed bool, log *slog.Logger) *Engine {
	return &Engine{store: st, counter: counter, failClosed: failClosed, log: log, now: time.Now, byTenant: map[string][]store.Limit{}}
}

// Reload reads the rules from the store.
func (e *Engine) Reload(ctx context.Context) error {
	ls, err := e.store.ListLimits(ctx)
	if err != nil {
		return err
	}
	by := map[string][]store.Limit{}
	for _, l := range ls {
		by[l.TenantID] = append(by[l.TenantID], l)
	}
	e.mu.Lock()
	e.byTenant = by
	e.mu.Unlock()
	return nil
}

// Run reloads the rules every interval until ctx is done.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Reload(ctx); err != nil {
				e.log.Warn("reloading limits failed; keeping the previous rules", "err", err)
			}
		}
	}
}

// Rules returns every rule, grouped by nothing, for the admin API.
func (e *Engine) Rules() []store.Limit {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []store.Limit
	for _, ls := range e.byTenant {
		out = append(out, ls...)
	}
	return out
}

// applies reports whether a rule counts this subject.
func applies(l *store.Limit, s Subject) bool {
	if l.AppID != "" && l.AppID != s.AppID {
		return false
	}
	switch l.User {
	case "":
		return true
	case "*":
		return s.User != ""
	default:
		return l.User == s.User
	}
}

// Key is the counter a rule keeps for a subject: one per rule, or one per
// user for "*" rules.
func Key(l *store.Limit, user string) string {
	if l.User == "*" {
		return "rl/" + l.ID + "/" + user
	}
	return "rl/" + l.ID
}

// Applied is a rule that counted a request.
type Applied struct {
	Rule store.Limit
	Key  string
	Used float64 // before this request
}

// Decision is the outcome of Check.
type Decision struct {
	Allowed bool
	// Blocked is the rule that refused the request.
	Blocked *store.Limit
	// RetryAfter is when to try again, for refused requests.
	RetryAfter time.Duration
	Applied    []Applied
}

// Check counts a request against every rule that applies to the subject,
// taking one request from each request limit if all have room. When the
// counter store fails, the request is allowed (or refused with fail_closed)
// and the failure is counted in a metric.
func (e *Engine) Check(ctx context.Context, s Subject) (*Decision, error) {
	now := e.now()
	d := &Decision{Allowed: true}
	e.mu.RLock()
	rules := e.byTenant[s.TenantID]
	e.mu.RUnlock()
	var ws []Window
	for i := range rules {
		l := &rules[i]
		if !applies(l, s) || l.Enforcement == "soft" {
			continue
		}
		w := Window{Key: Key(l, s.User), Limit: l.Amount}
		switch l.Kind {
		case store.LimitRPM:
			w.Take = 1
		case store.LimitTPM:
		default:
			continue // budgets are checked elsewhere
		}
		ws = append(ws, w)
		d.Applied = append(d.Applied, Applied{Rule: *l, Key: w.Key})
	}
	if len(ws) == 0 {
		return d, nil
	}
	blocked, used, err := e.counter.Take(ctx, ws, now)
	if err != nil {
		e.counterFailed(err)
		d.Applied = nil
		if e.failClosed {
			d.Allowed = false
			return d, err
		}
		return d, nil
	}
	for i := range used {
		d.Applied[i].Used = used[i]
	}
	if blocked >= 0 {
		d.Allowed = false
		d.Blocked = &d.Applied[blocked].Rule
		a := d.Applied[blocked]
		d.RetryAfter = drain(a.Used, a.Used+1-a.Rule.Amount)
		metrics.LimitRejections.WithLabelValues(d.Blocked.Kind, d.Blocked.Scope()).Inc()
	}
	return d, nil
}

// ChargesTokens reports whether any token limit counted the request.
func (d *Decision) ChargesTokens() bool {
	for _, a := range d.Applied {
		if a.Rule.Kind == store.LimitTPM {
			return true
		}
	}
	return false
}

// Charge adds a finished request's tokens to its token limits.
func (e *Engine) Charge(ctx context.Context, d *Decision, tokens int) {
	if d == nil || tokens <= 0 {
		return
	}
	var keys []string
	for _, a := range d.Applied {
		if a.Rule.Kind == store.LimitTPM {
			keys = append(keys, a.Key)
		}
	}
	if len(keys) == 0 {
		return
	}
	if err := e.counter.Add(ctx, keys, int64(tokens), e.now()); err != nil {
		e.counterFailed(err)
	}
}

// counterFailed counts a counter failure and logs at most once a minute.
func (e *Engine) counterFailed(err error) {
	metrics.LimiterErrors.Inc()
	now := time.Now().Unix()
	if last := e.lastErrLog.Load(); now-last >= 60 && e.lastErrLog.CompareAndSwap(last, now) {
		mode := "allowing requests (fail open)"
		if e.failClosed {
			mode = "refusing requests (fail_closed)"
		}
		e.log.Error("rate limit counters unavailable; "+mode, "err", err)
	}
}

// Used reads a rule's current use, for a user when the rule is per user.
func (e *Engine) Used(ctx context.Context, l *store.Limit, user string) (float64, error) {
	return e.counter.Used(ctx, Key(l, user), e.now())
}

// Headers sets OpenAI-style x-ratelimit headers for the tightest request
// and token limits, so client SDKs back off on their own.
func (d *Decision) Headers(h http.Header) {
	var req, tok *Applied
	for i := range d.Applied {
		a := &d.Applied[i]
		switch a.Rule.Kind {
		case store.LimitRPM:
			if req == nil || a.Rule.Amount-a.Used < req.Rule.Amount-req.Used {
				req = a
			}
		case store.LimitTPM:
			if tok == nil || a.Rule.Amount-a.Used < tok.Rule.Amount-tok.Used {
				tok = a
			}
		}
	}
	set := func(suffix string, a *Applied, taken float64) {
		if a == nil {
			return
		}
		reset := drain(a.Used+taken, a.Used+taken).String()
		remaining := max(int64(a.Rule.Amount-a.Used-taken), 0)
		h.Set("X-Ratelimit-Limit-"+suffix, strconv.FormatInt(int64(a.Rule.Amount), 10))
		h.Set("X-Ratelimit-Remaining-"+suffix, strconv.FormatInt(remaining, 10))
		h.Set("X-Ratelimit-Reset-"+suffix, reset)
	}
	taken := 0.0
	if d.Allowed {
		taken = 1
	}
	set("Requests", req, taken)
	set("Tokens", tok, 0)
	if !d.Allowed && d.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int((d.RetryAfter+time.Second-1)/time.Second)))
	}
}

// Message explains a refusal to the caller.
func (d *Decision) Message() string {
	if d.Blocked == nil {
		return "Rate limits are unavailable, so the request was refused."
	}
	unit := map[string]string{store.LimitRPM: "requests", store.LimitTPM: "tokens"}[d.Blocked.Kind]
	return fmt.Sprintf("Rate limit reached: %s %s per minute for this %s. Try again in %d seconds.",
		strconv.FormatFloat(d.Blocked.Amount, 'f', -1, 64), unit, d.Blocked.Scope(), int(d.RetryAfter.Seconds()+0.999))
}

// drain estimates how long a sliding minute holding used units takes to shed
// excess of them, assuming they arrived evenly: at least a second, at most
// the window.
func drain(used, excess float64) time.Duration {
	if excess <= 0 || used <= 0 {
		return 0
	}
	d := time.Duration(float64(window) * min(excess/used, 1))
	return max(d.Round(time.Second), time.Second)
}
