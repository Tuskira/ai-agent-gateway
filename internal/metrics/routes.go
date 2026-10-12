package metrics

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Route functions turn a request into the bounded "route" label of the
// HTTP metrics. None of them may return a client-controlled value.

// MCPRoute labels a request to the MCP plane by the route that serves it,
// mirroring the mux in internal/dataplane/transport/http.go: GET /health,
// POST and DELETE /mcp, GET /mcp/stream. Anything else, including those
// paths with another method, is "other".
func MCPRoute(r *http.Request) string {
	switch r.URL.Path {
	case "/health":
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return "/health"
		}
	case "/mcp":
		if r.Method == http.MethodPost || r.Method == http.MethodDelete {
			return "/mcp"
		}
	case "/mcp/stream":
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return "/mcp/stream"
		}
	}
	return "other"
}

// LLMRoute labels a request to the LLM plane by provider only, following
// the rules of pickProvider in internal/llmplane/provider.go: an explicit
// /{provider}/... prefix, else the bare native paths (/v1/messages* and
// /v1/complete* are Anthropic, /model/* is Bedrock). The path is never
// part of the label because it carries model ids. /health is "/health".
func LLMRoute(r *http.Request) string {
	path := r.URL.Path
	if path == "/health" {
		return "/health"
	}
	seg := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case "anthropic", "openai", "gemini", "bedrock":
		return seg
	}
	if strings.HasPrefix(path, "/v1/messages") || strings.HasPrefix(path, "/v1/complete") {
		return "anthropic"
	}
	if strings.HasPrefix(path, "/model/") {
		return "bedrock"
	}
	return "other"
}

// APIRoute labels a request to the control API by chi's matched route
// pattern (for example "/api/v1/connectors/{id}"), which is bounded by
// the route table. The UI catch-all mount shows as "/*"; a request no
// route matched is "unmatched". It reads the pattern from the request's
// chi route context, so it only works inside the chi router (see
// api.Deps.Instrument).
func APIRoute(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return unmatchedRoute
}
