package routing

import (
	"testing"
	"time"
)

func TestPreferredDeploymentComesFirst(t *testing.T) {
	tr := newTestRouter(t, twoKeys)
	b := tr.pools["openai"].Deployments[1]
	for range 20 {
		if out, _ := tr.runPrefer(t, "smart", false, b); out.Deployment != b {
			t.Fatalf("served by %s", out.Deployment.Name)
		}
	}
	// A resting preferred deployment is skipped, and the rest of the order holds.
	b.rest(tr.clock, time.Minute)
	if out, _ := tr.runPrefer(t, "smart", false, b); out.Deployment.Name != "a" {
		t.Fatalf("served by %s", out.Deployment.Name)
	}
	// A preferred deployment of another provider is ignored.
	local := tr.pools["local"].Deployments[0]
	if out, _ := tr.runPrefer(t, "smart", false, local); out.Target.Name != "smart" {
		t.Fatalf("served by %s", out.Target.Name)
	}
}

func TestStickExpiresAndRespectsSettings(t *testing.T) {
	tr := newTestRouter(t, twoKeys+`
insights: {cache_ttl: 1m}
`)
	a := tr.pools["openai"].Deployments[0]
	tr.Stick("s", a, false)
	if tr.Sticky("s") != a {
		t.Fatal("not remembered")
	}
	tr.clock = tr.clock.Add(time.Minute)
	if tr.Sticky("s") != nil {
		t.Fatal("should expire after the cache TTL")
	}
	// A single-deployment provider has nothing to stick to, unless asked.
	local := tr.pools["local"].Deployments[0]
	tr.Stick("x", local, false)
	if tr.Sticky("x") != nil {
		t.Error("stuck to a single deployment")
	}
	tr.Stick("x", local, true)
	if tr.Sticky("x") != local {
		t.Error("always should stick")
	}

	off := newTestRouter(t, `
admin_token: x
providers:
  - {name: openai, type: openai, sticky: false, deployments: [{name: a}, {name: b}]}
models: [{name: m, provider: openai, upstream_model: gpt}]
`)
	off.Stick("s", off.pools["openai"].Deployments[0], false)
	if off.Sticky("s") != nil {
		t.Error("sticky: false should not stick")
	}
}

func TestProviderCacheTTL(t *testing.T) {
	tr := newTestRouter(t, `
admin_token: x
providers:
  - {name: vllm, type: chat_completions, cache_ttl: 1h, deployments: [{name: a}, {name: b}]}
models: [{name: m, provider: vllm, upstream_model: q}]
`)
	a := tr.pools["vllm"].Deployments[0]
	tr.Stick("s", a, false)
	tr.clock = tr.clock.Add(59 * time.Minute)
	if tr.Sticky("s") != a {
		t.Error("provider cache_ttl should override the default")
	}
}
