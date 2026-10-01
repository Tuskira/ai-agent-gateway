// Package trace implements the sliver of W3C Trace Context the gateway
// needs: parsing an inbound traceparent header, minting ids when there
// isn't one, and formatting the header it propagates to backends.
//
// It is deliberately not an OpenTelemetry SDK. The gateway's job here is
// correlation -- keep one trace id across the inbound request, the access
// log row and the outbound backend call -- which is a string grammar, not
// a tracing pipeline. An OTel exporter can be layered on later without
// changing this contract.
package trace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// W3C Trace Context constants. See https://www.w3.org/TR/trace-context/.
const (
	// Version is the only traceparent version defined today.
	Version = "00"
	// TraceIDLength and SpanIDLength are lengths in hex characters.
	TraceIDLength = 32
	SpanIDLength  = 16
	// FlagsSampled and FlagsNotSampled are the two flag bytes in use.
	FlagsSampled    = "01"
	FlagsNotSampled = "00"

	// Header is the inbound/outbound header name.
	Header = "traceparent"
)

// ParseTraceparent parses a W3C traceparent header of the form
// "00-<32 hex trace id>-<16 hex span id>-<2 hex flags>".
//
// Validation is strict on purpose: a traceparent is echoed into the
// access log and forwarded to backends, so accepting a malformed one
// would let a caller inject arbitrary text into both.
func ParseTraceparent(traceparent string) (traceID, parentSpanID, flags string, err error) {
	tp := strings.TrimSpace(traceparent)

	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		return "", "", "", fmt.Errorf("trace: invalid traceparent %q: want 4 '-'-separated fields, got %d", traceparent, len(parts))
	}

	version, traceID, parentSpanID, flags := parts[0], parts[1], parts[2], parts[3]

	if version != Version {
		return "", "", "", fmt.Errorf("trace: unsupported traceparent version %q (want %q)", version, Version)
	}
	if !ValidateTraceID(traceID) {
		return "", "", "", fmt.Errorf("trace: invalid trace id %q: want %d non-zero hex characters", traceID, TraceIDLength)
	}
	if !ValidateSpanID(parentSpanID) {
		return "", "", "", fmt.Errorf("trace: invalid span id %q: want %d non-zero hex characters", parentSpanID, SpanIDLength)
	}
	if !isHex(flags, 2) {
		return "", "", "", fmt.Errorf("trace: invalid trace flags %q: want 2 hex characters", flags)
	}

	return traceID, parentSpanID, flags, nil
}

// FormatTraceparent renders a traceparent header. An empty flags value
// defaults to sampled.
func FormatTraceparent(traceID, spanID, flags string) string {
	if flags == "" {
		flags = FlagsSampled
	}
	return Version + "-" + traceID + "-" + spanID + "-" + flags
}

// GenerateTraceID returns a fresh random 128-bit trace id, hex encoded.
func GenerateTraceID() string { return randomHex(16) }

// GenerateSpanID returns a fresh random 64-bit span id, hex encoded.
func GenerateSpanID() string { return randomHex(8) }

// ValidateTraceID reports whether traceID is 32 hex characters and not
// all zeros (which the spec reserves as "invalid").
func ValidateTraceID(traceID string) bool {
	return isHex(traceID, TraceIDLength) && !allZeros(traceID)
}

// ValidateSpanID reports whether spanID is 16 hex characters and not all
// zeros.
func ValidateSpanID(spanID string) bool {
	return isHex(spanID, SpanIDLength) && !allZeros(spanID)
}

// Context is the trace state carried alongside one gateway request: the
// trace id it belongs to, the span id the gateway minted for itself, and
// the sampling flags inherited from the caller (or defaulted).
type Context struct {
	TraceID string
	SpanID  string
	Flags   string
}

// FromHeader derives a Context from an inbound traceparent value. A
// missing or malformed header starts a fresh trace rather than failing
// the request: correlation is best-effort, and a caller with a broken
// tracing library must still get its tool call served. The second return
// value reports whether the inbound header was usable, which the caller
// can log.
func FromHeader(traceparent string) (Context, bool) {
	if traceparent != "" {
		if traceID, _, flags, err := ParseTraceparent(traceparent); err == nil {
			return Context{TraceID: traceID, SpanID: GenerateSpanID(), Flags: flags}, true
		}
	}
	return Context{TraceID: GenerateTraceID(), SpanID: GenerateSpanID(), Flags: FlagsSampled}, traceparent == ""
}

// Traceparent renders c as the header value to forward to a backend.
func (c Context) Traceparent() string {
	if c.TraceID == "" || c.SpanID == "" {
		return ""
	}
	return FormatTraceparent(c.TraceID, c.SpanID, c.Flags)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	for {
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand does not fail on any supported platform;
			// if it somehow did, a zero id is invalid per spec, so
			// fall back to a fixed non-zero value rather than emit
			// one a collector would reject.
			for i := range buf {
				buf[i] = 0xde
			}
			return hex.EncodeToString(buf)
		}
		if !allZeroBytes(buf) {
			return hex.EncodeToString(buf)
		}
	}
}

func isHex(s string, want int) bool {
	if len(s) != want {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func allZeros(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

func allZeroBytes(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
