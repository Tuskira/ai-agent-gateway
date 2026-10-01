package auth

import (
	"testing"
	"time"
)

func TestKeyRateLimiter_Disabled(t *testing.T) {
	kl := NewKeyRateLimiter(0, nil)
	for i := 0; i < 1000; i++ {
		if allowed, _ := kl.Allow("k1"); !allowed {
			t.Fatalf("disabled limiter refused request %d", i)
		}
	}
}

func TestKeyRateLimiter_NilReceiver(t *testing.T) {
	var kl *KeyRateLimiter
	if allowed, retry := kl.Allow("k1"); !allowed || retry != 0 {
		t.Fatalf("nil *KeyRateLimiter.Allow() = (%v, %v), want (true, 0)", allowed, retry)
	}
}

// TestKeyRateLimiter_PerKeyIndependence proves the "per key, not per IP"
// design goal: two different keys each get their own budget, so one key's
// traffic never throttles another's -- the office-NAT scenario the design
// doc calls out.
func TestKeyRateLimiter_PerKeyIndependence(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	// rate=60/min -> burst = clamp(60/20, 5, 50) = 5.
	kl := NewKeyRateLimiter(60, clock)

	for i := 0; i < 5; i++ {
		if allowed, _ := kl.Allow("key-a"); !allowed {
			t.Fatalf("key-a request %d refused within burst", i)
		}
	}
	if allowed, retry := kl.Allow("key-a"); allowed || retry <= 0 {
		t.Fatalf("key-a 6th request = (%v, %v), want refused with positive retry-after", allowed, retry)
	}

	// key-b's own burst is untouched by key-a's exhaustion.
	for i := 0; i < 5; i++ {
		if allowed, _ := kl.Allow("key-b"); !allowed {
			t.Fatalf("key-b request %d refused; key-a's usage must not affect key-b", i)
		}
	}
}

func TestKeyRateLimiter_RefillOverTime(t *testing.T) {
	clock := &manualClock{t: time.Now()}
	kl := NewKeyRateLimiter(60, clock) // 1 token/sec, burst 5

	for i := 0; i < 5; i++ {
		kl.Allow("k1")
	}
	if allowed, _ := kl.Allow("k1"); allowed {
		t.Fatal("expected bucket exhausted after burst")
	}

	clock.Advance(1500 * time.Millisecond) // ~1.5 tokens refilled
	if allowed, _ := kl.Allow("k1"); !allowed {
		t.Fatal("expected a token to be available after refill")
	}
}
