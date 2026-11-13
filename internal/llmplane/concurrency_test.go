package llmplane

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

func TestMemoryConcurrency_CapAndRelease(t *testing.T) {
	c := newConcurrency(2, nil, 0)
	ctx := context.Background()

	r1, ok, err := c.acquire(ctx, "t1")
	if !ok || err != nil {
		t.Fatalf("first acquire = %v, %v", ok, err)
	}
	r2, ok, _ := c.acquire(ctx, "t1")
	if !ok {
		t.Fatal("second acquire refused under a limit of 2")
	}
	if _, ok, _ := c.acquire(ctx, "t1"); ok {
		t.Fatal("third acquire allowed over a limit of 2")
	}
	// Another tenant has its own bucket.
	if r, ok, _ := c.acquire(ctx, "t2"); !ok {
		t.Fatal("another tenant refused")
	} else {
		r()
	}

	r1()
	r1() // a second call is a no-op, never frees someone else's slot
	r3, ok, _ := c.acquire(ctx, "t1")
	if !ok {
		t.Fatal("acquire refused after a release")
	}
	if _, ok, _ := c.acquire(ctx, "t1"); ok {
		t.Fatal("a double release freed two slots")
	}
	r2()
	r3()
}

func TestNewConcurrency_ZeroLimitIsNoLimit(t *testing.T) {
	if c := newConcurrency(0, nil, 0); c != nil {
		t.Fatalf("limit 0 built %T, want nil (no limit)", c)
	}
}

// captureSlog routes the default logger into a buffer for the test.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// REQ-3: with Redis unreachable the limit falls back to the per-pod count
// (still enforced) and a warning is logged, once.
func TestFallbackConcurrency_RedisDownUsesPerPodLimit(t *testing.T) {
	logs := captureSlog(t)
	// Nothing listens on port 1.
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", ContextTimeoutEnabled: true, MaxRetries: -1})
	defer rdb.Close()
	c := newConcurrency(1, rdb, 3*time.Second)
	ctx := context.Background()

	rel, ok, err := c.acquire(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("acquire with redis down = %v, %v; want the per-pod fallback to allow it", ok, err)
	}
	if _, ok, _ := c.acquire(ctx, "t1"); ok {
		t.Fatal("fallback did not enforce the limit")
	}
	rel()
	if rel2, ok, _ := c.acquire(ctx, "t1"); !ok {
		t.Fatal("fallback slot not freed by release")
	} else {
		rel2()
	}
	if n := strings.Count(logs.String(), "tenant concurrency store (redis) unreachable"); n != 1 {
		t.Fatalf("warnings logged = %d, want 1 (rate-limited); logs:\n%s", n, logs.String())
	}
}

// testRedis returns a client on GATEWAY_TEST_REDIS_ADDR or skips: these
// tests run against a real server, never a fake.
func testRedis(t *testing.T) *goredis.Client {
	t.Helper()
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set; skipping the Redis concurrency tests")
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr, ContextTimeoutEnabled: true})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestRedisConcurrency_CapAcrossInstancesAndRelease(t *testing.T) {
	rdb := testRedis(t)
	tenant := "t-" + uuid.NewString()
	a := newRedisConcurrency(rdb, 2, 3*time.Second)
	b := newRedisConcurrency(rdb, 2, 3*time.Second) // a second pod
	ctx := context.Background()

	r1, ok, err := a.acquire(ctx, tenant)
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	r2, ok, _ := b.acquire(ctx, tenant)
	if !ok {
		t.Fatal("second slot refused")
	}
	if _, ok, _ := a.acquire(ctx, tenant); ok {
		t.Fatal("third slot allowed across two instances with a limit of 2")
	}
	r1()
	r3, ok, _ := a.acquire(ctx, tenant)
	if !ok {
		t.Fatal("slot not freed by release")
	}
	r2()
	r3()
	if n := rdb.ZCard(ctx, concurrencyKeyPrefix+tenant).Val(); n != 0 {
		t.Fatalf("leases left after every release = %d", n)
	}
}

// A pod that dies holding a slot never releases or renews it; the lease
// expires and the slot comes back.
func TestRedisConcurrency_CrashedPodSlotExpires(t *testing.T) {
	rdb := testRedis(t)
	tenant := "t-" + uuid.NewString()
	lease := 3 * time.Second
	ctx := context.Background()
	// Exactly what a crashed pod leaves behind: a taken lease, no renewer.
	if n, err := acquireScript.Run(ctx, rdb, []string{concurrencyKeyPrefix + tenant}, 1, "dead-pod", lease.Milliseconds()).Int(); err != nil || n != 1 {
		t.Fatalf("seed lease = %d, %v", n, err)
	}
	c := newRedisConcurrency(rdb, 1, lease)
	if _, ok, _ := c.acquire(ctx, tenant); ok {
		t.Fatal("slot allowed while the dead pod's lease is live")
	}
	time.Sleep(lease + 500*time.Millisecond)
	rel, ok, err := c.acquire(ctx, tenant)
	if err != nil || !ok {
		t.Fatalf("slot not back after the lease expired: %v, %v", ok, err)
	}
	rel()
}

// A request running longer than the lease keeps its slot by renewing it.
func TestRedisConcurrency_LongRequestKeepsSlot(t *testing.T) {
	rdb := testRedis(t)
	tenant := "t-" + uuid.NewString()
	lease := 3 * time.Second
	c := newRedisConcurrency(rdb, 1, lease)
	ctx := context.Background()

	rel, ok, err := c.acquire(ctx, tenant)
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	time.Sleep(2*lease + 500*time.Millisecond)
	if _, ok, _ := c.acquire(ctx, tenant); ok {
		t.Fatal("a running request lost its slot after the lease time")
	}
	rel()
	rel2, ok, _ := c.acquire(ctx, tenant)
	if !ok {
		t.Fatal("slot not freed by release")
	}
	rel2()
}
