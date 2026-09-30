package limits

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// counters returns the in-process counter and, with TEST_REDIS_URL, a
// Valkey one, so every behaviour is checked against both.
func counters(t *testing.T) map[string]func() Counter {
	cs := map[string]func() Counter{"memory": func() Counter { return NewMemory() }}
	if url := os.Getenv("TEST_REDIS_URL"); url != "" {
		cs["redis"] = func() Counter {
			r, err := NewRedis(url)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.c.FlushDB(context.Background()).Err(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			return r
		}
	}
	return cs
}

type fixture struct {
	*Engine
	st    *store.Memory
	clock time.Time
}

func newFixture(t *testing.T, c Counter, rules ...store.Limit) *fixture {
	t.Helper()
	st := store.NewMemory()
	for i := range rules {
		if rules[i].Enforcement == "" {
			rules[i].Enforcement = "hard"
		}
		if err := st.CreateLimit(context.Background(), &rules[i]); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{Engine: New(st, c, false, quiet), st: st, clock: time.Unix(1_700_000_000, 0)}
	f.now = func() time.Time { return f.clock }
	if err := f.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) check(t *testing.T, s Subject) *Decision {
	t.Helper()
	d, err := f.Check(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var (
	alice = Subject{TenantID: "t1", AppID: "a1", User: "alice@x.com"}
	bob   = Subject{TenantID: "t1", AppID: "a1", User: "bob@x.com"}
	other = Subject{TenantID: "t1", AppID: "a2", User: "alice@x.com"}
)

func TestRequestsPerMinute(t *testing.T) {
	for name, c := range counters(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, c(), store.Limit{TenantID: "t1", AppID: "a1", Kind: store.LimitRPM, Amount: 3})
			for i := range 3 {
				if d := f.check(t, alice); !d.Allowed {
					t.Fatalf("request %d refused", i)
				}
			}
			d := f.check(t, bob)
			if d.Allowed || d.Blocked == nil || d.Blocked.Scope() != "application" || d.RetryAfter <= 0 {
				t.Fatalf("4th request in the app should be refused: %+v", d)
			}
			if !f.check(t, other).Allowed {
				t.Error("another app is not limited")
			}
			// A refused request takes nothing; the window slides.
			f.clock = f.clock.Add(61 * time.Second)
			// Up to a minute later, 3 * (1 - elapsed share) still counts.
			if d := f.check(t, alice); d.Allowed && d.Applied[0].Used > 3 {
				t.Errorf("used %v", d.Applied[0].Used)
			}
			f.clock = f.clock.Add(2 * time.Minute)
			for i := range 3 {
				if !f.check(t, alice).Allowed {
					t.Fatalf("after two quiet minutes, request %d refused", i)
				}
			}
		})
	}
}

func TestEachUserRule(t *testing.T) {
	for name, c := range counters(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, c(), store.Limit{TenantID: "t1", User: "*", Kind: store.LimitRPM, Amount: 1})
			if !f.check(t, alice).Allowed || !f.check(t, bob).Allowed {
				t.Fatal("each user gets their own allowance")
			}
			if d := f.check(t, other); d.Allowed || d.Blocked.Scope() != "user" {
				t.Fatal("the rule is tenant-wide, so alice in another app shares her allowance")
			}
			if !f.check(t, Subject{TenantID: "t1", AppID: "a1"}).Allowed {
				t.Error("requests without a user are not counted by per-user rules")
			}
		})
	}
}

func TestNamedUserAndTenantRules(t *testing.T) {
	f := newFixture(t, NewMemory(),
		store.Limit{TenantID: "t1", User: "alice@x.com", Kind: store.LimitRPM, Amount: 1},
		store.Limit{TenantID: "t1", Kind: store.LimitRPM, Amount: 3},
	)
	if !f.check(t, alice).Allowed || f.check(t, alice).Allowed {
		t.Fatal("alice's own rule should allow one request")
	}
	if !f.check(t, bob).Allowed || !f.check(t, bob).Allowed {
		t.Fatal("bob is only under the tenant rule")
	}
	if d := f.check(t, bob); d.Allowed || d.Blocked.Scope() != "tenant" {
		t.Fatalf("tenant rule: %+v", d)
	}
	if !f.check(t, Subject{TenantID: "t2", AppID: "b", User: "alice@x.com"}).Allowed {
		t.Error("rules do not cross tenants")
	}
}

func TestTokensPerMinute(t *testing.T) {
	for name, c := range counters(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, c(), store.Limit{TenantID: "t1", AppID: "a1", Kind: store.LimitTPM, Amount: 1000})
			d := f.check(t, alice)
			if !d.Allowed || !d.ChargesTokens() {
				t.Fatal("first request allowed")
			}
			f.Charge(context.Background(), d, 999)
			d = f.check(t, alice)
			if !d.Allowed {
				t.Fatal("999 of 1000 used: one more request may start")
			}
			// It overshoots; the next is refused.
			f.Charge(context.Background(), d, 500)
			if d := f.check(t, alice); d.Allowed || d.RetryAfter < time.Second || d.RetryAfter > time.Minute {
				t.Fatalf("over the limit: %+v", d)
			}
		})
	}
}

func TestSoftRulesAndOtherTenantsAreIgnored(t *testing.T) {
	f := newFixture(t, NewMemory(), store.Limit{TenantID: "t1", Kind: store.LimitRPM, Amount: 1, Enforcement: "soft"})
	for range 3 {
		if d := f.check(t, alice); !d.Allowed || len(d.Applied) != 0 {
			t.Fatal("soft rules never block")
		}
	}
}

type broken struct{ *Memory }

func (*broken) Take(context.Context, []Window, time.Time) (int, []float64, error) {
	return -1, nil, errors.New("valkey down")
}

func TestFailOpenAndClosed(t *testing.T) {
	rule := store.Limit{TenantID: "t1", Kind: store.LimitRPM, Amount: 1}
	f := newFixture(t, &broken{NewMemory()}, rule)
	for range 3 {
		if d, err := f.Check(context.Background(), alice); err != nil || !d.Allowed {
			t.Fatal("fail open by default")
		}
	}
	f.failClosed = true
	if d, err := f.Check(context.Background(), alice); err == nil || d.Allowed {
		t.Fatal("fail_closed refuses")
	}
}

func TestHeaders(t *testing.T) {
	f := newFixture(t, NewMemory(),
		store.Limit{TenantID: "t1", Kind: store.LimitRPM, Amount: 10},
		store.Limit{TenantID: "t1", AppID: "a1", Kind: store.LimitRPM, Amount: 2},
		store.Limit{TenantID: "t1", Kind: store.LimitTPM, Amount: 5000},
	)
	h := http.Header{}
	f.check(t, alice).Headers(h)
	if h.Get("X-Ratelimit-Limit-Requests") != "2" || h.Get("X-Ratelimit-Remaining-Requests") != "1" || h.Get("X-Ratelimit-Limit-Tokens") != "5000" {
		t.Fatalf("headers %v", h)
	}
	f.check(t, alice)
	h = http.Header{}
	f.check(t, alice).Headers(h)
	if h.Get("Retry-After") == "" || h.Get("X-Ratelimit-Remaining-Requests") != "0" {
		t.Fatalf("refused headers %v", h)
	}
}

func TestRedisSharedAcrossEngines(t *testing.T) {
	mk, ok := counters(t)["redis"]
	if !ok {
		t.Skip("TEST_REDIS_URL not set")
	}
	c := mk()
	rule := store.Limit{TenantID: "t1", Kind: store.LimitRPM, Amount: 4}
	a := newFixture(t, c, rule)
	// A second replica with the same rule and counters.
	b := New(a.st, c, false, quiet)
	b.now = a.now
	if err := b.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	allowed := 0
	for i := range 10 {
		e := a.Engine
		if i%2 == 1 {
			e = b
		}
		if d, _ := e.Check(context.Background(), alice); d.Allowed {
			allowed++
		}
	}
	if allowed != 4 {
		t.Fatalf("two replicas allowed %d requests under a shared limit of 4", allowed)
	}
}
