package routing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
	"github.com/boramuyar/ultimate-proxy/internal/provider"
)

// script makes each deployment answer from a list of outcomes, in order,
// then succeed.
type script map[string][]error

func failure(kind provider.FailureKind, retryAfter time.Duration) error {
	return &provider.Failure{Kind: kind, RetryAfter: retryAfter, APIError: openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "x", "x", "")}
}

var (
	unavailable = failure(provider.Unavailable, 0)
	limited     = failure(provider.RateLimited, 0)
	authFailed  = failure(provider.AuthFailed, 0)
	rejected    = failure(provider.Rejected, 0)
	midstream   = openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_stream_error", "x", "")
)

type testRouter struct {
	*Router
	clock  time.Time
	slept  []time.Duration
	script script
	calls  []string // deployment names, in call order
}

func newTestRouter(t *testing.T, yaml string) *testRouter {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	tr := &testRouter{Router: r, clock: time.Unix(1_000_000, 0), script: script{}}
	r.now = func() time.Time { return tr.clock }
	r.sleep = func(_ context.Context, d time.Duration) error { tr.slept = append(tr.slept, d); return nil }
	return tr
}

func (tr *testRouter) run(t *testing.T, model string, pinned bool) (Outcome, error) {
	t.Helper()
	plan, ok := tr.Plan(model)
	if !ok {
		t.Fatalf("no plan for %s", model)
	}
	_, out, err := tr.Run(context.Background(), plan, pinned, func(a Attempt) (*provider.Result, error) {
		name := a.Deployment.Provider + "/" + a.Deployment.Name
		tr.calls = append(tr.calls, name)
		if q := tr.script[name]; len(q) > 0 {
			tr.script[name] = q[1:]
			return nil, q[0]
		}
		return &provider.Result{Status: "completed"}, nil
	})
	return out, err
}

const twoKeys = `
admin_token: x
providers:
  - name: openai
    type: openai
    api_key: k
    deployments:
      - {name: a}
      - {name: b}
  - name: local
    type: chat_completions
    base_url: http://localhost:1/v1
models:
  - {name: smart, provider: openai, upstream_model: gpt, fallbacks: [local/qwen]}
routing:
  max_attempts: 3
  cooldown: 10s
  breaker: {failures: 2, open_for: 30s}
`

// only keeps traffic on one deployment by making the other rest.
func (tr *testRouter) only(provider, name string) {
	for _, d := range tr.pools[provider].Deployments {
		if d.Name != name {
			d.rest(tr.clock, time.Hour)
		}
	}
}

func TestRetriesAnotherDeploymentAfterServerError(t *testing.T) {
	tr := newTestRouter(t, twoKeys)
	tr.script["openai/a"] = []error{unavailable}
	tr.script["openai/b"] = []error{unavailable}
	out, err := tr.run(t, "smart", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Attempts != 3 || out.Target.Name != "local/qwen" || len(tr.slept) != 2 {
		t.Fatalf("attempts %d, target %s, slept %v, calls %v", out.Attempts, out.Target.Name, tr.slept, tr.calls)
	}
	for _, d := range tr.slept {
		if d <= 0 || d > 2*time.Second {
			t.Errorf("backoff %v out of range", d)
		}
	}
}

func TestRateLimitCoolsDownWithoutBackoff(t *testing.T) {
	tr := newTestRouter(t, twoKeys)
	tr.script["openai/a"] = []error{failure(provider.RateLimited, 3*time.Second)}
	tr.script["openai/b"] = []error{limited}
	out, err := tr.run(t, "smart", false)
	if err != nil || out.Target.Name != "local/qwen" || len(tr.slept) != 0 {
		t.Fatalf("err %v, target %s, slept %v", err, out.Target.Name, tr.slept)
	}
	a, b := tr.pools["openai"].Deployments[0], tr.pools["openai"].Deployments[1]
	if a.usable(tr.clock.Add(2*time.Second)) || !a.usable(tr.clock.Add(3*time.Second)) {
		t.Error("a should rest for its Retry-After")
	}
	if b.usable(tr.clock.Add(9*time.Second)) || !b.usable(tr.clock.Add(10*time.Second)) {
		t.Error("b should rest for the default cooldown")
	}
	// While both rest, traffic goes straight to the fallback.
	tr.calls = nil
	if out, _ := tr.run(t, "smart", false); out.Target.Name != "local/qwen" || len(tr.calls) != 1 {
		t.Errorf("calls %v", tr.calls)
	}
}

func TestAuthFailureMovesOnAndSidelinesTheDeployment(t *testing.T) {
	tr := newTestRouter(t, twoKeys)
	tr.only("openai", "a")
	tr.script["openai/a"] = []error{authFailed}
	out, err := tr.run(t, "smart", false)
	if err != nil || out.Target.Name != "local/qwen" || len(tr.slept) != 0 {
		t.Fatalf("err %v, calls %v, slept %v", err, tr.calls, tr.slept)
	}
	if tr.pools["openai"].Deployments[0].usable(tr.clock.Add(29 * time.Second)) {
		t.Error("a deployment with a bad key should stay out for open_for")
	}
}

func TestRejectedAndMidstreamErrorsAreNotRetried(t *testing.T) {
	for name, e := range map[string]error{"rejected": rejected, "midstream": midstream} {
		tr := newTestRouter(t, twoKeys)
		tr.script["openai/a"] = []error{e}
		tr.script["openai/b"] = []error{e}
		out, err := tr.run(t, "smart", false)
		if !errors.Is(err, e) || out.Attempts != 1 {
			t.Errorf("%s: err %v after %d attempts", name, err, out.Attempts)
		}
	}
}

func TestPinnedStaysOnTheProvider(t *testing.T) {
	tr := newTestRouter(t, twoKeys)
	tr.script["openai/a"] = []error{unavailable, unavailable}
	tr.script["openai/b"] = []error{unavailable, unavailable}
	out, err := tr.run(t, "smart", true)
	if err == nil || out.Target.Name != "smart" {
		t.Fatalf("err %v, target %s, calls %v", err, out.Target.Name, tr.calls)
	}
	for _, c := range tr.calls {
		if c == "local/local" {
			t.Fatal("a pinned request went to the fallback")
		}
	}
}

func TestSingleDeploymentRetriesItself(t *testing.T) {
	tr := newTestRouter(t, `
admin_token: x
providers: [{name: openai, type: openai}]
models: [{name: m, provider: openai, upstream_model: gpt}]
`)
	tr.script["openai/openai"] = []error{unavailable, unavailable}
	out, err := tr.run(t, "m", false)
	if err != nil || out.Attempts != 3 {
		t.Fatalf("err %v after %d attempts", err, out.Attempts)
	}
}

func TestBreakerOpensAndHalfOpens(t *testing.T) {
	tr := newTestRouter(t, twoKeys)
	tr.only("openai", "a")
	a := tr.pools["openai"].Deployments[0]
	// Two requests, each failing on a, then served by the fallback.
	for range 2 {
		tr.script["openai/a"] = []error{unavailable}
		if _, err := tr.run(t, "smart", false); err != nil {
			t.Fatal(err)
		}
	}
	if a.usable(tr.clock) {
		t.Fatal("breaker should be open after 2 failures in a row")
	}
	tr.calls = nil
	tr.run(t, "smart", false)
	if len(tr.calls) != 1 || tr.calls[0] != "local/local" {
		t.Fatalf("open breaker should send traffic to the fallback: %v", tr.calls)
	}

	// After open_for, one probe goes through; a failed probe reopens it.
	tr.clock = tr.clock.Add(30 * time.Second)
	if !a.usable(tr.clock) || !a.claim(tr.clock) || a.usable(tr.clock) {
		t.Fatal("a half-open breaker should admit exactly one probe")
	}
	a.release()
	tr.script["openai/a"] = []error{unavailable}
	tr.calls = nil
	tr.run(t, "smart", false)
	if tr.calls[0] != "openai/a" || a.usable(tr.clock) {
		t.Fatalf("failed probe should reopen the breaker: %v", tr.calls)
	}

	// A successful probe closes it.
	tr.clock = tr.clock.Add(30 * time.Second)
	tr.calls = nil
	if out, _ := tr.run(t, "smart", false); out.Deployment != a || !a.usable(tr.clock) {
		t.Fatalf("probe should close the breaker: %v", tr.calls)
	}
}

func TestEverythingRestingStillTriesOnce(t *testing.T) {
	tr := newTestRouter(t, twoKeys)
	for _, p := range tr.pools {
		for _, d := range p.Deployments {
			d.rest(tr.clock, time.Hour)
		}
	}
	out, err := tr.run(t, "smart", false)
	if err != nil || out.Attempts != 1 || out.Target.Name != "smart" {
		t.Fatalf("err %v, out %+v", err, out)
	}
}

func TestWeightedPick(t *testing.T) {
	tr := newTestRouter(t, `
admin_token: x
providers:
  - name: openai
    type: openai
    deployments: [{name: a, weight: 3}, {name: b}]
models: [{name: m, provider: openai, upstream_model: gpt}]
`)
	counts := map[string]int{}
	for range 4000 {
		out, _ := tr.run(t, "m", false)
		counts[out.Deployment.Name]++
	}
	if counts["a"] < 2700 || counts["a"] > 3300 {
		t.Errorf("weight 3:1 gave %v", counts)
	}
}

func TestFallbackConfigValidation(t *testing.T) {
	for _, bad := range []string{
		`models: [{name: m, provider: openai, upstream_model: gpt, fallbacks: [nope]}]`,
		`models: [{name: m, provider: openai, upstream_model: gpt, fallbacks: [m]}]`,
		`models: [{name: m, provider: openai, upstream_model: gpt, fallbacks: [other/x]}]`,
	} {
		if _, err := config.Parse([]byte("admin_token: x\nproviders: [{name: openai, type: openai}]\n" + bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := config.Parse([]byte("admin_token: x\nproviders: [{name: openai, type: openai, deployments: [{name: a}, {name: a}]}]\n")); err == nil {
		t.Error("accepted duplicate deployment names")
	}
}
