package trace_test

import (
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

func TestParseTraceparent(t *testing.T) {
	const valid = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	traceID, spanID, flags, err := trace.ParseTraceparent(valid)
	if err != nil {
		t.Fatalf("ParseTraceparent(valid) = %v", err)
	}
	if traceID != "0af7651916cd43dd8448eb211c80319c" || spanID != "b7ad6b7169203331" || flags != "01" {
		t.Fatalf("parsed %q/%q/%q", traceID, spanID, flags)
	}

	// A traceparent is echoed into the access log and forwarded to
	// backends, so a lax parser is a header-injection primitive.
	invalid := []string{
		"",
		"garbage",
		"01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",   // version
		"00-00000000000000000000000000000000-b7ad6b7169203331-01",   // zero trace id
		"00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01",   // zero span id
		"00-0AF7651916CD43DD8448EB211C80319C-b7ad6b7169203331-01",   // uppercase
		"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331",      // too few fields
		"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-x", // too many
		"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-zz",   // non-hex flags
	}
	for _, tp := range invalid {
		if _, _, _, err := trace.ParseTraceparent(tp); err == nil {
			t.Errorf("ParseTraceparent(%q) = nil error, want a rejection", tp)
		}
	}
}

func TestFormatTraceparentRoundTrips(t *testing.T) {
	traceID, spanID := trace.GenerateTraceID(), trace.GenerateSpanID()
	header := trace.FormatTraceparent(traceID, spanID, "")

	gotTrace, gotSpan, gotFlags, err := trace.ParseTraceparent(header)
	if err != nil {
		t.Fatalf("generated header did not parse: %v (%q)", err, header)
	}
	if gotTrace != traceID || gotSpan != spanID || gotFlags != trace.FlagsSampled {
		t.Fatalf("round trip lost data: %q/%q/%q", gotTrace, gotSpan, gotFlags)
	}
}

func TestFromHeaderContinuesAnInboundTrace(t *testing.T) {
	const inbound = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	tc, ok := trace.FromHeader(inbound)
	if !ok {
		t.Fatal("FromHeader reported a usable header as unusable")
	}
	if tc.TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace id = %q, want the inbound one", tc.TraceID)
	}
	if tc.SpanID == "b7ad6b7169203331" {
		t.Error("the gateway must mint its own span id, not reuse the caller's")
	}
	if _, _, _, err := trace.ParseTraceparent(tc.Traceparent()); err != nil {
		t.Errorf("propagated header is invalid: %v", err)
	}
}

func TestFromHeaderStartsAFreshTraceWhenTheInboundOneIsBroken(t *testing.T) {
	// Correlation is best-effort: a caller with a broken tracing
	// library must still get its tool call served.
	tc, ok := trace.FromHeader("not-a-traceparent")
	if ok {
		t.Error("a malformed header must be reported as unusable")
	}
	if !trace.ValidateTraceID(tc.TraceID) || !trace.ValidateSpanID(tc.SpanID) {
		t.Fatalf("fallback produced an invalid context: %+v", tc)
	}
}

func TestGeneratedIDsAreDistinct(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for range 100 {
		id := trace.GenerateTraceID()
		if _, dup := seen[id]; dup {
			t.Fatalf("GenerateTraceID repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}
