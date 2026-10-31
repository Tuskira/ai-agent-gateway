package inflight

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

type fixedAuth struct{ keyID string }

func (fixedAuth) Name() string { return "fixed" }
func (f fixedAuth) Authenticate(context.Context, *http.Request) (*pkgauth.Principal, error) {
	return &pkgauth.Principal{KeyID: f.keyID}, nil
}

func TestCancelKeyCutsOpenRequest(t *testing.T) {
	r := New()
	a := r.Authenticator(fixedAuth{keyID: "k1"})
	started, cause := make(chan struct{}), make(chan error, 1)
	h := r.Middleware("llm")(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, err := a.Authenticate(req.Context(), req); err != nil {
			t.Error(err)
		}
		close(started)
		<-req.Context().Done()
		cause <- context.Cause(req.Context())
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", nil))
		close(done)
	}()
	<-started
	if got := r.Open("k1"); got != 1 {
		t.Fatalf("Open = %d, want 1", got)
	}
	if got := r.CancelKey("k1", "mcp"); got != 0 {
		t.Fatalf("CancelKey(mcp) cut %d llm requests", got)
	}
	if got := r.CancelKey("k1"); got != 1 {
		t.Fatalf("CancelKey = %d, want 1", got)
	}
	select {
	case err := <-cause:
		if !errors.Is(err, ErrContained) {
			t.Fatalf("cause = %v, want ErrContained", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not cut")
	}
	<-done
	if got := r.Open("k1"); got != 0 {
		t.Fatalf("Open after finish = %d", got)
	}
}
