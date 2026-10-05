package analytics

import (
	"testing"
	"time"
)

func TestParseMonitoringWindow(t *testing.T) {
	for _, s := range []string{"today", "7d", "30d"} {
		if w, ok := ParseMonitoringWindow(s); !ok || string(w) != s {
			t.Errorf("ParseMonitoringWindow(%q) = %q, %v; want %q, true", s, w, ok, s)
		}
	}
	for _, s := range []string{"", "24h", "1d", "TODAY", "90d"} {
		if _, ok := ParseMonitoringWindow(s); ok {
			t.Errorf("ParseMonitoringWindow(%q) ok = true, want false", s)
		}
	}
}

func TestMonitoringWindow_Bounds(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC)
	midnight := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		w                         MonitoringWindow
		start, prevStart, prevEnd time.Time
		gran                      Granularity
	}{
		// Today is the UTC calendar day so far, compared with the same
		// hours of yesterday (not the tail of yesterday).
		{WindowToday, midnight, midnight.Add(-24 * time.Hour), now.Add(-24 * time.Hour), GranularityHour},
		{Window7d, now.Add(-7 * 24 * time.Hour), now.Add(-14 * 24 * time.Hour), now.Add(-7 * 24 * time.Hour), GranularityDay},
		{Window30d, now.Add(-30 * 24 * time.Hour), now.Add(-60 * 24 * time.Hour), now.Add(-30 * 24 * time.Hour), GranularityDay},
	}
	for _, c := range cases {
		b := c.w.Bounds(now.In(time.FixedZone("IST", 5*3600+1800))) // any zone in, UTC out
		if !b.Start.Equal(c.start) || !b.End.Equal(now) || !b.PrevStart.Equal(c.prevStart) || !b.PrevEnd.Equal(c.prevEnd) {
			t.Errorf("%s bounds = %+v, want start %v end %v prev [%v, %v)", c.w, b, c.start, now, c.prevStart, c.prevEnd)
		}
		if b.Start.Location() != time.UTC {
			t.Errorf("%s bounds not UTC: %v", c.w, b.Start.Location())
		}
		if b.Granularity != c.gran {
			t.Errorf("%s granularity = %q, want %q", c.w, b.Granularity, c.gran)
		}
	}
}

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
	}, MonitoringBounds{Start: start, End: end, Granularity: GranularityHour})
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
	days := FillTokenBuckets(nil, MonitoringBounds{Start: dStart, End: dStart.Add(7 * 24 * time.Hour), Granularity: GranularityDay})
	if len(days) != 8 || !days[0].Bucket.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day buckets = %d starting %v, want 8 starting 2026-09-28", len(days), days[0].Bucket)
	}
}
