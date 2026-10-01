package redis_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/redis"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/sessiontest"
)

// redisAddr returns GATEWAY_TEST_REDIS_ADDR or skips: these tests run
// against a real server, never a fake.
func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set; skipping the Redis session driver tests")
	}
	return addr
}

// openPair opens the driver through the registry, exactly as the gateway
// does, and closes both halves at the end of the test.
func openPair(t *testing.T) (session.Store, session.Notifier) {
	t.Helper()
	st, n, err := session.Open(context.Background(), redis.Driver, session.Config{Addr: redisAddr(t)})
	if err != nil {
		t.Fatalf("Open(%q): %v", redis.Driver, err)
	}
	if n == nil {
		t.Fatal("the redis driver returned no Notifier")
	}
	return st, n
}

func TestConformance(t *testing.T) {
	sessiontest.Run(t, func(t *testing.T) session.Store {
		st, n := openPair(t)
		t.Cleanup(func() { _ = n.Close() })
		return st
	})
}

func TestNotifierConformance(t *testing.T) {
	sessiontest.RunNotifier(t, func(t *testing.T) session.Notifier {
		st, n := openPair(t)
		t.Cleanup(func() { _ = st.Close() })
		return n
	})
}

func TestNewRejectsAnUnreachableAddress(t *testing.T) {
	redisAddr(t) // the test still needs Redis in principle; keep it env-gated like the rest
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := redis.New(ctx, session.Config{Addr: "127.0.0.1:1"}); err == nil {
		t.Fatal("New(unreachable) = nil, want a ping error")
	}
	if _, _, err := redis.New(ctx, session.Config{}); err == nil {
		t.Fatal("New(no addr) = nil, want an error")
	}
}

// The key's TTL is the storage-side enforcement of Record.ExpiresAt: it
// must be set, must not outlive the record, and must vanish with it.
func TestKeyTTLTracksExpiresAt(t *testing.T) {
	addr := redisAddr(t)
	st, n := openPair(t)
	t.Cleanup(func() { _ = st.Close(); _ = n.Close() })

	raw := goredis.NewClient(&goredis.Options{Addr: addr})
	t.Cleanup(func() { _ = raw.Close() })
	ctx := context.Background()

	r := &session.Record{
		ID: "ttl-test-" + t.Name(), TenantID: "t", ExpiresAt: time.Now().Add(time.Second),
		Backends: map[string]session.Backend{"c": {SessionID: "b", ProtocolVersion: mcp.ProtocolVersion}},
	}
	if err := st.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Delete(context.Background(), r.ID) })

	ttl, err := raw.PTTL(ctx, redis.KeyPrefix+r.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > time.Second {
		t.Fatalf("key TTL = %v, want (0, 1s]", ttl)
	}

	time.Sleep(1300 * time.Millisecond)
	exists, err := raw.Exists(ctx, redis.KeyPrefix+r.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("session key still present after its TTL")
	}
	if _, err := st.Get(ctx, r.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get after TTL = %v, want ErrNotFound", err)
	}
}

// Closing one half of the pair must leave the other usable; closing both
// releases the pool.
func TestStoreAndNotifierShareOneConnection(t *testing.T) {
	st, n := openPair(t)
	ctx := context.Background()

	if err := n.Close(); err != nil {
		t.Fatalf("Notifier.Close: %v", err)
	}
	r := &session.Record{ID: "shared-" + t.Name(), TenantID: "t", ExpiresAt: time.Now().Add(time.Minute)}
	if err := st.Save(ctx, r); err != nil {
		t.Fatalf("Save after the Notifier was closed: %v", err)
	}
	if err := st.Delete(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Store.Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Store.Close = %v, want nil", err)
	}
	if err := st.Save(ctx, r); err == nil {
		t.Fatal("Save after both halves were closed succeeded; the pool was not released")
	}
}
