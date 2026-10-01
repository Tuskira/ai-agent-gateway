package analytics

import (
	"testing"
	"time"
)

func TestSortTimelineEvents_CompositeKeyBothDirections(t *testing.T) {
	t0 := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	mk := func(plane, id string, off time.Duration) TimelineEvent {
		return TimelineEvent{Plane: plane, ID: id, Timestamp: t0.Add(off)}
	}
	// MCP and LLM events interleaved, with equal timestamps across planes.
	in := func() []TimelineEvent {
		return []TimelineEvent{
			mk("llm", "req-3", time.Second),
			mk("mcp", "req-2", 0),
			mk("llm", "req-1", 0),
			mk("mcp", "req-4", time.Second),
			mk("llm", "req-0", -time.Second),
		}
	}
	ids := func(ev []TimelineEvent) string {
		s := ""
		for _, e := range ev {
			s += e.ID[len(e.ID)-1:]
		}
		return s
	}

	asc := in()
	SortTimelineEvents(asc, TimelineOrderAsc)
	if got := ids(asc); got != "01234" {
		t.Errorf("asc = %s, want 01234", got)
	}
	desc := in()
	SortTimelineEvents(desc, TimelineOrderDesc)
	if got := ids(desc); got != "43210" {
		t.Errorf("desc = %s, want 43210", got)
	}

	// Input order must never leak into the result (ties never swap).
	rev := in()
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	SortTimelineEvents(rev, TimelineOrderDesc)
	if ids(rev) != ids(desc) {
		t.Errorf("result depends on input order: %s vs %s", ids(rev), ids(desc))
	}
}

func TestParseTimelineOrder(t *testing.T) {
	for in, want := range map[string]TimelineOrder{"": TimelineOrderAsc, "asc": TimelineOrderAsc, "desc": TimelineOrderDesc} {
		if got, ok := ParseTimelineOrder(in); !ok || got != want {
			t.Errorf("ParseTimelineOrder(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"ASC", "bogus", "up"} {
		if _, ok := ParseTimelineOrder(bad); ok {
			t.Errorf("ParseTimelineOrder(%q) accepted", bad)
		}
	}
}
