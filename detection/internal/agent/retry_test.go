package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

// A throttled background stage is retried; an inline one is not.
func TestDetectRetriesThrottledBackground(t *testing.T) {
	old := detectRetryPause
	detectRetryPause = 10 * time.Millisecond
	t.Cleanup(func() { detectRetryPause = old })
	var calls atomic.Int32
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, `{"error":"tenant at its in-flight limit"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"result":null}`))
	}))
	t.Cleanup(eng.Close)
	a := New(Config{EngineURL: eng.URL, Token: "t"})
	if _, err := a.detect(context.Background(), wire.DetectRequest{Meta: wire.Meta{TenantID: "a", RequestID: "1"}}); err != nil {
		t.Fatalf("background detect after two 429s: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	calls.Store(0)
	if _, err := a.detect(context.Background(), wire.DetectRequest{Sync: true, Meta: wire.Meta{TenantID: "a", RequestID: "2"}}); err == nil {
		t.Fatal("inline detect retried past a 429")
	}
	if calls.Load() != 1 {
		t.Fatalf("inline calls = %d, want 1", calls.Load())
	}
}
