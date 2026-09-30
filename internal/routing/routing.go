// Package routing picks the upstream deployment for each request and moves
// on to other deployments and fallback models when one fails before sending
// anything.
package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/provider"
)

// Deployment is one way to reach a provider: an API key and a base URL.
// Its health is kept per process.
type Deployment struct {
	Name     string
	Provider string
	Weight   int
	Adapter  provider.Provider

	mu        sync.Mutex
	restUntil time.Time // cooling down after a 429, or kept away after an auth failure
	failures  int       // unavailable errors in a row
	openUntil time.Time // the breaker is open until then
	open      bool
	probing   bool // one request is testing a half-open breaker
}

// Pool is a provider's deployments.
type Pool struct {
	Name        string
	Deployments []*Deployment
	Sticky      bool
	CacheTTL    time.Duration
}

// Target is one model a request may be served by: the requested one or a
// fallback.
type Target struct {
	// Name is the model as configured: an alias or "<provider>/<model>".
	Name          string
	Pool          *Pool
	UpstreamModel string
}

// QualifiedName is "<provider>/<upstream model>".
func (t Target) QualifiedName() string { return t.Pool.Name + "/" + t.UpstreamModel }

type model struct {
	target    Target
	fallbacks []string
}

// Router maps client-facing model names to targets and runs attempts.
type Router struct {
	pools  map[string]*Pool
	models map[string]model
	opts   config.Routing
	log    *slog.Logger
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	aff    affinity
}

func New(cfg *config.Config, log *slog.Logger) (*Router, error) {
	client := provider.NewHTTPClient(cfg.ResponseHeaderTimeout)
	r := &Router{pools: map[string]*Pool{}, models: map[string]model{}, opts: cfg.Routing, log: log, now: time.Now, sleep: sleepCtx, aff: affinity{m: map[string]sticky{}}}
	for _, p := range cfg.Providers {
		pool := &Pool{Name: p.Name, Sticky: p.IsSticky(), CacheTTL: p.CacheTTL}
		for _, d := range p.Deployments {
			var a provider.Provider
			switch p.Type {
			case "openai":
				a = provider.NewOpenAI(p.Name, d.BaseURL, d.APIKey, d.Headers, client)
			case "chat_completions":
				a = provider.NewChat(p.Name, d.BaseURL, d.APIKey, d.Headers, client)
			default:
				return nil, fmt.Errorf("unknown provider type %q", p.Type)
			}
			pool.Deployments = append(pool.Deployments, &Deployment{Name: d.Name, Provider: p.Name, Weight: d.Weight, Adapter: a})
		}
		r.pools[p.Name] = pool
	}
	for _, m := range cfg.Models {
		r.models[m.Name] = model{target: Target{Name: m.Name, Pool: r.pools[m.Provider], UpstreamModel: m.UpstreamModel}, fallbacks: m.Fallbacks}
	}
	return r, nil
}

func (r *Router) target(name string) (Target, bool) {
	if m, ok := r.models[name]; ok {
		return m.target, true
	}
	if prov, upstream, ok := strings.Cut(name, "/"); ok && upstream != "" {
		if p, ok := r.pools[prov]; ok {
			return Target{Name: name, Pool: p, UpstreamModel: upstream}, true
		}
	}
	return Target{}, false
}

// Resolve finds a configured model alias, or accepts "<provider>/<model>".
func (r *Router) Resolve(name string) (Target, bool) { return r.target(name) }

// Plan returns the requested model's target followed by its fallbacks.
func (r *Router) Plan(name string) ([]Target, bool) {
	t, ok := r.target(name)
	if !ok {
		return nil, false
	}
	plan := []Target{t}
	for _, f := range r.models[name].fallbacks {
		if ft, ok := r.target(f); ok {
			plan = append(plan, ft)
		}
	}
	return plan, true
}

// Models lists configured aliases.
func (r *Router) Models() []string {
	out := make([]string, 0, len(r.models))
	for name := range r.models {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Attempt is where one try went.
type Attempt struct {
	Target     Target
	Deployment *Deployment
}

// Outcome is what Run did.
type Outcome struct {
	Attempt
	Attempts int
}

// Request says where a request may go.
type Request struct {
	// Plan is the requested model's target, then its fallbacks.
	Plan []Target
	// Pinned keeps the request on the first target, for requests whose
	// state lives with the upstream account (previous_response_id,
	// encrypted reasoning).
	Pinned bool
	// Prefer is the deployment the session last used, tried first while it
	// is healthy.
	Prefer *Deployment
}

// Run calls try for each attempt until one does not fail with a retryable
// *provider.Failure, or MaxAttempts is reached. The order is: the preferred
// deployment, or a weighted pick among the first target's healthy
// deployments; its other healthy deployments; then each fallback target the
// same way.
func (r *Router) Run(ctx context.Context, req Request, try func(Attempt) (*provider.Result, error)) (*provider.Result, Outcome, error) {
	plan := req.Plan
	if req.Pinned {
		plan = plan[:1]
	}
	type key struct {
		target int
		d      *Deployment
	}
	var (
		tried    = map[key]bool{}
		ti       int
		out      Outcome
		res      *provider.Result
		err      error
		lastKind = provider.Rejected
	)
	for out.Attempts < r.opts.MaxAttempts {
		var (
			a   Attempt
			idx int
		)
		now := r.now()
		if p := req.Prefer; out.Attempts == 0 && p != nil && p.Provider == plan[0].Pool.Name && p.usable(now) {
			a = Attempt{plan[0], p}
		}
		for ; a.Deployment == nil && ti < len(plan); ti++ {
			var avail []*Deployment
			for _, d := range plan[ti].Pool.Deployments {
				if !tried[key{ti, d}] && d.usable(now) {
					avail = append(avail, d)
				}
			}
			if d := pick(avail); d != nil {
				a, idx = Attempt{plan[ti], d}, ti
				break
			}
		}
		forced := false
		switch {
		case a.Deployment != nil:
		case out.Attempts == 0:
			// Every deployment is resting. Try one anyway rather than refuse:
			// the upstream decides.
			a, idx, forced = Attempt{plan[0], pick(plan[0].Pool.Deployments)}, 0, true
		case lastKind == provider.Unavailable && out.Deployment.usable(now):
			// Nothing else left: try the same deployment again.
			a, idx = out.Attempt, indexOf(plan, out.Target)
			delete(tried, key{idx, a.Deployment})
		}
		if a.Deployment == nil {
			break
		}
		if !forced && !a.Deployment.claim(now) {
			tried[key{idx, a.Deployment}] = true // another request is probing it
			continue
		}
		if out.Attempts > 0 {
			if lastKind == provider.Unavailable {
				if r.sleep(ctx, r.backoff(out.Attempts)) != nil {
					a.Deployment.release()
					break
				}
			}
			if a.Target.Name != out.Target.Name {
				metrics.Fallbacks.WithLabelValues(out.Target.Name, a.Target.Name).Inc()
			}
		}
		tried[key{idx, a.Deployment}] = true
		out.Attempt = a
		out.Attempts++
		res, err = try(a)

		var f *provider.Failure
		switch {
		case err == nil:
			a.Deployment.succeeded()
			observe(a, "ok")
			return res, out, nil
		case ctx.Err() != nil || !errors.As(err, &f):
			// The client left, or the upstream failed after streaming started.
			a.Deployment.release()
			observe(a, "error")
			return res, out, err
		}
		lastKind = f.Kind
		switch f.Kind {
		case provider.RateLimited:
			a.Deployment.rest(r.now(), cmp(f.RetryAfter, r.opts.Cooldown))
			observe(a, "rate_limited")
		case provider.AuthFailed:
			r.log.Error("upstream rejected a deployment's credentials; check its api_key",
				"provider", a.Deployment.Provider, "deployment", a.Deployment.Name, "error", f.Message)
			a.Deployment.rest(r.now(), r.opts.Breaker.OpenFor)
			observe(a, "auth_failed")
		case provider.Unavailable:
			if a.Deployment.failed(r.now(), r.opts.Breaker.Failures, r.opts.Breaker.OpenFor) {
				r.log.Warn("deployment breaker open", "provider", a.Deployment.Provider, "deployment", a.Deployment.Name, "for", r.opts.Breaker.OpenFor)
			}
			observe(a, "unavailable")
		default:
			// The request itself was refused; another deployment would refuse it too.
			a.Deployment.release()
			observe(a, "rejected")
			return res, out, err
		}
	}
	return res, out, err
}

func observe(a Attempt, outcome string) {
	metrics.UpstreamAttempts.WithLabelValues(a.Deployment.Provider, a.Deployment.Name, outcome).Inc()
}

func indexOf(plan []Target, t Target) int {
	for i := range plan {
		if plan[i] == t {
			return i
		}
	}
	return 0
}

// cmp returns a if set, else b.
func cmp(a, b time.Duration) time.Duration {
	if a > 0 {
		return a
	}
	return b
}

// backoff is exponential with full jitter, capped at Backoff.Max.
func (r *Router) backoff(attempt int) time.Duration {
	d := r.opts.Backoff.Base << (attempt - 1)
	if d > r.opts.Backoff.Max || d <= 0 {
		d = r.opts.Backoff.Max
	}
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pick chooses a deployment at random, in proportion to weight.
func pick(ds []*Deployment) *Deployment {
	total := 0
	for _, d := range ds {
		total += d.Weight
	}
	if total <= 0 {
		return nil
	}
	n := rand.N(total)
	for _, d := range ds {
		if n < d.Weight {
			return d
		}
		n -= d.Weight
	}
	return nil
}

// usable reports whether the deployment should get traffic now: not
// resting, and its breaker closed, or half-open with no probe in flight.
func (d *Deployment) usable(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Before(d.restUntil) {
		return false
	}
	return !d.open || (!now.Before(d.openUntil) && !d.probing)
}

// claim marks a half-open deployment as being probed, so only one request
// tests it. It fails when another request got there first.
func (d *Deployment) claim(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.open {
		return true
	}
	if d.probing || now.Before(d.openUntil) {
		return false
	}
	d.probing = true
	return true
}

// release ends a probe that proved nothing either way.
func (d *Deployment) release() {
	d.mu.Lock()
	d.probing = false
	d.mu.Unlock()
}

func (d *Deployment) succeeded() {
	d.mu.Lock()
	wasOpen := d.open
	d.failures, d.open, d.probing = 0, false, false
	d.mu.Unlock()
	if wasOpen {
		metrics.BreakerOpen.WithLabelValues(d.Provider, d.Name).Set(0)
	}
}

// failed counts an unavailable error and reports whether it opened the
// breaker. A failed probe reopens it.
func (d *Deployment) failed(now time.Time, threshold int, openFor time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failures++
	if d.probing || (!d.open && d.failures >= threshold) {
		opened := !d.open
		d.open, d.probing, d.openUntil = true, false, now.Add(openFor)
		metrics.BreakerOpen.WithLabelValues(d.Provider, d.Name).Set(1)
		return opened
	}
	return false
}

// rest keeps traffic away for a while without touching the breaker.
func (d *Deployment) rest(now time.Time, dur time.Duration) {
	d.mu.Lock()
	if until := now.Add(dur); until.After(d.restUntil) {
		d.restUntil = until
	}
	d.probing = false
	d.mu.Unlock()
}
