package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/omni-proxy/omni-proxy/internal/openresponses"
	"github.com/omni-proxy/omni-proxy/internal/provider"
)

// handleChat serves POST /<provider>/v1/chat/completions. The request and
// the reply pass through unchanged, except that a stream is asked for its
// usage so it can be accounted for; that last chunk is left out for clients
// that did not ask for it.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, p *provider.OpenAI) {
	x := s.admit(w, r, p)
	if x == nil {
		return
	}
	req := x.req
	res, err := p.Chat(r.Context(), req.ChatUpstreamBody(), req.Stream, req.Stream && !req.IncludeUsage, w)
	ev := &x.ev
	if err != nil {
		var ae *openresponses.APIError
		if !errors.As(err, &ae) {
			ae = openresponses.ServerError("proxy_error", err.Error())
		}
		openresponses.WriteError(w, ae)
		ev.Status, ev.ErrorCode, ev.HTTPStatus = "failed", ae.CodeString(), ae.Status
		s.finish(x, time.Time{}, nil)
		return
	}
	ev.Status, ev.ErrorCode, ev.HTTPStatus = res.Status, res.ErrorCode, res.HTTPStatus
	x.setUsage(res.Usage)
	if r.Context().Err() != nil {
		ev.Status, ev.ErrorCode, ev.HTTPStatus = "failed", "client_disconnected", 499
	}
	s.finish(x, res.FirstToken, nil)
}
