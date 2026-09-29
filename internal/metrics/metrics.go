// Package metrics exposes Prometheus metrics for the proxy.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

var (
	Registry = prometheus.NewRegistry()

	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ultimate_proxy_requests_total",
		Help: "Responses requests by tenant, application, model, provider and final status.",
	}, []string{"tenant", "application", "model", "provider", "status"})

	Tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ultimate_proxy_tokens_total",
		Help: "Tokens by tenant, application, model and kind (input, cached_input, cache_write, output, reasoning).",
	}, []string{"tenant", "application", "model", "kind"})

	Latency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ultimate_proxy_request_duration_seconds",
		Help:    "Total request duration, including the upstream.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
	}, []string{"provider", "model"})

	TTFT = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ultimate_proxy_time_to_first_token_seconds",
		Help:    "Time from request to the first output delta for streaming requests.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32},
	}, []string{"provider", "model"})

	UpstreamErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ultimate_proxy_upstream_errors_total",
		Help: "Failed requests by provider and error code.",
	}, []string{"provider", "code"})

	UsageDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ultimate_proxy_usage_events_dropped_total",
		Help: "Usage events dropped because the write queue was full.",
	})

	UsageWriteErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ultimate_proxy_usage_write_errors_total",
		Help: "Usage event batches that failed to write.",
	})

	CacheRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ultimate_proxy_cache_requests_total",
		Help: "Requests by prompt-cache status (hit, or the reason for a miss).",
	}, []string{"tenant", "application", "model", "status"})

	CacheMissedTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ultimate_proxy_cache_missed_tokens_total",
		Help: "Input tokens that should have been served from the prompt cache but were not.",
	}, []string{"tenant", "application", "model"})

	CostUSD = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ultimate_proxy_cost_usd_total",
		Help: "Estimated spend in USD from the prices table.",
	}, []string{"tenant", "application", "model"})

	InsightsOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ultimate_proxy_insights_open",
		Help: "Open insights by kind.",
	}, []string{"kind"})

	UsageQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ultimate_proxy_usage_queue_depth",
		Help: "Usage events waiting to be written.",
	})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		Requests, Tokens, Latency, TTFT, UpstreamErrors, UsageDropped, UsageWriteErrors, UsageQueueDepth,
		CacheRequests, CacheMissedTokens, CostUSD, InsightsOpen,
	)
}
