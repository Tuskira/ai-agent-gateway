package clickhouse

import (
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

func TestDedupByRequestID(t *testing.T) {
	records := []*sink.AccessLog{
		{RequestID: "a"},
		{RequestID: "b"},
		{RequestID: "a"}, // duplicate of the first
		{RequestID: "c"},
		{RequestID: "b"}, // duplicate of the second
	}

	kept, dup := dedupByRequestID(records, func(a *sink.AccessLog) string { return a.RequestID })

	if dup != 2 {
		t.Fatalf("duplicates = %d, want 2", dup)
	}
	if len(kept) != 3 {
		t.Fatalf("kept = %d records, want 3", len(kept))
	}
	var ids []string
	for _, k := range kept {
		ids = append(ids, k.RequestID)
	}
	want := []string{"a", "b", "c"}
	for i, id := range want {
		if ids[i] != id {
			t.Errorf("kept[%d] = %q, want %q (first occurrence must win, in order)", i, ids[i], id)
		}
	}
}

func TestDedupByRequestID_Empty(t *testing.T) {
	kept, dup := dedupByRequestID([]*sink.AccessLog{}, func(a *sink.AccessLog) string { return a.RequestID })
	if len(kept) != 0 || dup != 0 {
		t.Fatalf("kept=%d dup=%d, want 0, 0", len(kept), dup)
	}
}

func TestBatchTimeRange(t *testing.T) {
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	records := []*sink.AccessLog{
		{Timestamp: base},
		{Timestamp: base.Add(30 * time.Minute)},
		{Timestamp: base.Add(-10 * time.Minute)},
	}

	from, to := batchTimeRange(records, func(a *sink.AccessLog) time.Time { return a.Timestamp })

	wantFrom := base.Add(-10 * time.Minute).Add(-dupCheckWindow)
	wantTo := base.Add(30 * time.Minute).Add(dupCheckWindow)
	if !from.Equal(wantFrom) {
		t.Errorf("from = %v, want %v", from, wantFrom)
	}
	if !to.Equal(wantTo) {
		t.Errorf("to = %v, want %v", to, wantTo)
	}
}

func TestBatchTimeRange_SingleRecord(t *testing.T) {
	ts := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	from, to := batchTimeRange([]*sink.AccessLog{{Timestamp: ts}}, func(a *sink.AccessLog) time.Time { return a.Timestamp })
	if !from.Equal(ts.Add(-dupCheckWindow)) || !to.Equal(ts.Add(dupCheckWindow)) {
		t.Errorf("from/to = %v/%v, want %v/%v", from, to, ts.Add(-dupCheckWindow), ts.Add(dupCheckWindow))
	}
}
