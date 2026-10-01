package transport

import (
	"net/http"
	"strings"
	"testing"
)

// TestSanitizeSessionTag is the whitebox counterpart to
// TestAccessLogClientSessionIDIsCapped (http_test.go): it exercises
// control-character stripping directly, since net/http's own client
// refuses to send a header value containing them, so that path can't be
// reached through a real HTTP round trip.
func TestSanitizeSessionTag(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "abc-123", "abc-123"},
		{"strips control characters", "abc\x00\x07def\x7f", "abcdef"},
		{"caps at 128 runes", strings.Repeat("x", 200), strings.Repeat("x", 128)},
		{"invalid utf8 replaced, not dropped", "abc\xffdef", "abc�def"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSessionTag(tc.in); got != tc.want {
				t.Errorf("sanitizeSessionTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFirstNonEmptyHeader(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	r.Header.Set("X-Claude-Code-Session-Id", "cc-1")
	if got := firstNonEmptyHeader(r, "X-Session-Id", "X-Claude-Code-Session-Id"); got != "cc-1" {
		t.Errorf("got %q, want fallback cc-1", got)
	}

	r.Header.Set("X-Session-Id", "s-1")
	if got := firstNonEmptyHeader(r, "X-Session-Id", "X-Claude-Code-Session-Id"); got != "s-1" {
		t.Errorf("got %q, want the first header s-1", got)
	}

	r2, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	if got := firstNonEmptyHeader(r2, "X-Session-Id", "X-Claude-Code-Session-Id"); got != "" {
		t.Errorf("got %q, want empty when neither header is set", got)
	}
}
