package llmplane_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// allRec keeps every captured LLMCall.
type allRec struct {
	mu    sync.Mutex
	calls []*pkgsink.LLMCall
}

func (a *allRec) Record(_ context.Context, l *pkgsink.LLMCall) error {
	a.mu.Lock()
	a.calls = append(a.calls, l)
	a.mu.Unlock()
	return nil
}
func (a *allRec) refused() []*pkgsink.LLMCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*pkgsink.LLMCall
	for _, c := range a.calls {
		if c.StatusCode == http.StatusTooManyRequests {
			out = append(out, c)
		}
	}
	return out
}

// holdingUpstream answers every call with a stream that stays open until
// release is closed, counting the calls it got.
type holdingUpstream struct {
	srv     *httptest.Server
	hits    atomic.Int32
	release chan struct{}
	once    sync.Once
}

func newHoldingUpstream(t *testing.T) *holdingUpstream {
	t.Helper()
	h := &holdingUpstream{release: make(chan struct{})}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		h.free()
		h.srv.Close()
	})
	return h
}

// free ends every held stream; safe to call more than once. Tests call it
// in a cleanup registered after their plane, so a failed assertion frees
// the streams before the plane's server waits for them to close.
func (h *holdingUpstream) free() {
	h.once.Do(func() { close(h.release) })
}

func concPlane(t *testing.T, upstream string, limit int, rdb goredis.UniversalClient, rec llmplane.Recorder, tenant string) *httptest.Server {
	t.Helper()
	core, err := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL:        upstream,
		MaxRequestBytes:        1 << 20,
		MaxConcurrentPerTenant: limit,
		ConcurrencyRedis:       rdb,
		ConcurrencyLeaseTTL:    3 * time.Second,
		Authorizer:             pkgauth.NewRoleAuthorizer(),
	}, rec)
	if err != nil {
		t.Fatalf("build llm plane: %v", err)
	}
	srv := httptest.NewServer(principalMW(core, &pkgauth.Principal{
		Subject: "user@acme", TenantID: tenant, KeyID: "key-abc", Roles: []string{"agent"},
	}))
	t.Cleanup(srv.Close)
	return srv
}

// openStream starts a streamed call and returns its status once headers
// arrive; the body is drained in the background until the stream ends.
func openStream(t *testing.T, base string) int {
	t.Helper()
	body := `{"model":"claude-haiku-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("request: %v", err)
		return 0
	}
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	return resp.StatusCode
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// AC-2 / REQ-2: a request refused by the per-tenant concurrency limit is
// captured as an llm_calls row (429, tenant set, no upstream call).
func TestTenantConcurrency_RefusalIsRecorded(t *testing.T) {
	up := newHoldingUpstream(t)
	rec := &allRec{}
	srv := concPlane(t, up.srv.URL, 2, nil, rec, "tenant-x")
	t.Cleanup(up.free)

	for i := 0; i < 2; i++ {
		if code := openStream(t, srv.URL); code != http.StatusOK {
			t.Fatalf("stream %d status = %d, want 200", i, code)
		}
	}
	if code := openStream(t, srv.URL); code != http.StatusTooManyRequests {
		t.Fatalf("third stream status = %d, want 429", code)
	}
	if n := up.hits.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (the refused one must not reach it)", n)
	}
	refused := rec.refused()
	if len(refused) != 1 {
		t.Fatalf("429 rows = %d, want 1", len(refused))
	}
	c := refused[0]
	if c.Error != "tenant_concurrency_limit" || c.TenantID != "tenant-x" || c.KeyID != "key-abc" ||
		c.Provider != "anthropic" || c.RequestedModel != "" || c.FallbackIndex != -1 {
		// requested_model is empty: the refusal comes before the body is read.
		t.Errorf("refusal row = %+v", c)
	}

	// The check runs before the body is read: an over-size body from a
	// tenant at its cap is refused 429 (concurrency), not 413 (body cap),
	// so the pod never buffers it.
	big := `{"model":"claude-haiku-4-5","pad":"` + strings.Repeat("x", 2<<20) + `"}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatalf("over-size request at cap: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-size request at cap: status = %d, want 429 (body read before the check?)", resp.StatusCode)
	}

	// Ending the streams frees their slots.
	up.free()
	waitFor(t, "a slot to free once the streams end", func() bool {
		return openStream(t, srv.URL) == http.StatusOK
	})
}

// AC-1 / REQ-1: with the Redis store the cap holds across pods.
func TestTenantConcurrency_RedisCapAcrossPods(t *testing.T) {
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set; skipping the Redis concurrency tests")
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping %s: %v", addr, err)
	}

	up := newHoldingUpstream(t)
	rec := &allRec{}
	tenant := "t-" + uuid.NewString()
	pods := []*httptest.Server{
		concPlane(t, up.srv.URL, 4, rdb, rec, tenant),
		concPlane(t, up.srv.URL, 4, rdb, rec, tenant),
	}
	t.Cleanup(up.free)

	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code := openStream(t, pods[i%2].URL)
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if codes[http.StatusOK] != 4 || codes[http.StatusTooManyRequests] != 2 {
		t.Fatalf("status counts = %v, want 4x200 and 2x429", codes)
	}
	if n := up.hits.Load(); n != 4 {
		t.Fatalf("upstream calls = %d, want 4", n)
	}
	if n := len(rec.refused()); n != 2 {
		t.Fatalf("429 rows = %d, want 2", n)
	}

	// Ending the streams frees every lease.
	up.free()
	key := "gw:llm:conc:" + tenant
	waitFor(t, "every lease to be released", func() bool {
		return rdb.ZCard(context.Background(), key).Val() == 0
	})
}
