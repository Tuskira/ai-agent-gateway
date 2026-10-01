package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// manualClock is an injectable Clock for deterministic window/lockout/
// eviction tests.
type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time          { return c.t }
func (c *manualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRateLimiter(t *testing.T, clock Clock, overrides func(*RateLimiterConfig)) *RateLimiter {
	t.Helper()
	cfg := RateLimiterConfig{
		Enabled:     true,
		MaxFailures: 3,
		Window:      time.Minute,
		Lockout:     5 * time.Minute,
		Clock:       clock,
		Logger:      testLogger(),
	}
	if overrides != nil {
		overrides(&cfg)
	}
	rl := NewRateLimiter(cfg)
	t.Cleanup(rl.Close)
	return rl
}

func TestRateLimiter_LockoutAfterMaxFailures(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, nil)

	for i := 0; i < 2; i++ {
		rl.ReportFailure("1.2.3.4")
	}
	if locked, _ := rl.Locked("1.2.3.4"); locked {
		t.Fatal("locked after 2 failures, want not locked (MaxFailures=3)")
	}

	rl.ReportFailure("1.2.3.4") // 3rd failure trips the lockout
	locked, retryAfter := rl.Locked("1.2.3.4")
	if !locked {
		t.Fatal("not locked after 3 failures, want locked")
	}
	if retryAfter <= 0 || retryAfter > 5*time.Minute {
		t.Errorf("retryAfter = %v, want (0, 5m]", retryAfter)
	}

	// A different IP must be unaffected.
	if locked, _ := rl.Locked("5.6.7.8"); locked {
		t.Error("unrelated IP locked out")
	}
}

func TestRateLimiter_LockoutExpires(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, nil)

	for i := 0; i < 3; i++ {
		rl.ReportFailure("1.2.3.4")
	}
	if locked, _ := rl.Locked("1.2.3.4"); !locked {
		t.Fatal("expected locked")
	}

	clock.Advance(5*time.Minute + time.Second)
	if locked, _ := rl.Locked("1.2.3.4"); locked {
		t.Error("still locked after Lockout elapsed")
	}
}

func TestRateLimiter_WindowResets(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, nil) // MaxFailures=3, Window=1m

	rl.ReportFailure("1.2.3.4")
	rl.ReportFailure("1.2.3.4")

	clock.Advance(time.Minute + time.Second) // outside the window

	rl.ReportFailure("1.2.3.4") // should start a fresh window, count=1
	if locked, _ := rl.Locked("1.2.3.4"); locked {
		t.Error("locked out after window reset with only 1 failure in the new window")
	}
}

func TestRateLimiter_SuccessResetsCounter(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, nil) // MaxFailures=3

	rl.ReportFailure("1.2.3.4")
	rl.ReportFailure("1.2.3.4")
	rl.ReportSuccess("1.2.3.4")

	rl.ReportFailure("1.2.3.4")
	rl.ReportFailure("1.2.3.4")
	if locked, _ := rl.Locked("1.2.3.4"); locked {
		t.Error("locked out despite a success resetting the counter in between")
	}
}

func TestRateLimiter_Disabled_NeverLocksOrCaps(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.Enabled = false
		c.RequestsPerMinute = 1
	})

	for i := 0; i < 10; i++ {
		rl.ReportFailure("1.2.3.4")
	}
	if locked, _ := rl.Locked("1.2.3.4"); locked {
		t.Error("disabled limiter locked out an IP")
	}
	for i := 0; i < 10; i++ {
		if ok, _ := rl.AllowGeneral("1.2.3.4"); !ok {
			t.Error("disabled limiter rejected a request via the general cap")
		}
	}
}

func TestRateLimiter_GeneralCap(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.RequestsPerMinute = 60 // 1 token/sec; burst clamped to minBurst=5
	})

	for i := 0; i < 5; i++ {
		if ok, _ := rl.AllowGeneral("9.9.9.9"); !ok {
			t.Fatalf("request %d rejected within burst", i+1)
		}
	}
	ok, retryAfter := rl.AllowGeneral("9.9.9.9")
	if ok {
		t.Fatal("request beyond burst allowed, want rejected")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v, want > 0", retryAfter)
	}

	clock.Advance(time.Second) // refills ~1 token at this rate
	if ok, _ := rl.AllowGeneral("9.9.9.9"); !ok {
		t.Error("request rejected after refill, want allowed")
	}
}

func TestRateLimiter_GeneralCap_ZeroDisables(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.RequestsPerMinute = 0
	})

	for i := 0; i < 1000; i++ {
		if ok, _ := rl.AllowGeneral("9.9.9.9"); !ok {
			t.Fatalf("request %d rejected with RequestsPerMinute=0 (disabled)", i+1)
		}
	}
}

func TestRateLimiter_Eviction(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.IdleTTL = time.Minute
	})

	rl.ReportFailure("1.2.3.4")
	rl.mu.Lock()
	n := len(rl.ips)
	rl.mu.Unlock()
	if n != 1 {
		t.Fatalf("tracked IPs = %d, want 1", n)
	}

	clock.Advance(2 * time.Minute)
	rl.evictIdle()

	rl.mu.Lock()
	n = len(rl.ips)
	rl.mu.Unlock()
	if n != 0 {
		t.Errorf("tracked IPs after idle eviction = %d, want 0", n)
	}
}

func TestRateLimiter_Eviction_SkipsActiveLockout(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.IdleTTL = time.Minute
		c.Lockout = time.Hour
	})

	for i := 0; i < 3; i++ {
		rl.ReportFailure("1.2.3.4")
	}

	clock.Advance(2 * time.Minute) // past IdleTTL, but the lockout (1h) is still active
	rl.evictIdle()

	if locked, _ := rl.Locked("1.2.3.4"); !locked {
		t.Error("active lockout was evicted despite still being locked")
	}
}

func TestRateLimiter_MaxEntriesEvictsOldest(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.MaxEntries = 5
	})

	for i := 0; i < 5; i++ {
		rl.ReportFailure(string(rune('a' + i)))
		clock.Advance(time.Second)
	}
	rl.mu.Lock()
	n := len(rl.ips)
	rl.mu.Unlock()
	if n != 5 {
		t.Fatalf("tracked IPs = %d, want 5", n)
	}

	// One more distinct IP should trigger eviction of the oldest rather
	// than growing past MaxEntries.
	rl.ReportFailure("newcomer")
	rl.mu.Lock()
	n = len(rl.ips)
	_, oldestStillPresent := rl.ips["a"]
	rl.mu.Unlock()
	if n > 5 {
		t.Errorf("tracked IPs = %d, want <= 5 (MaxEntries)", n)
	}
	if oldestStillPresent {
		t.Error("oldest entry (\"a\") was not evicted to make room")
	}
}

func TestRateLimiter_ClientIP_UntrustedPeerIgnoresXFF(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.TrustedProxies = []string{"10.0.0.1"}
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:54321" // not a trusted proxy
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 10.0.0.1")

	if got, want := rl.ClientIP(r), "203.0.113.9"; got != want {
		t.Errorf("ClientIP() = %q, want %q (XFF must be ignored from an untrusted peer)", got, want)
	}
}

func TestRateLimiter_ClientIP_TrustedPeerHonorsXFF(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.TrustedProxies = []string{"10.0.0.1"}
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:54321" // trusted proxy
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 10.0.0.1")

	if got, want := rl.ClientIP(r), "1.1.1.1"; got != want {
		t.Errorf("ClientIP() = %q, want %q (first XFF entry from a trusted peer)", got, want)
	}
}

func TestRateLimiter_ClientIP_NoPortFallsBackToRawAddr(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9" // no port -- SplitHostPort fails

	if got, want := rl.ClientIP(r), "203.0.113.9"; got != want {
		t.Errorf("ClientIP() = %q, want %q", got, want)
	}
}
