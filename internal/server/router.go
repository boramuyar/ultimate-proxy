package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/provider"
)

// Route is where a model name is served.
type Route struct {
	Provider      provider.Provider
	UpstreamModel string
}

// Router maps client-facing model names to upstreams.
type Router struct {
	providers map[string]provider.Provider
	models    map[string]Route
}

func NewRouter(cfg *config.Config) (*Router, error) {
	client := provider.NewHTTPClient(cfg.ResponseHeaderTimeout)
	r := &Router{providers: map[string]provider.Provider{}, models: map[string]Route{}}
	for _, p := range cfg.Providers {
		switch p.Type {
		case "openai":
			r.providers[p.Name] = provider.NewOpenAI(p.Name, p.BaseURL, p.APIKey, p.Headers, client)
		case "anthropic":
			r.providers[p.Name] = provider.NewAnthropic(p.Name, p.BaseURL, p.APIKey, p.Headers, p.DefaultMaxTokens, client)
		default:
			return nil, fmt.Errorf("unknown provider type %q", p.Type)
		}
	}
	for _, m := range cfg.Models {
		r.models[m.Name] = Route{Provider: r.providers[m.Provider], UpstreamModel: m.UpstreamModel}
	}
	return r, nil
}

// Resolve finds a configured model alias, or accepts "<provider>/<model>".
func (r *Router) Resolve(model string) (Route, bool) {
	if rt, ok := r.models[model]; ok {
		return rt, true
	}
	if prov, upstream, ok := strings.Cut(model, "/"); ok && upstream != "" {
		if p, ok := r.providers[prov]; ok {
			return Route{Provider: p, UpstreamModel: upstream}, true
		}
	}
	return Route{}, false
}

// Models lists configured aliases.
func (r *Router) Models() []string {
	out := make([]string, 0, len(r.models))
	for name := range r.models {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
