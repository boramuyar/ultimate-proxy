package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/fakeupstream"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

const fastBackoff = `
routing:
  backoff: {base: 1ms, max: 2ms}
`

// lastEvent waits for the meter to record n events and returns the last.
func (h *harness) lastEvent(n int) store.UsageEvent {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := h.store.Events()
		if len(evs) >= n {
			return evs[n-1]
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d usage events, want %d", len(evs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFallsBackWhenTheUpstreamFails(t *testing.T) {
	h := newHarnessWith(t, fastBackoff)
	h.upstream.FailNext(1, http.StatusServiceUnavailable)
	resp := h.post(trustedKey, `{"model":"resilient","input":"hi"}`)
	body := decode(t, resp)
	if resp.StatusCode != 200 || body["status"] != "completed" || body["model"] != "resilient" {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	ev := h.lastEvent(1)
	if ev.Model != "resilient" || ev.Provider != "local" || ev.UpstreamModel != "llama-fake" || ev.Deployment != "local" || ev.Attempts != 2 {
		t.Errorf("event %+v", ev)
	}
}

func TestStreamRetriedBeforeFirstByte(t *testing.T) {
	h := newHarnessWith(t, fastBackoff)
	h.upstream.FailNext(1, http.StatusTooManyRequests)
	s := readStream(t, h.post(trustedKey, `{"model":"resilient","stream":true,"input":"hi"}`))
	created := 0
	for _, n := range s.names {
		if n == "response.created" {
			created++
		}
	}
	if created != 1 || s.names[len(s.names)-1] != "response.completed" {
		t.Fatalf("events %v", s.names)
	}
	if ev := h.lastEvent(1); ev.Attempts != 2 || ev.Provider != "local" {
		t.Errorf("event %+v", ev)
	}
}

func TestNoRetryAfterStreamStartedOrOnBadRequest(t *testing.T) {
	h := newHarnessWith(t, fastBackoff)
	s := readStream(t, h.post(trustedKey, `{"model":"resilient","stream":true,"input":"`+fakeupstream.TriggerFailMidstream+`"}`))
	if s.names[len(s.names)-1] != "response.failed" {
		t.Fatalf("events %v", s.names)
	}
	if ev := h.lastEvent(1); ev.Attempts != 1 || ev.Provider != "openai" {
		t.Errorf("event %+v", ev)
	}

	h.upstream.FailNext(1, http.StatusBadRequest)
	resp := h.post(trustedKey, `{"model":"resilient","input":"hi"}`)
	decode(t, resp)
	if resp.StatusCode != 400 {
		t.Errorf("status %d", resp.StatusCode)
	}
	if ev := h.lastEvent(2); ev.Attempts != 1 {
		t.Errorf("event %+v", ev)
	}
}

func TestPreviousResponseIDIsNotMovedToAnotherProvider(t *testing.T) {
	h := newHarnessWith(t, fastBackoff)
	h.upstream.FailNext(3, http.StatusBadGateway)
	resp := h.post(trustedKey, `{"model":"resilient","input":"hi","previous_response_id":"resp_1"}`)
	decode(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ev := h.lastEvent(1); ev.Provider != "openai" || ev.Attempts != 3 {
		t.Errorf("event %+v", ev)
	}
}

func TestFallbackTheCallerMayNotUseIsSkipped(t *testing.T) {
	h := newHarnessWith(t, fastBackoff)
	resp := h.post(trustedKey, `{"model":"llama","input":"hi"}`)
	decode(t, resp)
	key := h.createKey(t, `{"allowed_models":["resilient"]}`)
	h.upstream.FailNext(3, http.StatusBadGateway)
	resp = h.post(key, `{"model":"resilient","input":"hi"}`)
	decode(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ev := h.lastEvent(2); ev.Provider != "openai" {
		t.Errorf("served by %s", ev.Provider)
	}
}

// createKey makes a key for the first application with the given policy.
func (h *harness) createKey(t *testing.T, policy string) string {
	t.Helper()
	resp := h.admin(http.MethodGet, "/admin/applications", "")
	var apps struct {
		Data []struct{ ID string }
	}
	_ = json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()
	resp = h.admin(http.MethodPost, "/admin/applications/"+apps.Data[0].ID+"/keys", policy)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create key: %d", resp.StatusCode)
	}
	return decode(t, resp)["key"].(string)
}

func TestBadKeyIsSidelined(t *testing.T) {
	h := newHarnessWith(t, fastBackoff)
	for i := range 6 {
		resp := h.post(trustedKey, `{"model":"pool/gpt-fake","input":"hi"}`)
		if b := decode(t, resp); resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d %v", i, resp.StatusCode, b)
		}
	}
	calls := 0
	for _, ev := range h.events(6) {
		if ev.Deployment != "good" {
			t.Errorf("served by %q", ev.Deployment)
		}
		calls += ev.Attempts
	}
	// The revoked key is tried at most once, then kept out.
	if calls > 7 {
		t.Errorf("%d upstream calls for 6 requests", calls)
	}
}
