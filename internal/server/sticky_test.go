package server

import (
	"fmt"
	"net/http"
	"testing"
)

// turn sends one turn of conversation c: the same instructions and first
// user message, then n more items.
func (h *harness) turn(c, n int, headers ...string) (int, map[string]any) {
	h.t.Helper()
	items := fmt.Sprintf(`{"role":"developer","content":"Be brief."},{"role":"user","content":"conversation %d"}`, c)
	for i := range n {
		items += fmt.Sprintf(`,{"role":"assistant","content":"reply %d"},{"role":"user","content":"more %d"}`, i, i)
	}
	resp := h.post(trustedKey, `{"model":"duo/gpt-fake","input":[`+items+`]}`, headers...)
	return resp.StatusCode, decode(h.t.(*testing.T), resp)
}

func TestConversationsStickToADeployment(t *testing.T) {
	h := newHarness(t)
	const convs, turns = 20, 3
	for n := range turns {
		for c := range convs {
			if status, body := h.turn(c, n); status != 200 {
				t.Fatalf("status %d %v", status, body)
			}
		}
	}
	evs := h.events(convs * turns)
	used := map[string]bool{}
	for c := range convs {
		first := evs[c].Deployment
		used[first] = true
		for n := 1; n < turns; n++ {
			if got := evs[n*convs+c].Deployment; got != first {
				t.Errorf("conversation %d moved from %s to %s on turn %d", c, first, got, n)
			}
		}
	}
	if !used["d1"] || !used["d2"] {
		t.Errorf("conversations should spread over both deployments: %v", used)
	}
}

func TestConversationMovesOnceWhenItsDeploymentFails(t *testing.T) {
	h := newHarnessWith(t, fastBackoff)
	h.turn(1, 0)
	home := h.lastEvent(1).Deployment

	h.upstream.FailNext(1, http.StatusServiceUnavailable)
	if status, body := h.turn(1, 1); status != 200 {
		t.Fatalf("status %d %v", status, body)
	}
	moved := h.lastEvent(2)
	if moved.Deployment == home || moved.Attempts != 2 {
		t.Fatalf("event %+v, home %s", moved, home)
	}
	for n := 2; n < 5; n++ {
		h.turn(1, n)
		if ev := h.lastEvent(n + 1); ev.Deployment != moved.Deployment {
			t.Errorf("turn %d went back to %s", n, ev.Deployment)
		}
	}
}

func TestSessionHeaderAndPreviousResponseID(t *testing.T) {
	h := newHarness(t)
	// Different first messages, one session: one deployment.
	for c := range 10 {
		h.turn(c, 0, SessionHeader, "s-1")
	}
	evs := h.events(10)
	for _, ev := range evs[1:] {
		if ev.Deployment != evs[0].Deployment {
			t.Fatalf("session moved from %s to %s", evs[0].Deployment, ev.Deployment)
		}
	}

	// A stored response is continued where it was made.
	resp := h.post(trustedKey, `{"model":"duo/gpt-fake","input":"hi"}`)
	id := decode(t, resp)["id"].(string)
	made := h.lastEvent(11).Deployment
	for i := range 10 {
		resp := h.post(trustedKey, `{"model":"duo/gpt-fake","input":"again","previous_response_id":"`+id+`"}`)
		decode(t, resp)
		if ev := h.lastEvent(12 + i); ev.Deployment != made {
			t.Fatalf("previous_response_id went to %s, made on %s", ev.Deployment, made)
		}
	}
}
