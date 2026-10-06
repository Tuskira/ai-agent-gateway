package analytics

import (
	"testing"
	"time"
)

func TestDeltaPct(t *testing.T) {
	if d := DeltaPct(150, 100); d == nil || *d != 50 {
		t.Errorf("DeltaPct(150,100) = %v, want 50", d)
	}
	if d := DeltaPct(50, 100); d == nil || *d != -50 {
		t.Errorf("DeltaPct(50,100) = %v, want -50", d)
	}
	if d := DeltaPct(1, 3); d == nil || *d != -66.67 {
		t.Errorf("DeltaPct(1,3) = %v, want -66.67 (2 decimals)", d)
	}
	// No previous usage: there is no meaningful percentage ("new"), not 0%.
	if d := DeltaPct(10, 0); d != nil {
		t.Errorf("DeltaPct(10,0) = %v, want nil", *d)
	}
	if d := DeltaPct(0, 0); d != nil {
		t.Errorf("DeltaPct(0,0) = %v, want nil", *d)
	}
}

func TestFillTokenBuckets(t *testing.T) {
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(3*time.Hour + 20*time.Minute)
	got := FillTokenBuckets([]TokenBucket{
		{Bucket: start.Add(time.Hour), Tokens: 7},
		{Bucket: start.Add(3 * time.Hour), Tokens: 2},
	}, Period{Start: start, End: end, Granularity: GranularityHour})
	want := []uint64{0, 7, 0, 2}
	if len(got) != len(want) {
		t.Fatalf("FillTokenBuckets len = %d (%+v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if !got[i].Bucket.Equal(start.Add(time.Duration(i)*time.Hour)) || got[i].Tokens != w {
			t.Errorf("bucket %d = %+v, want %v/%d", i, got[i], start.Add(time.Duration(i)*time.Hour), w)
		}
	}

	// Day buckets over a rolling window start at the window's first UTC day.
	dStart := time.Date(2026, 9, 28, 14, 30, 0, 0, time.UTC)
	days := FillTokenBuckets(nil, Period{Start: dStart, End: dStart.Add(7 * 24 * time.Hour), Granularity: GranularityDay})
	if len(days) != 8 || !days[0].Bucket.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day buckets = %d starting %v, want 8 starting 2026-09-28", len(days), days[0].Bucket)
	}

	// A custom window ending at midnight stops at its last day: End is exclusive.
	week := FillTokenBuckets(nil, Period{Start: start, End: start.Add(7 * 24 * time.Hour), Granularity: GranularityDay})
	if len(week) != 7 {
		t.Errorf("aligned week = %d buckets, want 7", len(week))
	}
}
