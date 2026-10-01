package insights

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/store"
)

func TestWebhooks(t *testing.T) {
	got := make(chan map[string]any, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		got <- m
	}))
	defer srv.Close()

	wh := NewWebhooks(srv.URL+"/hook", srv.URL+"/slack", slog.New(slog.NewTextHandler(io.Discard, nil)))
	wh.Notify("opened", store.Insight{Kind: KindErrorRate, Severity: "critical", Title: "app: 50% fail", Detail: "boom"})
	var generic, slack map[string]any
	for range 2 {
		select {
		case m := <-got:
			if _, ok := m["text"]; ok {
				slack = m
			} else {
				generic = m
			}
		case <-time.After(5 * time.Second):
			t.Fatal("webhook not called")
		}
	}
	if generic["event"] != "insight.opened" || generic["insight"].(map[string]any)["kind"] != KindErrorRate {
		t.Fatalf("generic payload %v", generic)
	}
	if text, _ := slack["text"].(string); !strings.Contains(text, ":rotating_light: app: 50% fail") {
		t.Fatalf("slack payload %v", slack)
	}
	if NewWebhooks("", "", nil) != nil {
		t.Fatal("expected nil without URLs")
	}
}
