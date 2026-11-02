// Package inflight tracks the requests each API key has open on the data
// planes, so containing an agent can cut its calls mid-stream instead of
// waiting for its next request: a long LLM stream or a slow tool call ends
// the moment an admin kills the agent.
//
// Wiring: Middleware wraps a plane's handler (outside authentication) and
// gives every request a cancellable context; Authenticator wraps the plane's
// authenticator and, once a request's key is known, files the request under
// that key. CancelKey cancels them.
package inflight

import (
	"context"
	"errors"
	"net/http"
	"sync"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// ErrContained is the cancel cause of a request cut by CancelKey.
var ErrContained = errors.New("agent contained: an admin cut its open calls")

// Registry holds the open requests per key. The zero value is not usable;
// call New.
type Registry struct {
	mu    sync.Mutex
	next  uint64
	byKey map[string]map[uint64]*slot
}

type slot struct {
	plane  string
	cancel context.CancelCauseFunc
	keyID  string
	id     uint64
}

type ctxKey struct{}

// New returns an empty Registry.
func New() *Registry { return &Registry{byKey: map[string]map[uint64]*slot{}} }

// Middleware gives each request through next a context CancelKey can cancel,
// and drops the request from the registry when it finishes.
func (r *Registry) Middleware(plane string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx, cancel := context.WithCancelCause(req.Context())
			s := &slot{plane: plane, cancel: cancel}
			defer func() {
				r.drop(s)
				cancel(nil)
			}()
			next.ServeHTTP(w, req.WithContext(context.WithValue(ctx, ctxKey{}, s)))
		})
	}
}

// Authenticator wraps inner: a request it authenticates is filed under the
// principal's key, so CancelKey reaches it.
func (r *Registry) Authenticator(inner pkgauth.Authenticator) pkgauth.Authenticator {
	return authenticator{inner: inner, r: r}
}

type authenticator struct {
	inner pkgauth.Authenticator
	r     *Registry
}

func (a authenticator) Name() string { return a.inner.Name() }

func (a authenticator) Authenticate(ctx context.Context, req *http.Request) (*pkgauth.Principal, error) {
	p, err := a.inner.Authenticate(ctx, req)
	if err == nil && p != nil && p.KeyID != "" {
		if s, ok := req.Context().Value(ctxKey{}).(*slot); ok {
			a.r.add(s, p.KeyID)
		}
	}
	return p, err
}

func (r *Registry) add(s *slot, keyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.keyID != "" {
		return // already filed (a plane may authenticate twice)
	}
	r.next++
	s.keyID, s.id = keyID, r.next
	if r.byKey[keyID] == nil {
		r.byKey[keyID] = map[uint64]*slot{}
	}
	r.byKey[keyID][s.id] = s
}

func (r *Registry) drop(s *slot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.keyID == "" {
		return
	}
	delete(r.byKey[s.keyID], s.id)
	if len(r.byKey[s.keyID]) == 0 {
		delete(r.byKey, s.keyID)
	}
}

// CancelKey cuts keyID's open requests on the named planes (all planes when
// none are named) and returns how many it cut.
func (r *Registry) CancelKey(keyID string, planes ...string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, s := range r.byKey[keyID] {
		if len(planes) > 0 && !contains(planes, s.plane) {
			continue
		}
		s.cancel(ErrContained)
		delete(r.byKey[keyID], id)
		n++
	}
	return n
}

// Open reports how many requests keyID has open.
func (r *Registry) Open(keyID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey[keyID])
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
