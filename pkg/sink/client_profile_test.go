package sink

import (
	"strings"
	"testing"
)

func TestCapProfile(t *testing.T) {
	long := strings.Repeat("é", MaxProfileLen) // two bytes per rune
	cases := map[string]string{
		"":                       "",
		"   ":                    "",
		"SOC Agent":              "SOC Agent",
		"  SOC Agent \t":         "SOC Agent",
		"SOC\x00 Ag\x1bent":      "SOC Agent",
		"bad\xffutf8":            "badutf8",
		strings.Repeat("a", 200): strings.Repeat("a", MaxProfileLen),
		long:                     strings.Repeat("é", MaxProfileLen/2), // cut on a rune boundary
	}
	for in, want := range cases {
		if got := CapProfile(in); got != want {
			t.Errorf("CapProfile(%.20q) = %.20q (len %d), want %.20q (len %d)", in, got, len(got), want, len(want))
		}
	}
}
