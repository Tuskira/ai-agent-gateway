package redis_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/redis"
)

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// forward accepts on addr and copies every connection to target until the
// test ends: a Redis that "starts" when forward is called.
func forward(t *testing.T, addr, target string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
}

// Redis starting alongside the gateway: New keeps retrying and succeeds
// once the server is reachable.
func TestNewWaitsForRedisToStart(t *testing.T) {
	target := redisAddr(t)
	addr := freeAddr(t)

	type result struct {
		st      session.Store
		n       session.Notifier
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	go func() {
		st, n, err := redis.New(ctx, session.Config{Addr: addr})
		done <- result{st, n, err, time.Since(start)}
	}()

	time.Sleep(1500 * time.Millisecond)
	forward(t, addr, target)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("New after Redis came up: %v", r.err)
		}
		_ = r.st.Close()
		_ = r.n.Close()
		if r.elapsed < time.Second {
			t.Fatalf("New returned after %v, before Redis was reachable", r.elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("New did not return after Redis came up")
	}
}

// Redis never comes up: New gives up when ctx is done, wrapping the last
// ping error under the usual prefix. Needs no Redis.
func TestNewGivesUpWhenCtxIsDone(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := redis.New(ctx, session.Config{Addr: addr})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("New against a closed port = nil, want an error")
	}
	if elapsed < time.Second || elapsed > 5*time.Second {
		t.Fatalf("New returned after %v, want about the 2s ctx budget", elapsed)
	}
	for _, want := range []string{"redis: ping " + addr + ":", "gave up after"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// An error reply from Redis is a config mistake: New must fail at once,
// not burn the ctx budget retrying. A DB index past the server's range
// is answered with -ERR on every Redis; a wrong password is not a
// reliable trigger, since a Redis with no requirepass accepts any
// password sent with HELLO.
func TestNewFailsFastOnErrorReply(t *testing.T) {
	addr := redisAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := redis.New(ctx, session.Config{Addr: addr, Password: "s3cret-bad", DB: 99})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("New with a bad DB index = nil, want an error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("New took %v on an error reply, want a fast failure", elapsed)
	}
	if !strings.Contains(err.Error(), "redis: ping "+addr+":") {
		t.Fatalf("error %q lacks the redis: ping prefix", err)
	}
	if strings.Contains(err.Error(), "s3cret-bad") {
		t.Fatalf("error leaks the password: %v", err)
	}
}
