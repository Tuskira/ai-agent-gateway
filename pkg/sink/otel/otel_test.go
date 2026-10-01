package otel

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// newTestSink returns a Sink wired to an in-memory span exporter via a
// synchronous (non-batching) processor, so WriteAccess/WriteLLMCall's
// span is visible to the test immediately -- no flush/close needed.
func newTestSink(t *testing.T) (*Sink, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	return newSink(tp), exp
}

func TestWriteAccess_EmitsSpanWithAttributes(t *testing.T) {
	s, exp := newTestSink(t)

	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.WriteAccess(&sink.AccessLog{
		Timestamp:   start,
		TenantID:    "tenant-a",
		Principal:   "user-1",
		KeyID:       "key-1",
		SessionID:   "sess-1",
		RequestID:   "req-1",
		Method:      "tools/call",
		ConnectorID: "conn-1",
		ToolName:    "echo",
		StatusCode:  200,
		ErrorCode:   "",
		DurationMS:  42,
		BytesIn:     10,
		Bytes:       20,
		ClientIP:    "1.2.3.4",
	})

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	got := spans[0]
	if got.Name != "mcp.tools/call" {
		t.Errorf("span name = %q, want %q", got.Name, "mcp.tools/call")
	}
	if !got.StartTime.Equal(start) {
		t.Errorf("start time = %v, want %v", got.StartTime, start)
	}
	wantEnd := start.Add(42 * time.Millisecond)
	if !got.EndTime.Equal(wantEnd) {
		t.Errorf("end time = %v, want %v", got.EndTime, wantEnd)
	}

	attrs := attrMap(got.Attributes)
	for k, want := range map[string]string{
		"tenant_id": "tenant-a", "principal": "user-1", "key_id": "key-1",
		"session_id": "sess-1", "request_id": "req-1", "connector_id": "conn-1",
		"tool_name": "echo", "client_ip": "1.2.3.4",
	} {
		if attrs[k] != want {
			t.Errorf("attr %s = %q, want %q", k, attrs[k], want)
		}
	}

	// No body/header attributes must ever be present.
	for _, k := range []string{"request_body", "response_body", "headers"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("span must never carry attribute %q (bodies/headers are never recorded)", k)
		}
	}
}

func TestWriteAccess_JoinsCallersTraceWhenTraceIDValid(t *testing.T) {
	s, exp := newTestSink(t)

	const traceID = "0af7651916cd43dd8448eb211c80319c"
	s.WriteAccess(&sink.AccessLog{Timestamp: time.Now(), TraceID: traceID, Method: "tools/list"})

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	gotTraceID := spans[0].SpanContext.TraceID().String()
	if gotTraceID != traceID {
		t.Errorf("span trace id = %q, want the record's trace id %q (must join the caller's trace)", gotTraceID, traceID)
	}
	if !spans[0].Parent.IsValid() {
		t.Error("span has no parent recorded; expected a synthetic remote parent so it joins the caller's trace")
	}
}

func TestWriteAccess_StartsFreshTraceWhenTraceIDInvalid(t *testing.T) {
	s, exp := newTestSink(t)

	for _, bad := range []string{"", "not-hex", "00000000000000000000000000000000"} {
		exp.Reset()
		s.WriteAccess(&sink.AccessLog{Timestamp: time.Now(), TraceID: bad, Method: "tools/list"})
		spans := exp.GetSpans()
		if len(spans) != 1 {
			t.Fatalf("trace id %q: got %d spans, want 1", bad, len(spans))
		}
		if spans[0].Parent.IsValid() {
			t.Errorf("trace id %q: span unexpectedly has a valid parent; want a fresh root trace", bad)
		}
	}
}

func TestWriteLLMCall_EmitsSpanWithAttributes(t *testing.T) {
	s, exp := newTestSink(t)

	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.WriteLLMCall(&sink.LLMCall{
		Timestamp:           start,
		TenantID:            "tenant-a",
		Provider:            "anthropic",
		Model:               "claude-opus",
		Path:                "/v1/messages",
		StatusCode:          200,
		DurationMS:          123,
		InputTokens:         10,
		OutputTokens:        20,
		CacheReadTokens:     5,
		CacheCreationTokens: 1,
		StopReason:          "end_turn",
		Stream:              true,
		ClientName:          "claude-code",
	})

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	got := spans[0]
	if got.Name != "llm.anthropic" {
		t.Errorf("span name = %q, want %q", got.Name, "llm.anthropic")
	}
	wantEnd := start.Add(123 * time.Millisecond)
	if !got.EndTime.Equal(wantEnd) {
		t.Errorf("end time = %v, want %v", got.EndTime, wantEnd)
	}

	attrs := attrMap(got.Attributes)
	if attrs["model"] != "claude-opus" || attrs["stop_reason"] != "end_turn" || attrs["client_name"] != "claude-code" {
		t.Errorf("attrs = %+v", attrs)
	}
	streamAttr, ok := boolAttr(got.Attributes, "stream")
	if !ok || !streamAttr {
		t.Errorf("stream attribute = %v, %v, want true, true", streamAttr, ok)
	}
}

// Discovery names never leave the gateway over OTLP: counts only.
func TestWrite_DiscoveryCountsOnly(t *testing.T) {
	s, exp := newTestSink(t)
	s.WriteLLMCall(&sink.LLMCall{Provider: "anthropic", Model: "m", SkillsUsed: []string{"review-pr", "deploy"}, MCPToolsUsed: []string{"gw__langfuse__get_trace"}})
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans", len(spans))
	}
	attrs := attrMap(spans[0].Attributes)
	if attrs["skills_used_count"] != "2" || attrs["mcp_tools_used_count"] != "1" {
		t.Errorf("counts = %q, %q", attrs["skills_used_count"], attrs["mcp_tools_used_count"])
	}
	for k, v := range attrs {
		if strings.Contains(v, "review-pr") || strings.Contains(v, "langfuse") {
			t.Errorf("attribute %s = %q leaks a discovered name", k, v)
		}
	}
}

func TestClose_ShutsDownProvider(t *testing.T) {
	s, _ := newTestSink(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// attrMap renders a span's attributes as a plain map[string]string of
// each key's Emit() rendering, for easy comparison in tests.
func attrMap(attrs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}

func boolAttr(attrs []attribute.KeyValue, key string) (val, ok bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}

// Caller-controlled strings with invalid UTF-8 must not reach the exporter: the
// OTLP protobuf marshal rejects them and drops the whole batch.
func TestWrite_SanitizesInvalidUTF8(t *testing.T) {
	s, exp := newTestSink(t)
	s.WriteLLMCall(&sink.LLMCall{Provider: "openai\xff", SessionID: "a\xffb", Path: "/openai/v1/\xff", Model: "m"})
	for _, sp := range exp.GetSpans() {
		if !utf8.ValidString(sp.Name) {
			t.Errorf("span name %q is not valid UTF-8", sp.Name)
		}
		for _, kv := range sp.Attributes {
			if v := kv.Value.String(); !utf8.ValidString(v) {
				t.Errorf("%s: attribute %s = %q is not valid UTF-8", sp.Name, kv.Key, v)
			}
		}
	}
	if attrs := attrMap(exp.GetSpans()[0].Attributes); attrs["session_id"] != "a�b" {
		t.Errorf("session_id = %q, want a\\uFFFDb", attrs["session_id"])
	}
}
