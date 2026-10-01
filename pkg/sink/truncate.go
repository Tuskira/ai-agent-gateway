package sink

// TruncateBody caps b to at most max bytes, reporting whether anything was
// cut. max <= 0 means unbounded (never truncates). This is the one-shot
// truncation rule every capture path that already holds a full body in
// memory applies before storing it in AccessLog.RequestBody/ResponseBody or
// LLMCall.RequestBody/ResponseBody/Messages: internal/dataplane/transport's
// request-body capture, internal/llmplane's request-body capture, and
// internal/api/handlers' ingest endpoint (POST /api/v1/ingest) all call this
// rather than each reimplementing "keep the first N bytes, flag it when we
// did". A response body that arrives incrementally (streamed) instead uses
// its own bounded writer (internal/llmplane's boundedCap,
// internal/dataplane/transport's statusRecorder) -- this helper is for the
// simpler case of a value already fully in hand.
func TruncateBody(b []byte, max int) (out []byte, truncated bool) {
	if max <= 0 || len(b) <= max {
		return b, false
	}
	return b[:max], true
}
