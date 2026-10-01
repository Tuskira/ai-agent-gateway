package llmplane

import (
	"testing"
	"time"
)

// Pure-logic tests for limits.go; the Postgres-backed enforcement tests
// (spend query, 429/400 paths, capture of denials) are in
// limits_pg_test.go.

func TestUTCPeriods(t *testing.T) {
	for _, tc := range []struct {
		now                                      string
		dayStart, nextDay, monthStart, nextMonth string
	}{
		{"2026-09-27T15:04:05Z", "2026-09-27T00:00:00Z", "2026-09-28T00:00:00Z", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"2026-12-31T23:59:59Z", "2026-12-31T00:00:00Z", "2027-01-01T00:00:00Z", "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		// A non-UTC wall clock still uses UTC boundaries: 2026-03-01 01:00+05:00 is Feb 28 UTC.
		{"2026-03-01T01:00:00+05:00", "2026-02-28T00:00:00Z", "2026-03-01T00:00:00Z", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		ds, nd, ms, nm := utcPeriods(now)
		for name, pair := range map[string][2]any{
			"dayStart": {ds, tc.dayStart}, "nextDay": {nd, tc.nextDay}, "monthStart": {ms, tc.monthStart}, "nextMonth": {nm, tc.nextMonth},
		} {
			if got := pair[0].(time.Time).Format(time.RFC3339); got != pair[1] {
				t.Errorf("utcPeriods(%s).%s = %s, want %s", tc.now, name, got, pair[1])
			}
		}
	}
}

func TestSecondsUntil(t *testing.T) {
	now := time.Date(2026, 9, 27, 23, 59, 58, 500_000_000, time.UTC)
	if got := secondsUntil(now, time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)); got != 2 {
		t.Errorf("1.5s to boundary: got %d, want 2 (rounded up)", got)
	}
	if got := secondsUntil(now, now); got != 1 {
		t.Errorf("at boundary: got %d, want the floor of 1", got)
	}
}

func TestAdmitRPM_ExactSlidingWindow(t *testing.T) {
	e := &limitEntry{}
	t0 := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for i, at := range []time.Duration{0, 10 * time.Second, 20 * time.Second} {
		if !e.admitRPM(3, t0.Add(at)) {
			t.Fatalf("request %d should be admitted (window not full)", i)
		}
	}
	if e.admitRPM(3, t0.Add(59*time.Second)) {
		t.Fatal("4th request within the minute admitted")
	}
	// The first slot (t0) frees at exactly t0+60s; the second not until t0+70s.
	if !e.admitRPM(3, t0.Add(60*time.Second)) {
		t.Fatal("request once the oldest admission aged out was denied")
	}
	if e.admitRPM(3, t0.Add(65*time.Second)) {
		t.Fatal("request before the next slot frees was admitted")
	}
	if !e.admitRPM(3, t0.Add(70*time.Second)) {
		t.Fatal("request after the second slot freed was denied")
	}
	// A changed rpm restarts the window.
	if !e.admitRPM(1, t0.Add(71*time.Second)) || e.admitRPM(1, t0.Add(72*time.Second)) {
		t.Fatal("rpm=1 after a change: want admit then deny")
	}
	if (&limitEntry{}).admitRPM(0, t0) {
		t.Fatal("rpm=0 must block")
	}
}

func TestRequestedMaxTokens(t *testing.T) {
	for body, want := range map[string]float64{
		`{"model":"m","max_tokens":1024}`:                          1024,
		`{"max_completion_tokens":300}`:                            300,
		`{"max_output_tokens":77}`:                                 77,
		`{"generationConfig":{"maxOutputTokens":2048}}`:            2048,
		`{"inferenceConfig":{"maxTokens":512}}`:                    512,
		`{"max_tokens":10,"max_completion_tokens":9000}`:           9000, // the largest ask wins
		`{"max_tokens":1e4}`:                                       10000,
		`{"model":"m","messages":[],"stream":true,"max_tokens":1}`: 1,
	} {
		got, ok := requestedMaxTokens([]byte(body))
		if !ok || got != want {
			t.Errorf("requestedMaxTokens(%s) = %v, %v; want %v, true", body, got, ok, want)
		}
	}
	for _, body := range []string{`{"model":"m"}`, `not json`, ``, `{"inferenceConfig":{}}`} {
		if _, ok := requestedMaxTokens([]byte(body)); ok {
			t.Errorf("requestedMaxTokens(%q) reported a cap", body)
		}
	}
}
