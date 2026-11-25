package sink

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTagsHeader(t *testing.T) {
	long := strings.Repeat("x", MaxTagValueLen+1)
	for _, tc := range []struct {
		name        string
		in          string
		max         int
		want        map[string]string
		wantDropped int
	}{
		{"empty", "", 0, nil, 0},
		{"blank pairs", " , ,", 0, nil, 0},
		{"basic", "team=search,feature=chat", 0, map[string]string{"team": "search", "feature": "chat"}, 0},
		{"spaces trimmed", " team = search , job=nightly ", 0, map[string]string{"team": "search", "job": "nightly"}, 0},
		{"percent decoded utf8", "name=caf%C3%A9%2C+x,path=a%2Fb", 0, map[string]string{"name": "café,+x", "path": "a/b"}, 0},
		{"value may hold equals", "q=a=b", 0, map[string]string{"q": "a=b"}, 0},
		{"key dots dashes underscores", "a.b-c_d=1", 0, map[string]string{"a.b-c_d": "1"}, 0},
		{"uppercase key dropped", "Team=x,ok=1", 0, map[string]string{"ok": "1"}, 1},
		{"key starting with digit dropped", "1a=x,ok=1", 0, map[string]string{"ok": "1"}, 1},
		{"no equals dropped", "novalue,ok=1", 0, map[string]string{"ok": "1"}, 1},
		{"empty value dropped", "a=,ok=1", 0, map[string]string{"ok": "1"}, 1},
		{"bad percent escape dropped", "a=%zz,b=%,ok=1", 0, map[string]string{"ok": "1"}, 2},
		{"invalid utf8 dropped", "a=%FF,ok=1", 0, map[string]string{"ok": "1"}, 1},
		{"control char dropped", "a=x%0Ay,b=%00,ok=1", 0, map[string]string{"ok": "1"}, 2},
		{"value too long dropped", "a=" + long + ",ok=1", 0, map[string]string{"ok": "1"}, 1},
		{"value at limit kept", "a=" + long[:MaxTagValueLen], 0, map[string]string{"a": long[:MaxTagValueLen]}, 0},
		{"key too long dropped", strings.Repeat("k", 65) + "=v,ok=1", 0, map[string]string{"ok": "1"}, 1},
		{"duplicate key first wins", "a=1,a=2", 0, map[string]string{"a": "1"}, 1},
		{"pair cap", "a=1,b=2,c=3", 2, map[string]string{"a": "1", "b": "2"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := TagsDropped()
			got, dropped := ParseTagsHeader(tc.in, tc.max)
			if !reflect.DeepEqual(got, tc.want) || dropped != tc.wantDropped {
				t.Errorf("ParseTagsHeader(%q, %d) = %v, %d; want %v, %d", tc.in, tc.max, got, dropped, tc.want, tc.wantDropped)
			}
			if d := TagsDropped() - before; d != uint64(tc.wantDropped) {
				t.Errorf("counter moved by %d, want %d", d, tc.wantDropped)
			}
		})
	}
}

func TestParseTagsHeader_DefaultCapIs16(t *testing.T) {
	var pairs []string
	for i := 0; i < 20; i++ {
		pairs = append(pairs, "k"+string(rune('a'+i))+"=v")
	}
	got, dropped := ParseTagsHeader(strings.Join(pairs, ","), 0)
	if len(got) != DefaultMaxTagPairs || dropped != 4 {
		t.Errorf("kept %d dropped %d, want %d and 4", len(got), dropped, DefaultMaxTagPairs)
	}
}

func TestParseTagsHeader_OversizeHeaderCutAtLastPair(t *testing.T) {
	var b strings.Builder
	n := 0
	for b.Len() < MaxTagsHeaderLen+500 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString("k" + strings.Repeat("a", n%5) + string(rune('a'+n%26)) + string(rune('a'+(n/26)%26)) + "=" + strings.Repeat("v", 200))
		n++
	}
	_, dropped := ParseTagsHeader(b.String(), 64)
	if dropped < 1 {
		t.Errorf("oversize header dropped %d pairs, want >= 1", dropped)
	}
	// Every pair is accounted for exactly once: kept + dropped == pairs sent.
	total := strings.Count(b.String(), ",") + 1
	kept, dropped2 := ParseTagsHeader(b.String(), 64)
	if len(kept)+dropped2 != total {
		t.Errorf("kept %d + dropped %d != %d pairs sent", len(kept), dropped2, total)
	}
	// A single oversized pair with no comma is dropped whole.
	got, dropped := ParseTagsHeader("k="+strings.Repeat("v", MaxTagsHeaderLen), 0)
	if got != nil || dropped != 1 {
		t.Errorf("single oversize pair: %v, %d", got, dropped)
	}
}

func TestCleanTags(t *testing.T) {
	got, dropped := CleanTags(map[string]string{"team": "a", "Bad": "x", "empty": "", "c": "3", "d": "4"}, 2)
	// Valid candidates in key order: c, d, team; cap 2 keeps c and d; Bad, empty and team dropped.
	if !reflect.DeepEqual(got, map[string]string{"c": "3", "d": "4"}) || dropped != 3 {
		t.Errorf("CleanTags = %v, %d", got, dropped)
	}
	if got, d := CleanTags(nil, 0); got != nil || d != 0 {
		t.Errorf("nil in: %v, %d", got, d)
	}
}

func TestCapUser(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"  alice@example.com\t": "alice@example.com",
		"a\x00b\nc":             "abc",
		"bad\xffutf8":           "badutf8",
	} {
		if got := CapUser(in); got != want {
			t.Errorf("CapUser(%q) = %q, want %q", in, got, want)
		}
	}
	got := CapUser(strings.Repeat("é", 200)) // 400 bytes
	if len(got) > MaxUserLen || strings.ContainsRune(got, '�') || len(got)%2 != 0 {
		t.Errorf("CapUser cut mid-rune or over the cap: %d bytes", len(got))
	}
}
