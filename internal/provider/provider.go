// Package provider defines upstream model providers and the adapters that
// translate between Open Responses and each provider's native API.
package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
)

// Call is one request routed to an upstream.
type Call struct {
	Env           *openresponses.Envelope // the parsed client request
	Model         string                  // model name the client asked for
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

// FailureKind says how routing should treat an upstream that failed before
// sending anything.
type FailureKind int

const (
	// Rejected: the upstream refused the request itself (400, 404, 422...).
	// Another deployment would refuse it too.
	Rejected FailureKind = iota
	// RateLimited: 429. The deployment should cool down.
	RateLimited
	// Unavailable: 5xx, a connection error or a header timeout. Worth
	// retrying, and counts toward the deployment's circuit breaker.
	Unavailable
	// AuthFailed: the upstream refused the proxy's credentials. The
	// deployment's key is wrong or revoked.
	AuthFailed
)

// Failure is an upstream failure before any event was sent, so the request
// can be tried again elsewhere. It unwraps to the client-facing error.
type Failure struct {
	*openresponses.APIError
	Kind       FailureKind
	RetryAfter time.Duration // from the Retry-After header, if any
}

func (f *Failure) Unwrap() error { return f.APIError }

// upstreamError maps a non-2xx upstream reply to a client-facing error. Client
// errors keep their status; upstream auth and server failures become 502s so
// callers can tell the proxy's upstream apart from their own request.
func upstreamError(provider string, resp *http.Response, message string) *Failure {
	status := resp.StatusCode
	f := &Failure{RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	switch {
	case status == http.StatusTooManyRequests:
		f.Kind = RateLimited
		f.APIError = openresponses.NewError(status, openresponses.ErrTooManyRequests, "upstream_rate_limited", message, "")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		f.Kind = AuthFailed
		f.APIError = openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_auth_failed", provider+" rejected the proxy's credentials: "+message, "")
	case status == http.StatusNotFound:
		f.APIError = openresponses.NewError(http.StatusBadRequest, openresponses.ErrInvalidRequest, "model_not_found", message, "model")
	case status >= 400 && status < 500:
		f.APIError = openresponses.NewError(status, openresponses.ErrInvalidRequest, "upstream_invalid_request", message, "")
	case status == 529 || status == http.StatusServiceUnavailable:
		f.Kind = Unavailable
		f.APIError = openresponses.NewError(http.StatusServiceUnavailable, openresponses.ErrServer, "upstream_overloaded", message, "")
	default:
		f.Kind = Unavailable
		f.APIError = openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_error", message, "")
	}
	return f
}

// retryAfter parses a Retry-After header: seconds or an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func networkError(provider string, err error) *Failure {
	return &Failure{Kind: Unavailable, APIError: openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_unreachable", provider+": "+err.Error(), "")}
}

func streamError(provider string, err error) *openresponses.APIError {
	return openresponses.NewError(http.StatusBadGateway, openresponses.ErrServer, "upstream_stream_error", provider+": "+err.Error(), "")
}
