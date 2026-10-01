// Package otel is a sink.LogSink that turns AccessLog and LLMCall records
// into OpenTelemetry spans, exported via OTLP (http or grpc).
//
// One span per record: "mcp.<method>" for an AccessLog, "llm.<provider>"
// for an LLMCall. When the record's TraceID is a valid, parseable W3C
// trace id, the span is created as a child of that trace (via a
// synthetic remote parent SpanContext) so it lands in the same trace as
// the caller's own spans; otherwise it starts a fresh trace. Only
// identifiers, status and timing are ever set as attributes -- bodies and
// headers are never recorded on a span, mirroring every other sink.
//
// Export is asynchronous: spans are handed to an sdktrace.BatchSpanProcessor,
// which queues and flushes them on its own background goroutine, so
// WriteAccess/WriteLLMCall never block the request path (satisfying
// sink.LogSink's contract without this package needing its own queue).
package otel

// Config configures the otel sink. It is built from
// internal/config.Config.Sinks.Otel by cmd/gateway/main.go.
type Config struct {
	// Endpoint is the OTLP collector address, e.g. "localhost:4318" (http)
	// or "localhost:4317" (grpc). Required.
	Endpoint string
	// Protocol selects the OTLP transport: "http" (default) or "grpc".
	Protocol string
	// Insecure disables TLS on the OTLP connection (plaintext). Default
	// false.
	Insecure bool
	// Headers are extra headers sent with every OTLP export request (e.g.
	// an ingest API key).
	Headers map[string]string
	// ServiceName sets the exported resource's service.name attribute.
	// Defaults to "tusk-ai-secured-gateway" when empty.
	ServiceName string
}

const defaultServiceName = "tusk-ai-secured-gateway"
