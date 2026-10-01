package handlers

import (
	"net/http"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
)

// formatTime renders t as RFC3339 for JSON responses.
func formatTime(t time.Time) string {
	return t.Format(time.RFC3339)
}

// formatTimePtr renders a nullable timestamp, or "" (rendered as
// omitempty) when nil.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// opsUnavailableMessage is the body of every 503 an ops-backed route
// returns when no data plane was handed to this router -- these routes
// mount either way (so the OpenAPI surface stays complete) and must not
// crash. cmd/gateway builds the data plane whenever the API plane is
// enabled, so a running gateway does not take this path; it is the
// contract for an embedder, or a test, that leaves the Deps field nil.
const opsUnavailableMessage = "MCP data plane is not enabled on this instance"

// writeOpsUnavailable writes the 503 every ConnectorOps/CacheOps-backed
// handler returns when its Deps field is nil.
func writeOpsUnavailable(w http.ResponseWriter) {
	httpx.Unavailable(w, opsUnavailableMessage)
}

// paginate slices items to the requested page. Every list handler fetches
// its full result set from the store (these are lightweight control-plane
// tables, not expected to reach a size where that's a problem) and
// applies limit/offset in memory here, rather than pushing LIMIT/OFFSET
// into the store interface.
func paginate[T any](items []T, p httpx.Pagination) []T {
	if p.Offset >= len(items) {
		return []T{}
	}
	end := p.Offset + p.Limit
	if end > len(items) {
		end = len(items)
	}
	return items[p.Offset:end]
}
