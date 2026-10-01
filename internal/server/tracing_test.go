package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// otlpSpan is the part of an OTLP/JSON span the tests read.
type otlpSpan struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Kind         int    `json:"kind"`
	Attributes   []struct {
		Key   string                     `json:"key"`
		Value map[string]json.RawMessage `json:"value"`
	} `json:"attributes"`
	Status *struct {
		Code int `json:"code"`
	} `json:"status"`
}

// collector is an OTLP/HTTP endpoint that keeps what it receives.
type collector struct {
	mu    sync.Mutex
	raw   []byte
	spans []otlpSpan
}

func newCollector(t *testing.T) (*collector, string) {
	c := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("X-Collector-Key") != "k" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []otlpSpan `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.raw = append(c.raw, body...)
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

// spanAttrs flattens attributes to their JSON text, with strings unquoted.
func spanAttrs(s otlpSpan) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		for _, v := range kv.Value {
			var str string
			if json.Unmarshal(v, &str) == nil {
				out[kv.Key] = str
			} else {
				out[kv.Key] = string(v)
			}
		}
	}
	return out
}

func TestTracing(t *testing.T) {
	c, url := newCollector(t)
	h := newHarnessWith(t, "tracing: {endpoint: \""+url+"\", headers: {X-Collector-Key: k}}\n")
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	resp := h.post(trustedKey, `{"model":"gpt","input":"a secret prompt"}`,
		"traceparent", "00-"+traceID+"-00f067aa0ba902b7-01", "X-Proxy-Tags", "feature=search",
		"X-Proxy-User-Email", "alice@example.com")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, decode(t, resp))
	}
	resp.Body.Close()
	// A request refused before it becomes a model call gets no span.
	resp = h.post(trustedKey, `{"model":"gpt","input":"hi"}`, "X-Proxy-Tags", "Bad=tag")
	resp.Body.Close()
	// An upstream failure is an error span.
	resp = h.postTo("/badkey/v1/responses", trustedKey, `{"model":"gpt","input":"hi"}`)
	resp.Body.Close()
	if err := h.srv.Tracer().Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.spans) != 2 {
		t.Fatalf("got %d spans, want 2 (a bad tags header is refused before the call is admitted)", len(c.spans))
	}
	if f := c.spans[1]; f.Status == nil || f.Status.Code != 2 || spanAttrs(f)["error.type"] == "" {
		t.Errorf("failed call: status %+v, attributes %v", f.Status, spanAttrs(f))
	}
	s := c.spans[0]
	if s.TraceID != traceID {
		t.Errorf("trace id %s, want the caller's %s", s.TraceID, traceID)
	}
	if s.ParentSpanID != "00f067aa0ba902b7" || len(s.SpanID) != 16 {
		t.Errorf("span %s, parent %s", s.SpanID, s.ParentSpanID)
	}
	if s.Name != "chat gpt" || s.Kind != 2 || s.Status != nil {
		t.Errorf("span name %q, kind %d, status %+v", s.Name, s.Kind, s.Status)
	}
	a := spanAttrs(s)
	for k, want := range map[string]string{
		"gen_ai.provider.name": "openai", "gen_ai.request.model": "gpt",
		"omni_proxy.tenant": "acme", "omni_proxy.application": "chat",
		"omni_proxy.status": "completed", "omni_proxy.tag.feature": "search",
	} {
		if a[k] != want {
			t.Errorf("%s = %q, want %q (all: %v)", k, a[k], want, a)
		}
	}
	// 64-bit integers are strings in OTLP/JSON.
	if n := a["gen_ai.usage.input_tokens"]; n == "" || n == "0" {
		t.Errorf("no token counts: %v", a)
	}
	if _, ok := a["user.email"]; ok {
		t.Errorf("user email sent without include_user")
	}
	if strings.Contains(string(c.raw), "secret prompt") || strings.Contains(string(c.raw), "alice@") {
		t.Error("the export carries the prompt or the user")
	}
}

func TestTracingIncludeUser(t *testing.T) {
	c, url := newCollector(t)
	h := newHarnessWith(t, "tracing: {endpoint: \""+url+"/v1/traces\", headers: {X-Collector-Key: k}, include_user: true}\n")
	resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`, "X-Proxy-User-Email", "alice@example.com")
	resp.Body.Close()
	if err := h.srv.Tracer().Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.spans) != 1 || spanAttrs(c.spans[0])["user.email"] != "alice@example.com" {
		t.Fatalf("spans %v", c.spans)
	}
}

func TestTracingOffByDefault(t *testing.T) {
	h := newHarness(t)
	resp := h.post(trustedKey, `{"model":"gpt","input":"hi"}`, "traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp.Body.Close()
	if h.srv.Tracer().Shutdown(context.Background()) != nil {
		t.Fatal("shutdown of the off tracer failed")
	}
}
