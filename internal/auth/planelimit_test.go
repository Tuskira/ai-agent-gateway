package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/clientip"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

func TestRateLimiter_ClientIP_CIDRAndRightmost(t *testing.T) {
	rl := newTestRateLimiter(t, &manualClock{t: time.Now()}, func(c *RateLimiterConfig) {
		c.TrustedProxies = []string{"10.0.0.0/8"}
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.4.5.6:1234"
	// The client spoofs the left-most entry; the proxy appended the real one.
	r.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.50, 10.9.9.9")
	if got, want := rl.ClientIP(r), "203.0.113.50"; got != want {
		t.Errorf("ClientIP() = %q, want %q", got, want)
	}
}

func TestRateLimiter_SharedResolverWins(t *testing.T) {
	rl := newTestRateLimiter(t, &manualClock{t: time.Now()}, func(c *RateLimiterConfig) {
		c.Resolver = clientip.MustNew("127.0.0.0/8")
		c.TrustedProxies = []string{"192.0.2.0/24"} // ignored: Resolver is set
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := rl.ClientIP(r); got != "198.51.100.7" {
		t.Errorf("ClientIP() = %q, want 198.51.100.7", got)
	}
}

func TestRateLimiter_SpoofedXFFCannotEvadeOrFrame(t *testing.T) {
	rl := newTestRateLimiter(t, &manualClock{t: time.Now()}, func(c *RateLimiterConfig) {
		c.TrustedProxies = []string{"127.0.0.0/8"}
		c.MaxFailures = 5
	})
	attacker := RateLimitedAuthenticator(&fakeAuthenticator{name: "f", err: pkgauth.ErrInvalid}, rl)

	// Attacker rotates the left-most XFF each time; the proxy always
	// appends the attacker's real address 203.0.113.50.
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "127.0.0.1:9"
		r.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('1'+i))+", 203.0.113.50")
		_, _ = attacker.Authenticate(r.Context(), r)
	}
	if locked, _ := rl.Locked("203.0.113.50"); !locked {
		t.Error("rotating the left-most XFF evaded the lockout")
	}
	// The addresses the attacker claimed are innocent and untouched.
	for i := 0; i < 5; i++ {
		if locked, _ := rl.Locked("198.51.100." + string(rune('1'+i))); locked {
			t.Errorf("spoofed victim 198.51.100.%d was framed", i+1)
		}
	}
}

func TestRateLimiter_CredentialLockout(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.CredMaxFailures = 4
		c.CredWindow = time.Minute
	})

	for i := 0; i < 3; i++ {
		rl.ReportCredFailure("gk_AAAAAAAA")
	}
	if locked, _ := rl.CredLocked("gk_AAAAAAAA"); locked {
		t.Fatal("prefix locked after 3 failures, want 4")
	}
	rl.ReportCredFailure("gk_AAAAAAAA")
	locked, retry := rl.CredLocked("gk_AAAAAAAA")
	if !locked || retry <= 0 {
		t.Fatalf("CredLocked = %v, %v after 4 failures", locked, retry)
	}
	if other, _ := rl.CredLocked("gk_BBBBBBBB"); other {
		t.Error("another prefix was locked")
	}
	// The per-IP dimension is independent: nothing was reported for any IP.
	if rl.LockedIPCount() != 0 {
		t.Error("credential lockout locked an IP")
	}

	clock.Advance(6 * time.Minute) // lockout (5m) over
	if locked, _ := rl.CredLocked("gk_AAAAAAAA"); locked {
		t.Error("credential lock did not expire")
	}
}

func TestRateLimiter_CredentialWindowResets(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
		c.CredMaxFailures = 3
		c.CredWindow = time.Minute
	})
	rl.ReportCredFailure("p")
	rl.ReportCredFailure("p")
	clock.Advance(2 * time.Minute)
	rl.ReportCredFailure("p")
	if locked, _ := rl.CredLocked("p"); locked {
		t.Error("failures across windows were accumulated")
	}
}

func TestRateLimiter_CredentialStateBoundedAndEvicted(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	rl := newTestRateLimiter(t, clock, func(c *RateLimiterConfig) { c.MaxEntries = 20 })
	for i := 0; i < 200; i++ {
		rl.ReportCredFailure("gk_" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+i/26)))
		clock.Advance(time.Second)
	}
	rl.mu.Lock()
	n := len(rl.creds)
	rl.mu.Unlock()
	if n > 20 {
		t.Errorf("tracked credential prefixes = %d, want <= 20", n)
	}
	clock.Advance(time.Hour)
	rl.evictIdle()
	rl.mu.Lock()
	n = len(rl.creds)
	rl.mu.Unlock()
	if n != 0 {
		t.Errorf("idle credential entries not swept: %d left", n)
	}
}

func TestPlaneAuthenticator_RefusesLockedIPBeforeLookup(t *testing.T) {
	rl := newTestRateLimiter(t, &manualClock{t: time.Now()}, nil)
	inner := &fakeAuthenticator{name: "f", principal: &pkgauth.Principal{Subject: "ok"}}
	a := PlaneRateLimitedAuthenticator(inner, rl)

	for i := 0; i < 3; i++ {
		rl.ReportFailure("203.0.113.5")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:1"
	_, err := a.Authenticate(r.Context(), r)
	var rle *pkgauth.RateLimitedError
	if !errors.As(err, &rle) || !errors.Is(err, pkgauth.ErrRateLimited) || rle.RetryAfter <= 0 {
		t.Fatalf("err = %v, want RateLimitedError with RetryAfter", err)
	}
	if inner.calls != 0 {
		t.Errorf("locked IP still reached the credential lookup (%d calls)", inner.calls)
	}

	// A different IP is unaffected by that IP's failures.
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "198.51.100.9:1"
	if _, err := a.Authenticate(r2.Context(), r2); err != nil {
		t.Errorf("good request from another IP failed: %v", err)
	}
}

func TestPlaneAuthenticator_FailuresLockTheIP(t *testing.T) {
	rl := newTestRateLimiter(t, &manualClock{t: time.Now()}, nil)
	a := PlaneRateLimitedAuthenticator(&fakeAuthenticator{name: "f", err: pkgauth.ErrInvalid}, rl)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:1"
	for i := 0; i < 3; i++ {
		if _, err := a.Authenticate(r.Context(), r); !errors.Is(err, pkgauth.ErrInvalid) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalid", i, err)
		}
	}
	if _, err := a.Authenticate(r.Context(), r); !errors.Is(err, pkgauth.ErrRateLimited) {
		t.Fatalf("4th attempt err = %v, want ErrRateLimited", err)
	}
}

func TestRateLimitedAuthenticator_CredentialLockDoesNotCountAgainstIP(t *testing.T) {
	rl := newTestRateLimiter(t, &manualClock{t: time.Now()}, nil)
	a := RateLimitedAuthenticator(&fakeAuthenticator{name: "f", err: &pkgauth.RateLimitedError{RetryAfter: time.Minute}}, rl)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:1"
	for i := 0; i < 10; i++ {
		_, _ = a.Authenticate(r.Context(), r)
	}
	if locked, _ := rl.Locked("203.0.113.5"); locked {
		t.Error("a credential lockout was charged to the caller's IP")
	}
}

func TestRateLimitedAuthenticator_DisabledIsPassthrough(t *testing.T) {
	inner := &fakeAuthenticator{name: "f"}
	if got := PlaneRateLimitedAuthenticator(inner, nil); got != pkgauth.Authenticator(inner) {
		t.Error("nil limiter should return inner unwrapped")
	}
}

func TestMiddleware_RateLimitedError429(t *testing.T) {
	a := &fakeAuthenticator{name: "f", err: &pkgauth.RateLimitedError{RetryAfter: 90 * time.Second}}
	h := Middleware(a, nil)(okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "90" {
		t.Errorf("Retry-After = %q, want 90", got)
	}
	if body := decodeErrorBody(t, w); body.Error.Type != "rate_limited" {
		t.Errorf("error type = %q", body.Error.Type)
	}
}

func TestMiddlewareWith_CustomResponder(t *testing.T) {
	a := &fakeAuthenticator{name: "f", err: &pkgauth.RateLimitedError{RetryAfter: time.Second}}
	var got time.Duration
	h := MiddlewareWith(a, nil, func(w http.ResponseWriter, _ *http.Request, retry time.Duration) {
		got = retry
		SetRetryAfter(w, retry)
		w.WriteHeader(http.StatusTooManyRequests)
	})(okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTooManyRequests || got != time.Second || w.Header().Get("Retry-After") != "1" {
		t.Errorf("code=%d retry=%v header=%q", w.Code, got, w.Header().Get("Retry-After"))
	}
}
