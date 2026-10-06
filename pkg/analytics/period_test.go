package analytics

import (
	"testing"
	"time"
)

func TestRange_Period(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		r    Range
		span time.Duration
		gran Granularity
	}{
		{Range24h, 24 * time.Hour, GranularityHour},
		{Range7d, 7 * 24 * time.Hour, GranularityDay},
		{Range30d, 30 * 24 * time.Hour, GranularityDay},
	} {
		p := c.r.Period(now.In(time.FixedZone("IST", 5*3600+1800))) // any zone in, UTC out
		if !p.End.Equal(now) || !p.Start.Equal(now.Add(-c.span)) || !p.PrevEnd.Equal(p.Start) || !p.PrevStart.Equal(now.Add(-2*c.span)) {
			t.Errorf("%s period = %+v, want [now-%v, now) and the same span before it", c.r, p, c.span)
		}
		if p.Granularity != c.gran || p.Start.Location() != time.UTC {
			t.Errorf("%s granularity/zone = %q/%v, want %q/UTC", c.r, p.Granularity, p.Start.Location(), c.gran)
		}
	}
}

func TestParseDateRange(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC)
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

	// Whole UTC days, both inclusive; compared with the same number of days before.
	p, err := ParseDateRange("2026-09-01", "2026-09-10", now)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Start.Equal(day(9, 1)) || !p.End.Equal(day(9, 11)) || !p.PrevStart.Equal(day(8, 22)) || !p.PrevEnd.Equal(day(9, 1)) || p.Granularity != GranularityDay {
		t.Errorf("Sep 1-10 = %+v, want [Sep 1, Sep 11) vs [Aug 22, Sep 1), daily", p)
	}

	// A single day is hourly; a range ending today stops at now.
	p, err = ParseDateRange("2026-10-05", "2026-10-05", now)
	if err != nil || !p.Start.Equal(day(10, 5)) || !p.End.Equal(now) || p.Granularity != GranularityHour {
		t.Errorf("today only = %+v, %v; want [Oct 5 00:00, now), hourly", p, err)
	}
	if !p.PrevStart.Equal(day(10, 5).Add(-now.Sub(day(10, 5)))) {
		t.Errorf("today only prev start = %v, want the same %v before", p.PrevStart, now.Sub(day(10, 5)))
	}

	for _, c := range [][2]string{
		{"2026-9-1", "2026-09-10"},   // not YYYY-MM-DD
		{"2026-09-10", "2026-09-01"}, // ends before it starts
		{"2026-10-06", "2026-10-07"}, // starts in the future
		{"2025-01-01", "2026-10-01"}, // more than 366 days
		{"", "2026-10-01"},           // half a range
	} {
		if _, err := ParseDateRange(c[0], c[1], now); err == nil {
			t.Errorf("ParseDateRange(%q, %q) = nil error, want a validation error", c[0], c[1])
		}
	}
}
