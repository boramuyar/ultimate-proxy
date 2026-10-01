// Package limits enforces rate limits and budgets per tenant, application
// and end user. Rules come from the store and are cached in each proxy, so
// the store is never on the request path; counters live in Valkey when it is
// configured, otherwise in process.
package limits

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/metrics"
	"github.com/omni-proxy/omni-proxy/internal/store"
)

// Subject is who a request counts against.
type Subject struct {
	TenantID string
	AppID    string
	User     string // email, "" when unknown
}

// Alert is called when a charge takes a budget past 80% or 100% of its
// amount, on the one replica whose charge crossed it.
type Alert func(l store.Limit, user string, used float64, periodEnd time.Time)

type Engine struct {
	store      store.Store
	counter    Counter
	failClosed bool
	log        *slog.Logger
	now        func() time.Time
	alert      Alert

	mu       sync.RWMutex
	byTenant map[string][]store.Limit

	lastErrLog atomic.Int64 // unix seconds
}

func New(st store.Store, counter Counter, failClosed bool, log *slog.Logger) *Engine {
	return &Engine{store: st, counter: counter, failClosed: failClosed, log: log, now: time.Now, byTenant: map[string][]store.Limit{}}
}

// OnBudget sets the function told about budget thresholds.
func (e *Engine) OnBudget(a Alert) { e.alert = a }

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

// Run rereads the rules every interval, and rebuilds budget counters from
// the usage log every hour, until ctx is done.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	rebuild := time.NewTicker(time.Hour)
	defer rebuild.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Reload(ctx); err != nil {
				e.log.Warn("reloading limits failed; keeping the previous rules", "err", err)
			}
		case <-rebuild.C:
			if err := e.Rebuild(ctx); err != nil {
				e.log.Warn("rebuilding budgets from usage failed", "err", err)
			}
		}
	}
}

func (e *Engine) rules() []store.Limit {
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

// PeriodStart is when a budget's current period began, in UTC.
func PeriodStart(period string, now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case "week":
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // back to Monday
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return day
}

// PeriodEnd is when a budget's current period ends.
func PeriodEnd(period string, now time.Time) time.Time {
	start := PeriodStart(period, now)
	switch period {
	case "week":
		return start.AddDate(0, 0, 7)
	case "month":
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

// Key is the counter a rule keeps for a subject: one per rule, or one per
// user for "*" rules; budgets get a new one each period.
func Key(l *store.Limit, user string, now time.Time) string {
	k := "rl/" + l.ID
	if l.User == "*" {
		k += "/" + user
	}
	if l.IsBudget() {
		k += "/" + PeriodStart(l.Period, now).Format("2006-01-02")
	}
	return k
}

// unit scales a rule's amounts to integer counters: USD budgets count
// micro-dollars.
func unit(l *store.Limit) float64 {
	if l.Kind == store.LimitBudgetUSD {
		return 1e6
	}
	return 1
}

// Applied is a rule that counted a request.
type Applied struct {
	Rule store.Limit
	Key  string
	Used float64 // before this request, in the rule's unit (USD for USD budgets)
}

// Decision is the outcome of Check.
type Decision struct {
	Allowed bool
	// Blocked is the rule that refused the request.
	Blocked *store.Limit
	// RetryAfter is when to try again, for refused requests.
	RetryAfter time.Duration
	Applied    []Applied
	user       string
}

// Check counts a request against every rule that applies to the subject,
// taking one request from each request limit if all have room. Soft budgets
// are counted but never refuse. When the counter store fails, the request is
// allowed (or refused with fail_closed) and the failure is counted in a
// metric.
func (e *Engine) Check(ctx context.Context, s Subject) (*Decision, error) {
	now := e.now()
	d := &Decision{Allowed: true, user: s.User}
	e.mu.RLock()
	rules := e.byTenant[s.TenantID]
	e.mu.RUnlock()
	var ws []Window
	for i := range rules {
		l := &rules[i]
		if !applies(l, s) {
			continue
		}
		w := Window{Key: Key(l, s.User, now), Limit: l.Amount * unit(l), Fixed: l.IsBudget()}
		switch {
		case l.Kind == store.LimitRPM:
			w.Take = 1
		case l.Kind == store.LimitTPM, l.IsBudget():
		default:
			continue
		}
		if l.Enforcement == "soft" {
			if !l.IsBudget() {
				continue
			}
			w.Limit = math.MaxFloat64
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
		d.Applied[i].Used = used[i] / unit(&d.Applied[i].Rule)
	}
	if blocked >= 0 {
		a := d.Applied[blocked]
		d.Allowed = false
		d.Blocked = &a.Rule
		if a.Rule.IsBudget() {
			d.RetryAfter = PeriodEnd(a.Rule.Period, now).Sub(now)
		} else {
			d.RetryAfter = drain(a.Used, a.Used+1-a.Rule.Amount)
		}
		metrics.LimitRejections.WithLabelValues(a.Rule.Kind, a.Rule.Scope()).Inc()
	}
	return d, nil
}

// Charges reports whether anything is charged after the response: token
// limits or budgets.
func (d *Decision) Charges() bool {
	for _, a := range d.Applied {
		if a.Rule.Kind != store.LimitRPM {
			return true
		}
	}
	return false
}

// Charge adds a finished request's tokens and cost to its token limits and
// budgets, and raises an alert for any budget it takes past 80% or 100%.
func (e *Engine) Charge(ctx context.Context, d *Decision, tokens int, costUSD float64) {
	if d == nil || (tokens <= 0 && costUSD <= 0) {
		return
	}
	now := e.now()
	var (
		cs    []Charge
		rules []store.Limit
	)
	for _, a := range d.Applied {
		c := Charge{Key: a.Key, Fixed: a.Rule.IsBudget()}
		switch a.Rule.Kind {
		case store.LimitTPM, store.LimitBudgetTokens:
			c.N = int64(tokens)
		case store.LimitBudgetUSD:
			c.N = int64(math.Round(costUSD * 1e6))
		}
		if c.N <= 0 {
			continue
		}
		if c.Fixed {
			// Kept a day past the period, for the status page.
			c.TTL = PeriodEnd(a.Rule.Period, now).Sub(now) + 24*time.Hour
		}
		cs = append(cs, c)
		rules = append(rules, a.Rule)
	}
	if len(cs) == 0 {
		return
	}
	totals, err := e.counter.Add(ctx, cs, now)
	if err != nil {
		e.counterFailed(err)
		return
	}
	if e.alert == nil {
		return
	}
	for i, l := range rules {
		if !l.IsBudget() {
			continue
		}
		after := totals[i] / unit(&l)
		before := (totals[i] - float64(cs[i].N)) / unit(&l)
		for _, share := range []float64{0.8, 1} {
			if at := l.Amount * share; before < at && after >= at {
				e.alert(l, alertUser(&l, d.user), after, PeriodEnd(l.Period, now))
			}
		}
	}
}

// alertUser is who a budget alert is about: the user for per-user rules,
// the named user for a user's rule, else no one.
func alertUser(l *store.Limit, user string) string {
	switch l.User {
	case "":
		return ""
	case "*":
		return user
	}
	return l.User
}

// Rebuild sets every budget's counters from the usage log, so a Valkey
// restart loses nothing: counters are a cache, the usage log is the record.
func (e *Engine) Rebuild(ctx context.Context) error {
	for _, l := range e.rules() {
		if err := e.RebuildRule(ctx, &l); err != nil {
			return err
		}
	}
	return nil
}

// RebuildRule sets one budget's counters for the current period.
func (e *Engine) RebuildRule(ctx context.Context, l *store.Limit) error {
	if !l.IsBudget() {
		return nil
	}
	now := e.now()
	q := store.UsageQuery{
		From: PeriodStart(l.Period, now), To: now.Add(time.Second),
		Filters: map[string]string{"tenant": l.TenantID},
	}
	if l.AppID != "" {
		q.Filters["application"] = l.AppID
	}
	switch l.User {
	case "":
	case "*":
		q.GroupBy = []string{"email"}
	default:
		q.Filters["email"] = l.User
	}
	rows, err := e.store.QueryUsage(ctx, q)
	if err != nil {
		return err
	}
	ttl := PeriodEnd(l.Period, now).Sub(now) + 24*time.Hour
	for _, r := range rows {
		user := r.Group["email"]
		if l.User == "*" && user == "" {
			continue
		}
		v := float64(r.InputTokens + r.OutputTokens)
		if l.Kind == store.LimitBudgetUSD {
			v = r.CostUSD * 1e6
		}
		if err := e.counter.Set(ctx, Key(l, user, now), int64(math.Round(v)), ttl); err != nil {
			return err
		}
	}
	return nil
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

// Used reads a rule's current use, for a user when the rule is per user, in
// the rule's unit.
func (e *Engine) Used(ctx context.Context, l *store.Limit, user string) (float64, error) {
	now := e.now()
	v, err := e.counter.Used(ctx, Key(l, user, now), l.IsBudget(), now)
	return v / unit(l), err
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

// Code is the error code for a refusal.
func (d *Decision) Code() string {
	if d.Blocked != nil && d.Blocked.IsBudget() {
		return "budget_exceeded"
	}
	return "rate_limit_exceeded"
}

// Message explains a refusal to the caller.
func (d *Decision) Message() string {
	l := d.Blocked
	if l == nil {
		return "Rate limits are unavailable, so the request was refused."
	}
	if l.IsBudget() {
		return fmt.Sprintf("Budget reached: %s per %s for this %s. It resets in %s, at the start of the next %s (UTC).",
			FormatAmount(l), l.Period, l.Scope(), d.RetryAfter.Round(time.Minute), l.Period)
	}
	return fmt.Sprintf("Rate limit reached: %s per minute for this %s. Try again in %d seconds.",
		FormatAmount(l), l.Scope(), int(d.RetryAfter.Seconds()+0.999))
}

// FormatAmount writes a rule's amount with its unit, like "$5.00" or
// "20000 tokens".
func FormatAmount(l *store.Limit) string {
	n := strconv.FormatFloat(l.Amount, 'f', -1, 64)
	switch l.Kind {
	case store.LimitBudgetUSD:
		return fmt.Sprintf("$%.2f", l.Amount)
	case store.LimitRPM:
		return n + " requests"
	}
	return n + " tokens"
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
