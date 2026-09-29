package insights

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/store"
)

// Notifier is told when an insight opens or resolves. It must not block.
type Notifier interface {
	Notify(event string, in store.Insight)
}

type nopNotifier struct{}

func (nopNotifier) Notify(string, store.Insight) {}

// Webhooks posts insight changes to a generic JSON webhook and/or a Slack
// incoming webhook, from a background goroutine.
type Webhooks struct {
	url, slackURL string
	client        *http.Client
	log           *slog.Logger
	queue         chan notification
}

type notification struct {
	event string
	in    store.Insight
}

// NewWebhooks returns nil when neither URL is set.
func NewWebhooks(url, slackURL string, log *slog.Logger) *Webhooks {
	if url == "" && slackURL == "" {
		return nil
	}
	w := &Webhooks{url: url, slackURL: slackURL, client: &http.Client{Timeout: 10 * time.Second}, log: log, queue: make(chan notification, 256)}
	go w.loop()
	return w
}

func (w *Webhooks) Notify(event string, in store.Insight) {
	select {
	case w.queue <- notification{event, in}:
	default:
		w.log.Warn("insight notification dropped; queue full", "kind", in.Kind)
	}
}

func (w *Webhooks) loop() {
	for n := range w.queue {
		if w.url != "" {
			w.post(w.url, map[string]any{"event": "insight." + n.event, "insight": n.in})
		}
		if w.slackURL != "" {
			w.post(w.slackURL, map[string]any{"text": slackText(n.event, &n.in)})
		}
	}
}

func (w *Webhooks) post(url string, payload any) {
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		w.log.Error("insight webhook failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		w.log.Error("insight webhook failed", "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		w.log.Error("insight webhook failed", "status", resp.StatusCode)
	}
}

func slackText(event string, in *store.Insight) string {
	if event == "resolved" {
		return fmt.Sprintf(":white_check_mark: Resolved: %s", in.Title)
	}
	icon := ":warning:"
	if in.Severity == "critical" {
		icon = ":rotating_light:"
	}
	return fmt.Sprintf("%s %s\n%s", icon, in.Title, in.Detail)
}
