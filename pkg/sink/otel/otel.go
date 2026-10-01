package otel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// tracerName identifies this package's spans as their own instrumentation
// scope.
const tracerName = "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/otel"

// shutdownTimeout bounds Close's flush of any spans still queued in the
// batch processor.
const shutdownTimeout = 5 * time.Second

// Sink is a sink.LogSink that exports one span per record over OTLP. The
// zero value is not usable; construct with New.
type Sink struct {
	tp     *sdktrace.TracerProvider
	tracer oteltrace.Tracer
}

// New builds an OTLP span exporter per cfg (http or grpc, selected by
// cfg.Protocol), wraps it in a batching TracerProvider, and returns the
// sink.LogSink over it. Building the exporter does not dial the
// collector: both otlptracehttp and otlptracegrpc connect lazily on
// first export, so a collector that is down at startup does not fail
// New or block the gateway's boot.
func New(cfg Config) (sink.LogSink, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("otel: endpoint is required")
	}

	ctx := context.Background()
	exp, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("otel: build exporter: %w", err)
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", serviceName)))
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return newSink(tp), nil
}

// newExporter builds the OTLP span exporter selected by cfg.Protocol.
func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch cfg.Protocol {
	case "", "http":
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		return otlptracehttp.New(ctx, opts...)
	case "grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		return otlptracegrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown protocol %q (want %q or %q)", cfg.Protocol, "http", "grpc")
	}
}

// newSink wraps an already-built TracerProvider. Tests use it directly
// with an in-memory span processor in place of New's real OTLP exporter.
func newSink(tp *sdktrace.TracerProvider) *Sink {
	return &Sink{tp: tp, tracer: tp.Tracer(tracerName)}
}

// WriteAccess emits one span "mcp.<method>" for a, parented onto a's
// trace id when it is a valid W3C trace id (see parentContext), with
// start/end derived from a.Timestamp and a.DurationMS. Never includes
// headers or bodies.
func (s *Sink) WriteAccess(a *sink.AccessLog) {
	ctx := parentContext(a.TraceID)
	end := a.Timestamp.Add(time.Duration(a.DurationMS) * time.Millisecond)

	_, span := s.tracer.Start(ctx, "mcp."+validUTF8(orUnknown(a.Method)),
		oteltrace.WithTimestamp(a.Timestamp),
		oteltrace.WithAttributes(
			str("tenant_id", a.TenantID),
			str("principal", a.Principal),
			str("key_id", a.KeyID),
			str("session_id", a.SessionID),
			str("client_session_id", a.ClientSessionID),
			str("request_id", a.RequestID),
			str("connector_id", a.ConnectorID),
			str("tool_name", a.ToolName),
			str("skill_name", a.SkillName),
			attribute.Int("status_code", a.StatusCode),
			str("error_code", a.ErrorCode),
			attribute.Int64("duration_ms", a.DurationMS),
			attribute.Int64("bytes_in", a.BytesIn),
			attribute.Int64("bytes", a.Bytes),
			str("client_ip", a.ClientIP),
		),
	)
	span.End(oteltrace.WithTimestamp(end))
}

// WriteLLMCall emits one span "llm.<provider>" for l, with start/end
// derived from l.Timestamp and l.DurationMS. LLMCall carries no trace id,
// so every LLM span starts its own trace. Never includes headers, bodies,
// messages, system, or tools.
func (s *Sink) WriteLLMCall(l *sink.LLMCall) {
	end := l.Timestamp.Add(time.Duration(l.DurationMS) * time.Millisecond)

	_, span := s.tracer.Start(context.Background(), "llm."+validUTF8(orUnknown(l.Provider)),
		oteltrace.WithTimestamp(l.Timestamp),
		oteltrace.WithAttributes(
			str("tenant_id", l.TenantID),
			str("principal", l.Principal),
			str("key_id", l.KeyID),
			str("session_id", l.SessionID),
			str("request_id", l.RequestID),
			str("model", l.Model),
			str("path", l.Path),
			attribute.Int("status_code", l.StatusCode),
			attribute.Int64("duration_ms", l.DurationMS),
			attribute.Int64("input_tokens", l.InputTokens),
			attribute.Int64("output_tokens", l.OutputTokens),
			attribute.Int64("cache_read_tokens", l.CacheReadTokens),
			attribute.Int64("cache_creation_tokens", l.CacheCreationTokens),
			str("stop_reason", l.StopReason),
			attribute.Bool("stream", l.Stream),
			str("client_name", l.ClientName),
			attribute.Int("skills_used_count", len(l.SkillsUsed)),
			attribute.Int("mcp_tools_used_count", len(l.MCPToolsUsed)),
		),
	)
	span.End(oteltrace.WithTimestamp(end))
}

// str is attribute.String with invalid UTF-8 replaced: caller-controlled values
// (session header, URL path, X-Forwarded-For) would otherwise fail the whole
// OTLP export batch.
func str(k, v string) attribute.KeyValue { return attribute.String(k, validUTF8(v)) }

func validUTF8(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// Close flushes every span still queued in the batch processor and shuts
// down the exporter, bounded by shutdownTimeout.
func (s *Sink) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return s.tp.Shutdown(ctx)
}

// parentContext returns a context carrying a remote SpanContext for
// traceID when it is a valid, parseable W3C trace id, so the span
// tracer.Start creates under it joins that trace; otherwise it returns
// context.Background(), which starts a fresh trace.
//
// The parent span id is synthetic: AccessLog only carries a trace id, not
// the id of the specific span that owns it in the caller's trace. It
// exists only to make the SpanContext valid (a SpanContext with a
// zero/invalid span id is treated by the SDK as absent, which would
// start a new trace instead of joining this one). A backend that
// resolves the parent reference will simply show this span as the
// trace's root; what makes it "join the caller's trace" is the trace id,
// which is exact.
func parentContext(traceID string) context.Context {
	tid, err := oteltrace.TraceIDFromHex(traceID)
	if err != nil || !tid.IsValid() {
		return context.Background()
	}
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     syntheticSpanID(traceID),
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})
	return oteltrace.ContextWithRemoteSpanContext(context.Background(), sc)
}

// syntheticSpanID derives a deterministic, non-zero span id from traceID
// (already validated 32 hex chars) so parentContext's SpanContext is
// valid, without minting genuine randomness for a span that was never
// actually created.
func syntheticSpanID(traceID string) oteltrace.SpanID {
	var id oteltrace.SpanID
	tail := traceID[len(traceID)-16:] // last 16 hex chars = 8 bytes
	for i := 0; i < 8; i++ {
		b, err := strconv.ParseUint(tail[i*2:i*2+2], 16, 8)
		if err != nil {
			id[i] = 0xaa
			continue
		}
		id[i] = byte(b)
	}
	if id == (oteltrace.SpanID{}) {
		id[0] = 0xaa // never all-zero: that is an invalid span id
	}
	return id
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
