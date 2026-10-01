// Package memory is the in-process session.Store: a map behind a mutex,
// registered as driver "memory". It is the default.
//
// Its scope is one replica. Run two replicas on it and a client whose
// requests land on the other replica gets JSON-RPC -32000 ("session not
// found") and re-initializes -- correct, but it re-does every backend
// handshake. That is why deploy/k8s pins gateway-mcp to one replica
// unless sessions.store is a shared driver (pkg/session/redis).
//
// It has no Notifier: with one replica there is nothing to relay.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
)

// Driver is the name this package registers with pkg/session.
const Driver = "memory"

func init() {
	session.Register(Driver, func(context.Context, session.Config) (session.Store, session.Notifier, error) {
		return New(), nil, nil
	})
}

// Store is the in-process session.Store. It also implements
// session.Sweeper, since a map cannot expire its own entries.
type Store struct {
	mu   sync.RWMutex
	recs map[string]*session.Record
	// now is the clock Get filters expired records with; tests may
	// replace it via NewWithClock.
	now func() time.Time
}

// New returns an empty Store on the wall clock.
func New() *Store { return NewWithClock(time.Now) }

// NewWithClock returns an empty Store whose expiry checks read now.
func NewWithClock(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{recs: make(map[string]*session.Record), now: now}
}

var (
	_ session.Store   = (*Store)(nil)
	_ session.Sweeper = (*Store)(nil)
)

// Get implements session.Store. Every Get hands back a fresh copy, so
// this Store behaves like a serializing one: a caller that forgets to
// Save a mutation loses it here too, which is what keeps the gateway's
// Save discipline honest under the default driver.
func (s *Store) Get(_ context.Context, id string) (*session.Record, error) {
	s.mu.RLock()
	r, ok := s.recs[id]
	s.mu.RUnlock()
	if !ok {
		return nil, session.ErrNotFound
	}
	if r.Expired(s.now()) {
		// Reclaim in passing; the sweep would get it eventually.
		s.mu.Lock()
		if cur, still := s.recs[id]; still && cur == r {
			delete(s.recs, id)
		}
		s.mu.Unlock()
		return nil, session.ErrNotFound
	}
	return r.Clone(), nil
}

// Save implements session.Store.
func (s *Store) Save(_ context.Context, r *session.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	cp := r.Clone()
	s.mu.Lock()
	s.recs[cp.ID] = cp
	s.mu.Unlock()
	return nil
}

// Delete implements session.Store.
func (s *Store) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.recs, id)
	s.mu.Unlock()
	return nil
}

// Sweep implements session.Sweeper.
func (s *Store) Sweep(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	for id, r := range s.recs {
		if r.Expired(now) {
			delete(s.recs, id)
			n++
		}
	}
	return n, nil
}

// Close implements session.Store. It is a no-op: there is nothing to
// release, and the map is dropped with the Store.
func (s *Store) Close() error { return nil }

// Len returns how many records are held, expired ones included until a
// Get or Sweep reclaims them. Exposed for tests and log lines.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.recs)
}
