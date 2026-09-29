// Package provider defines upstream model providers and the adapters that
// translate between Open Responses and each provider's native API.
package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
)

// Call is one request routed to an upstream.
type Call struct {
	Req           *openresponses.Request
	Body          []byte // the client's original request body
	Model         string // model name the client asked for
	UpstreamModel string
	ResponseID    string
	CreatedAt     int64
}

// Usage is the token accounting the proxy meters. Unlike the spec usage
// object, it separates cache writes, which some providers bill differently.
type Usage struct {
	InputTokens       int // all input tokens, including cached and cache-write tokens
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	ReasoningTokens   int
	Reported          bool // false when the upstream never reported usage
}

// Result is the outcome of a call.
type Result struct {
	Response  json.RawMessage // final ResponseResource, returned to non-streaming clients
	Status    string          // completed, incomplete or failed
	Usage     Usage
	ErrorCode string
}

// Provider serves Open Responses requests from one upstream. Adapters always
// stream from the upstream and report events to sink (nil for non-streaming
// clients). If the call fails before any event was sent, Create returns an
// *openresponses.APIError the caller can turn into an HTTP error. If it fails
// mid-stream, the adapter has already emitted the error events.
type Provider interface {
	Name() string
	Create(ctx context.Context, call *Call, sink openresponses.Sink) (*Result, error)
}

// NewHTTPClient returns a client tuned for many long-lived streaming requests
// to a few hosts.
func NewHTTPClient(responseHeaderTimeout time.Duration) *http.Client {
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
	return &http.Client{Transport: t}
}

// upstreamError maps a non-2xx upstream reply to a client-facing error. Client
// errors keep their status; upstream auth and server failures become 502s so
// callers can tell the proxy's upstream apart from their own request.
func upstreamError(provider string, status int, message string) *openresponses.APIError {
	switch {
	case status == http.StatusTooManyRequests:
		return openresponses.NewError(status, openresponses.ErrTooManyRequests, "upstream_rate_limited", message, "")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_auth_failed", provider+" rejected the proxy's credentials: "+message, "")
	case status == http.StatusNotFound:
		return openresponses.NewError(http.StatusBadRequest, openresponses.ErrInvalidRequest, "model_not_found", message, "model")
	case status >= 400 && status < 500:
		return openresponses.NewError(status, openresponses.ErrInvalidRequest, "upstream_invalid_request", message, "")
	case status == 529 || status == http.StatusServiceUnavailable:
		return openresponses.NewError(http.StatusServiceUnavailable, openresponses.ErrServer, "upstream_overloaded", message, "")
	default:
		return openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_error", message, "")
	}
}

func networkError(provider string, err error) *openresponses.APIError {
	return openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_unreachable", provider+": "+err.Error(), "")
}

func streamError(provider string, err error) *openresponses.APIError {
	return openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_stream_error", provider+": "+err.Error(), "")
}
