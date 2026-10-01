package upstream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/upstream"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// recorder is a NotificationHandler that keeps what it is handed.
type recorder struct {
	mu    sync.Mutex
	notes []mcp.Request
}

func (r *recorder) HandleNotification(_ context.Context, _ upstream.SessionRef, _ upstream.ConnectorRef, note mcp.Request) {
	r.mu.Lock()
	r.notes = append(r.notes, note)
	r.mu.Unlock()
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notes)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	backend *dptest.Backend
	conn    *store.Connector
	manager *upstream.Manager
	notes   *recorder
	alive   atomic.Bool
	gone    atomic.Int32
}

func newEnv(t *testing.T, backendOpts dptest.BackendOptions, opts upstream.Options) *env {
	t.Helper()
	e := &env{notes: &recorder{}}
	e.alive.Store(true)
	e.backend = dptest.NewBackend(backendOpts)
	t.Cleanup(e.backend.Close)
	e.conn = &store.Connector{ID: "conn-1", Name: "alpha", Slug: "alpha", Endpoint: e.backend.URL, TimeoutMS: 2000}

	c := client.New(client.Options{Logger: quiet})
	t.Cleanup(func() { _ = c.Close() })

	opts.Client = c
	opts.Notifications = e.notes
	opts.Logger = quiet
	if opts.InitialBackoff == 0 {
		opts.InitialBackoff = 10 * time.Millisecond
		opts.MaxBackoff = 50 * time.Millisecond
	}
	opts.Lookup = func(context.Context, upstream.SessionRef, string) (session.Backend, bool, error) {
		return session.Backend{}, e.alive.Load(), nil
	}
	opts.OnSessionGone = func(upstream.SessionRef) { e.gone.Add(1) }
	m, err := upstream.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e.manager = m
	t.Cleanup(func() {
		m.Close()
		if n := m.Running(); n != 0 {
			t.Errorf("%d upstream goroutines still running after Close", n)
		}
	})
	return e
}

var sess = upstream.SessionRef{TenantID: "t", SessionID: "s1"}

func (e *env) target(conn *store.Connector) upstream.Target {
	return upstream.Target{Session: sess, Connector: conn, Backend: session.Backend{ProtocolVersion: mcp.ProtocolVersion}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAStreamReconnectsAndResumesFromTheLastEventID(t *testing.T) {
	e := newEnv(t, dptest.BackendOptions{}, upstream.Options{})
	if err := e.manager.Acquire(context.Background(), e.target(e.conn), "test"); err != nil {
		t.Fatal(err)
	}

	e.backend.Notify(mcp.NotificationResourcesListChanged, nil)
	waitFor(t, "the first notification", func() bool { return e.notes.count() == 1 })

	// Dropped by the server; what it sends while nobody listens is
	// replayed after the reconnect because the gateway names the last
	// event it saw.
	e.backend.DropStreams()
	missed := e.backend.Notify(mcp.NotificationPromptsListChanged, nil)
	waitFor(t, "the missed notification after reconnecting", func() bool { return e.notes.count() == 2 })

	ids := e.backend.LastEventIDs()
	if len(ids) != 2 || ids[0] != "" || ids[1] != "ev-1" {
		t.Fatalf("Last-Event-ID per GET = %q, want [\"\" \"ev-1\"] (missed %s)", ids, missed)
	}
	if e.manager.Streams() != 1 || e.manager.Running() != 1 {
		t.Fatalf("streams/goroutines = %d/%d, want 1/1", e.manager.Streams(), e.manager.Running())
	}
}

func TestA405IsRecordedAndNotRetried(t *testing.T) {
	e := newEnv(t, dptest.BackendOptions{NoStream: true}, upstream.Options{})

	for i := 0; i < 3; i++ {
		err := e.manager.Acquire(context.Background(), e.target(e.conn), "test")
		if !errors.Is(err, upstream.ErrStreamUnsupported) {
			t.Fatalf("Acquire #%d = %v, want ErrStreamUnsupported", i+1, err)
		}
	}
	time.Sleep(200 * time.Millisecond) // many backoffs' worth
	if n := e.backend.StreamGETs(); n != 1 {
		t.Fatalf("backend saw %d GETs, want 1", n)
	}
	if e.manager.Streams() != 0 || e.manager.Running() != 0 {
		t.Fatalf("streams/goroutines = %d/%d, want 0/0", e.manager.Streams(), e.manager.Running())
	}

	// A new session is asked afresh.
	e.manager.CloseSession(sess)
	_ = e.manager.Acquire(context.Background(), e.target(e.conn), "test")
	if n := e.backend.StreamGETs(); n != 2 {
		t.Fatalf("backend saw %d GETs after the session was forgotten, want 2", n)
	}
}

func TestTheLastReleaseClosesTheStream(t *testing.T) {
	e := newEnv(t, dptest.BackendOptions{}, upstream.Options{})
	ctx := context.Background()
	for _, need := range []string{"a", "b", "a"} {
		if err := e.manager.Acquire(ctx, e.target(e.conn), need); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.backend.StreamGETs(); n != 1 {
		t.Fatalf("%d GETs for one (session, connector), want 1", n)
	}

	e.manager.Release(sess, e.conn.ID, "a")
	time.Sleep(50 * time.Millisecond)
	if e.backend.OpenStreams() != 1 {
		t.Fatal("the stream closed while a need was still held")
	}
	e.manager.Release(sess, e.conn.ID, "b")
	waitFor(t, "the stream to close", func() bool {
		return e.backend.OpenStreams() == 0 && e.manager.Running() == 0 && e.manager.Streams() == 0
	})
}

func TestTheStreamCapIsPerSession(t *testing.T) {
	e := newEnv(t, dptest.BackendOptions{}, upstream.Options{MaxPerSession: 1})
	ctx := context.Background()
	other := &store.Connector{ID: "conn-2", Name: "beta", Slug: "beta", Endpoint: e.backend.URL, TimeoutMS: 2000}

	if err := e.manager.Acquire(ctx, e.target(e.conn), "x"); err != nil {
		t.Fatal(err)
	}
	if err := e.manager.Acquire(ctx, e.target(other), "x"); !errors.Is(err, upstream.ErrTooManyStreams) {
		t.Fatalf("second connector = %v, want ErrTooManyStreams", err)
	}
	// Another session has its own budget.
	t2 := e.target(other)
	t2.Session = upstream.SessionRef{TenantID: "t", SessionID: "s2"}
	if err := e.manager.Acquire(ctx, t2, "x"); err != nil {
		t.Fatalf("another session: %v", err)
	}
}

func TestAGoneSessionEndsItsStreams(t *testing.T) {
	e := newEnv(t, dptest.BackendOptions{}, upstream.Options{})
	if err := e.manager.Acquire(context.Background(), e.target(e.conn), "x"); err != nil {
		t.Fatal(err)
	}

	// Found by the sweep.
	e.alive.Store(false)
	e.manager.Sweep(context.Background())
	waitFor(t, "the swept stream to close", func() bool { return e.manager.Running() == 0 && e.backend.OpenStreams() == 0 })
	if e.gone.Load() != 1 {
		t.Fatalf("OnSessionGone called %d times, want 1", e.gone.Load())
	}

	// Found on a reconnect: the stream is not reopened for a dead
	// session.
	e.alive.Store(true)
	if err := e.manager.Acquire(context.Background(), e.target(e.conn), "x"); err != nil {
		t.Fatal(err)
	}
	gets := e.backend.StreamGETs()
	e.alive.Store(false)
	e.backend.DropStreams()
	waitFor(t, "the loop to give up", func() bool { return e.manager.Running() == 0 })
	if n := e.backend.StreamGETs(); n != gets {
		t.Fatalf("a stream was reopened for a gone session (%d GETs, want %d)", n, gets)
	}
	if e.gone.Load() != 2 {
		t.Fatalf("OnSessionGone called %d times, want 2", e.gone.Load())
	}
}

func TestAnIdleStreamIsNotATimeout(t *testing.T) {
	e := newEnv(t, dptest.BackendOptions{}, upstream.Options{})
	e.conn.TimeoutMS = 50
	if err := e.manager.Acquire(context.Background(), e.target(e.conn), "x"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := e.backend.StreamGETs(); n != 1 {
		t.Fatalf("an idle stream was torn down and reopened (%d GETs)", n)
	}
	e.backend.Notify(mcp.NotificationResourcesListChanged, nil)
	waitFor(t, "a notification on the long-idle stream", func() bool { return e.notes.count() == 1 })
}

func TestAConnectorRequestIsAnsweredByTheHandler(t *testing.T) {
	e := newEnv(t, dptest.BackendOptions{}, upstream.Options{})
	if err := e.manager.Acquire(context.Background(), e.target(e.conn), "x"); err != nil {
		t.Fatal(err)
	}
	id := e.backend.Request("roots/list", nil)
	waitFor(t, "the answer", func() bool { return len(e.backend.Replies()) == 1 })

	reply := e.backend.Replies()[0]
	if mcp.FormatID(reply.ID) != id || reply.Error == nil || reply.Error.Code != mcp.ErrorCodeMethodNotFound {
		t.Fatalf("reply = %+v, want -32601 under id %s", reply, id)
	}
	if e.notes.count() != 0 {
		t.Fatal("a request was handed to the notification handler")
	}
}
