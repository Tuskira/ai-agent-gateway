package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const testTenant = "tenant-a"

// recordingNotifier is a SessionNotifier that keeps what it is told.
type recordingNotifier struct {
	mu   sync.Mutex
	msgs []delivered
}

type delivered struct {
	tenantID, sessionID string
	msg                 mcp.Request
}

func (n *recordingNotifier) ToolsListChanged(string) {}

func (n *recordingNotifier) Notify(tenantID, sessionID string, msg mcp.Request) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, delivered{tenantID, sessionID, msg})
	return true
}

func (n *recordingNotifier) all() []delivered {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]delivered(nil), n.msgs...)
}

type harness struct {
	orch      *Orchestrator
	backend   *dptest.Backend
	store     *dptest.Store
	conn      *store.Connector
	notifier  *recordingNotifier
	principal *pkgauth.Principal
	sessions  *session.Manager
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	backend := dptest.NewBackend(dptest.BackendOptions{
		Tools:          []mcp.Tool{dptest.SlowToolDef()},
		RequireSession: true,
	})
	t.Cleanup(backend.Close)

	st := dptest.New()
	if err := st.Tenants().Create(ctx, &store.Tenant{ID: testTenant, Slug: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	conn := &store.Connector{
		TenantID: testTenant, Name: "alpha", Slug: "alpha",
		Endpoint: backend.URL, TimeoutMS: 5000, Status: "unknown", Metadata: map[string]any{},
	}
	if err := st.Connectors().Create(ctx, conn); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := session.NewManager(memory.New(), session.Options{Logger: logger})
	notifier := &recordingNotifier{}
	cl := client.New(client.Options{Logger: logger})
	t.Cleanup(func() { _ = cl.Close() })

	orch, err := New(Deps{
		Sessions:   sessions,
		Router:     router.New(st.Connectors(), router.Options{}),
		Client:     cl,
		Connectors: st.Connectors(),
		Logger:     logger,
		Notifier:   notifier,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &harness{
		orch: orch, backend: backend, store: st, conn: conn, notifier: notifier, sessions: sessions,
		principal: &pkgauth.Principal{Subject: "key-1", TenantID: testTenant, Roles: []string{"agent"}, AuthMethod: "apikey"},
	}
}

func (h *harness) ctx() context.Context {
	return pkgauth.WithPrincipal(context.Background(), h.principal)
}

// initialize opens a gateway session (and, through the fan-out, the
// backend session).
func (h *harness) initialize(t *testing.T) *session.Session {
	t.Helper()
	res := h.orch.Handle(h.ctx(), Request{
		JSONRPC: &mcp.Request{
			JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
			Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`),
		},
		Principal: h.principal,
	})
	if res.NewSession == nil {
		t.Fatalf("initialize issued no session: %+v", res.Response)
	}
	return res.NewSession
}

func (h *harness) handle(sess *session.Session, req *mcp.Request) Result {
	in := Request{JSONRPC: req, Principal: h.principal, Session: sess}
	if sess != nil {
		in.Backends = sess
	}
	return h.orch.Handle(h.ctx(), in)
}

func slowCall(id any, params string) *mcp.Request {
	return &mcp.Request{JSONRPC: mcp.Version, ID: id, Method: mcp.MethodToolsCall, Params: json.RawMessage(params)}
}

func cancelNote(requestID, reason string) *mcp.Request {
	return &mcp.Request{
		JSONRPC: mcp.Version, Method: mcp.NotificationCancelled,
		Params: json.RawMessage(`{"requestId":` + requestID + `,"reason":"` + reason + `"}`),
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------

func TestRequestKeyNormalisesNumbersAndKeepsKinds(t *testing.T) {
	cases := []struct {
		a, b any
		same bool
	}{
		{float64(7), float64(7), true},
		{float64(7), 7, true},
		{float64(7), int64(7), true},
		{float64(7), "7", false},
		{"abc", "abc", true},
		{"abc", "abd", false},
	}
	for _, c := range cases {
		ka, okA := requestKey(c.a)
		kb, okB := requestKey(c.b)
		if !okA || !okB {
			t.Fatalf("requestKey(%#v)/(%#v) not keyable", c.a, c.b)
		}
		if (ka == kb) != c.same {
			t.Errorf("requestKey(%#v)=%q vs requestKey(%#v)=%q, want same=%v", c.a, ka, c.b, kb, c.same)
		}
	}

	for _, raw := range []string{`7`, `7.0`, `7e0`} {
		if k, ok := rawRequestKey(json.RawMessage(raw)); !ok || k != "n:7" {
			t.Errorf("rawRequestKey(%s) = %q, %v; want n:7", raw, k, ok)
		}
	}
	if k, _ := rawRequestKey(json.RawMessage(`"7"`)); k != "s:7" {
		t.Errorf(`rawRequestKey("7") = %q, want s:7`, k)
	}
	for _, raw := range []string{``, `null`, `{}`, `[1]`, `true`} {
		if _, ok := rawRequestKey(json.RawMessage(raw)); ok {
			t.Errorf("rawRequestKey(%q) keyed, want rejected", raw)
		}
	}
}

func TestInflightRegistryIsBoundedPerSessionAndWarnsOnce(t *testing.T) {
	r := newInflightRegistry()
	for i := 0; i < maxInflightPerSession; i++ {
		k, _ := requestKey(float64(i))
		if ok, _ := r.register("t", "s", k, &inflightCall{}); !ok {
			t.Fatalf("register #%d refused below the bound", i)
		}
	}
	ok, overflowed := r.register("t", "s", "n:overflow-1", &inflightCall{})
	if ok || !overflowed {
		t.Fatalf("first register over the bound = ok %v, overflowed %v; want false, true", ok, overflowed)
	}
	if ok, overflowed := r.register("t", "s", "n:overflow-2", &inflightCall{}); ok || overflowed {
		t.Fatalf("second register over the bound = ok %v, overflowed %v; want false, false", ok, overflowed)
	}
	// Another session is unaffected.
	if ok, _ := r.register("t", "other", "n:1", &inflightCall{}); !ok {
		t.Fatal("a different session was refused")
	}
	// A duplicate id is refused rather than overwriting the first call.
	first := &inflightCall{}
	r.register("t", "dup", "n:1", first)
	if ok, _ := r.register("t", "dup", "n:1", &inflightCall{}); ok {
		t.Fatal("a duplicate in-flight id replaced the first call")
	}
	if got := r.take("t", "dup", "n:1"); got != first {
		t.Fatal("take returned the wrong call")
	}
	if r.take("t", "dup", "n:1") != nil {
		t.Fatal("a second take found the call again")
	}
}

func TestProgressIsRelayedForTheCallersTokenOnly(t *testing.T) {
	h := newHarness(t)
	sess := h.initialize(t)

	res := h.handle(sess, slowCall(5, `{"name":"alpha__slow","arguments":{"steps":3,"foreign":true},"_meta":{"progressToken":"tok-1"}}`))
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("tools/call failed: %+v", res.Response)
	}

	got := h.notifier.all()
	if len(got) != 3 {
		t.Fatalf("relayed %d notifications, want the 3 for the caller's token: %+v", len(got), got)
	}
	for i, d := range got {
		if d.tenantID != testTenant || d.sessionID != sess.ID {
			t.Errorf("notification %d addressed to %s/%s, want %s/%s", i, d.tenantID, d.sessionID, testTenant, sess.ID)
		}
		if d.msg.Method != mcp.NotificationProgress || d.msg.ID != nil {
			t.Errorf("notification %d = %+v, want a notifications/progress notification", i, d.msg)
		}
		var p mcp.ProgressParams
		if err := json.Unmarshal(d.msg.Params, &p); err != nil {
			t.Fatal(err)
		}
		if string(p.ProgressToken) != `"tok-1"` || p.Progress != float64(i+1) || p.Total == nil || *p.Total != 3 {
			t.Errorf("notification %d params = %s", i, d.msg.Params)
		}
	}
	if h.orch.inflight.count(testTenant, sess.ID) != 0 {
		t.Fatal("the finished call is still registered")
	}
}

func TestNoProgressTokenMeansNoRelay(t *testing.T) {
	h := newHarness(t)
	sess := h.initialize(t)

	res := h.handle(sess, slowCall(5, `{"name":"alpha__slow","arguments":{"steps":2}}`))
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("tools/call failed: %+v", res.Response)
	}
	if got := h.notifier.all(); len(got) != 0 {
		t.Fatalf("relayed %d notifications without a progressToken", len(got))
	}
}

func TestCancelStopsTheCallForwardsUpstreamAndKeepsTheConnectorHealthy(t *testing.T) {
	h := newHarness(t)
	sess := h.initialize(t)

	done := make(chan Result, 1)
	go func() {
		done <- h.handle(sess, slowCall(float64(7), `{"name":"alpha__slow","arguments":{"steps":1,"hold":true}}`))
	}()
	waitFor(t, "the call to reach the backend", func() bool {
		return h.backend.Running() == 1 && h.orch.inflight.count(testTenant, sess.ID) == 1
	})

	// "7" is a different id from 7: nothing is cancelled.
	if res := h.handle(sess, cancelNote(`"7"`, "wrong kind")); res.Response != nil {
		t.Fatalf("a notification got a response: %+v", res.Response)
	}
	// Another session cannot cancel this one's call.
	other := h.initialize(t)
	h.handle(other, cancelNote(`7`, "not yours"))
	if h.orch.inflight.count(testTenant, sess.ID) != 1 {
		t.Fatal("a mismatched cancellation removed the call")
	}

	if res := h.handle(sess, cancelNote(`7`, "user abort")); res.Response != nil {
		t.Fatalf("a notification got a response: %+v", res.Response)
	}

	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled tools/call did not return")
	}
	if res.Response == nil || res.Response.Error == nil || res.Response.Error.Code != mcp.ErrorCodeRequestCancelled {
		t.Fatalf("response = %+v, want -32800", res.Response)
	}
	if !strings.Contains(string(res.Response.Error.Data), "user abort") {
		t.Errorf("error data = %s, want the reason", res.Response.Error.Data)
	}
	if h.orch.inflight.count(testTenant, sess.ID) != 0 {
		t.Fatal("the cancelled call is still registered")
	}

	waitFor(t, "the forwarded cancellation", func() bool { return len(h.backend.Cancellations()) == 1 })
	c := h.backend.Cancellations()[0]
	if ids := h.backend.SlowCallIDs(); len(ids) != 1 || c.RequestID != ids[0] || c.Reason != "user abort" {
		t.Fatalf("backend saw cancellation %+v for calls %v, want the upstream call's own id and the reason", c, ids)
	}
	if !strings.HasPrefix(c.RequestID, "tools/call:slow:") {
		t.Errorf("upstream id = %q, want a per-call tools/call:slow:<nonce>", c.RequestID)
	}

	conn, err := h.store.Connectors().Get(context.Background(), testTenant, h.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Status != router.StatusHealthy {
		t.Fatalf("connector status = %q after a cancellation, want healthy", conn.Status)
	}
	// The backend session survived: the next call reuses it.
	if sess.Backend(h.conn.ID).ProtocolVersion == "" {
		t.Fatal("the cancellation dropped the backend handle")
	}

	// A second cancellation of the same id, and one after completion,
	// are no-ops that forward nothing.
	h.handle(sess, cancelNote(`7`, "again"))
	if res := h.handle(sess, slowCall("s-1", `{"name":"alpha__slow","arguments":{"steps":1}}`)); res.Response.Error != nil {
		t.Fatalf("follow-up call failed: %+v", res.Response.Error)
	}
	h.handle(sess, cancelNote(`"s-1"`, "too late"))
	h.handle(sess, cancelNote(`12345`, "unknown"))
	time.Sleep(50 * time.Millisecond)
	if n := len(h.backend.Cancellations()); n != 1 {
		t.Fatalf("backend saw %d cancellations, want only the one for the running call", n)
	}
}

// TestCancelRecordsTheCauseBeforeForwardingUpstream guards the ordering
// handleCancelled must keep: entry.cancel has to run -- so
// context.Cause(callCtx) already reports the client's cancellation --
// before forwardCancel, which runs on its own goroutine and tells the
// connector, can have any observable effect. Getting this backwards
// let a connector that honours the cancellation close its streamed
// reply (a spec-compliant "send no response") before the gateway's own
// bookkeeping caught up: CallTool then returned a plain stream-closed
// error with no context.Canceled in it, clientCancelled found no cause
// recorded yet, and the caller got a generic tool-call failure -- and
// the connector was wrongly marked unhealthy -- instead of -32800. See
// TestCancelStopsTheCallForwardsUpstreamAndKeepsTheConnectorHealthy for
// the end-to-end behaviour this ordering protects.
//
// This does not depend on scheduling luck: the Go memory model
// guarantees a "go" statement happens after everything the launching
// goroutine did before it, so if entry.cancel is that last thing before
// the "go", the cause it records is guaranteed visible the instant
// forwardCancel starts -- deterministically, every run.
func TestCancelRecordsTheCauseBeforeForwardingUpstream(t *testing.T) {
	h := newHarness(t)
	sess := h.initialize(t)

	callCtx, cancel := context.WithCancelCause(h.ctx())
	entry := &inflightCall{
		ctx:        h.ctx(),
		cancel:     cancel,
		connector:  h.conn,
		upstreamID: "tools/call:probe:1",
	}
	id, _ := requestKey(float64(99))
	if ok, _ := h.orch.inflight.register(testTenant, sess.ID, id, entry); !ok {
		t.Fatal("register refused")
	}

	started := make(chan error, 1)
	release := make(chan struct{})
	forwardCancelStarted = func() {
		started <- context.Cause(callCtx)
		<-release
	}
	defer func() { forwardCancelStarted = nil }()

	done := make(chan Result, 1)
	go func() { done <- h.handle(sess, cancelNote(`99`, "order check")) }()

	var causeAtForwardStart error
	select {
	case causeAtForwardStart = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("forwardCancel never started")
	}
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleCancelled never returned")
	}

	var cause *cancelledByClient
	if !errors.As(causeAtForwardStart, &cause) || cause.reason != "order check" {
		t.Fatalf("context.Cause(callCtx) when forwardCancel started = %v, want the cancelledByClient cause already recorded", causeAtForwardStart)
	}
}

func TestCancelWithoutASessionIsANoOp(t *testing.T) {
	h := newHarness(t)
	if res := h.handle(nil, cancelNote(`1`, "")); res.Response != nil {
		t.Fatalf("got a response: %+v", res.Response)
	}
	if res := h.handle(nil, &mcp.Request{JSONRPC: mcp.Version, Method: mcp.NotificationCancelled, Params: json.RawMessage(`"garbage"`)}); res.Response != nil {
		t.Fatalf("got a response: %+v", res.Response)
	}
}
