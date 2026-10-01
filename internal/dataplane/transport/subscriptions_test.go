package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

const (
	staticURI   = "test://static/resource/1"
	reportURI   = "file:///var/data/report.md"
	gwStaticURI = "gw://alpha/" + staticURI
	gwReportURI = "gw://alpha/" + reportURI
)

func subscribeRequest(method string, id int, uri string) mcp.Request {
	params, _ := json.Marshal(mcp.ResourcesSubscribeParams{URI: uri})
	return mcp.Request{JSONRPC: mcp.Version, ID: id, Method: method, Params: params}
}

// subscribe runs resources/subscribe on a session and returns the
// decoded response.
func (f *fixture) subscribe(t *testing.T, sessionID, uri string, headers ...map[string]string) mcp.Response {
	t.Helper()
	h := map[string]string{mcp.HeaderSessionID: sessionID}
	for _, extra := range headers {
		for k, v := range extra {
			h[k] = v
		}
	}
	_, resp := f.call(t, subscribeRequest(mcp.MethodResourcesSubscribe, 7, uri), h)
	return resp
}

// nextNotification reads frames until one that is not the tenant-wide
// tools/list_changed arrives, or d passes.
func nextNotification(t *testing.T, frames <-chan string, d time.Duration) (mcp.Request, bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return mcp.Request{}, false
			}
			var note mcp.Request
			if err := json.Unmarshal([]byte(frame), &note); err != nil {
				t.Fatalf("frame %q: %v", frame, err)
			}
			if note.Method == mcp.NotificationToolsListChanged {
				continue
			}
			return note, true
		case <-deadline:
			return mcp.Request{}, false
		}
	}
}

func updatedURI(t *testing.T, note mcp.Request) string {
	t.Helper()
	if note.Method != mcp.NotificationResourcesUpdated {
		t.Fatalf("notification = %s, want %s", note.Method, mcp.NotificationResourcesUpdated)
	}
	var p mcp.ResourceUpdatedParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p.URI
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSubscribeRelaysAnUpdateWithTheURIReNamespaced(t *testing.T) {
	f := newFixture(t, fixtureOptions{ConnectorMetadata: map[string]any{
		"headers": map[string]any{"X-Static-Header": map[string]any{"type": "static", "value": "static-value"}},
	}})
	sessionID := f.initialize(t)
	frames := f.openStream(t, sessionID)

	resp := f.subscribe(t, sessionID, gwStaticURI)
	if resp.Error != nil || string(resp.Result) != "{}" {
		t.Fatalf("resources/subscribe = %s / %+v, want {}", resp.Result, resp.Error)
	}
	// The connector was asked with its own URI, and a stream is open.
	if !f.backend.Subscribed(staticURI) {
		t.Fatal("the backend was not subscribed to the original uri")
	}
	if n := f.backend.OpenStreams(); n != 1 {
		t.Fatalf("backend has %d streams open, want 1", n)
	}
	// The stream is opened like any call: on the backend session, with
	// the connector's resolved headers and the gateway-owned ones.
	h := f.backend.StreamHeaders()
	for name, want := range map[string]string{
		mcp.HeaderSessionID: f.backend.SessionID(),
		"X-Static-Header":   "static-value",
		"X-Tenant-Id":       tenantID,
		"Accept":            "text/event-stream",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("stream GET %s = %q, want %q", name, got, want)
		}
	}

	if !f.backend.ResourceUpdated(staticURI) {
		t.Fatal("backend did not emit the update")
	}
	note, ok := nextNotification(t, frames, 5*time.Second)
	if !ok {
		t.Fatal("the update never reached the agent's stream")
	}
	if got := updatedURI(t, note); got != gwStaticURI {
		t.Fatalf("relayed uri = %q, want %q", got, gwStaticURI)
	}
}

func TestUnsubscribeStopsTheUpdatesAndAnUnsubscribedURIIsDropped(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	sessionID := f.initialize(t)
	frames := f.openStream(t, sessionID)

	for _, uri := range []string{gwStaticURI, gwReportURI} {
		if resp := f.subscribe(t, sessionID, uri); resp.Error != nil {
			t.Fatalf("subscribe %s: %+v", uri, resp.Error)
		}
	}
	_, resp := f.call(t, subscribeRequest(mcp.MethodResourcesUnsubscribe, 8, gwStaticURI),
		map[string]string{mcp.HeaderSessionID: sessionID})
	if resp.Error != nil || string(resp.Result) != "{}" {
		t.Fatalf("resources/unsubscribe = %s / %+v", resp.Result, resp.Error)
	}
	if f.backend.Subscribed(staticURI) {
		t.Fatal("the unsubscribe was not forwarded with the original uri")
	}

	// The connector (mis)sends an update for the dropped uri anyway, then
	// one for the uri still held: only the second reaches the agent.
	f.backend.Notify(mcp.NotificationResourcesUpdated, mcp.ResourceUpdatedParams{URI: staticURI})
	f.backend.Notify(mcp.NotificationResourcesUpdated, mcp.ResourceUpdatedParams{URI: "test://never/subscribed"})
	f.backend.ResourceUpdated(reportURI)

	note, ok := nextNotification(t, frames, 5*time.Second)
	if !ok {
		t.Fatal("the update for the uri still subscribed never arrived")
	}
	if got := updatedURI(t, note); got != gwReportURI {
		t.Fatalf("first relayed uri = %q, want %q (the others must be dropped)", got, gwReportURI)
	}

	// Unsubscribing the last uri closes the connector stream; doing it
	// twice is harmless.
	for i := 0; i < 2; i++ {
		_, resp = f.call(t, subscribeRequest(mcp.MethodResourcesUnsubscribe, 9, gwReportURI),
			map[string]string{mcp.HeaderSessionID: sessionID})
		if resp.Error != nil {
			t.Fatalf("unsubscribe #%d: %+v", i+1, resp.Error)
		}
	}
	eventually(t, "the connector stream to close", func() bool { return f.backend.OpenStreams() == 0 })
}

func TestTwoSessionsDoNotSeeEachOthersUpdates(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	a, b := f.initialize(t), f.initialize(t)
	framesA, framesB := f.openStream(t, a), f.openStream(t, b)

	if resp := f.subscribe(t, a, gwStaticURI); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if resp := f.subscribe(t, b, gwReportURI); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	eventually(t, "both sessions' streams", func() bool { return f.backend.OpenStreams() == 2 })

	// dptest broadcasts every update on every stream, so both sessions'
	// streams carry both; each session must be told only of its own.
	f.backend.ResourceUpdated(staticURI)
	f.backend.ResourceUpdated(reportURI)

	for _, tc := range []struct {
		frames <-chan string
		want   string
	}{{framesA, gwStaticURI}, {framesB, gwReportURI}} {
		note, ok := nextNotification(t, tc.frames, 5*time.Second)
		if !ok {
			t.Fatalf("no update for %s", tc.want)
		}
		if got := updatedURI(t, note); got != tc.want {
			t.Fatalf("session got %q, want only %q", got, tc.want)
		}
		if extra, ok := nextNotification(t, tc.frames, 200*time.Millisecond); ok {
			t.Fatalf("session also got %s %s", extra.Method, extra.Params)
		}
	}
}

func TestSubscribeOnAConnectorOutsideTheProfileIsDenied(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})
	beta, _ := f.addConnector(t, "beta", catalogOptions())
	sessionID := f.initialize(t)
	reader := map[string]string{profile.Header: "Reader"}

	resp := f.subscribe(t, sessionID, "gw://beta/"+staticURI, reader)
	wantCode(t, resp, mcp.ErrorCodeToolNotAllowed)
	if beta.StreamGETs() != 0 || beta.Subscribed(staticURI) {
		t.Fatal("a denied subscribe still reached the connector")
	}

	// The granted connector is fine under the same profile.
	if resp := f.subscribe(t, sessionID, gwStaticURI, reader); resp.Error != nil {
		t.Fatalf("subscribe on a granted connector: %+v", resp.Error)
	}
}

func TestSubscribeNeedsASessionAndAGatewayURI(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, resp := f.call(t, subscribeRequest(mcp.MethodResourcesSubscribe, 1, gwStaticURI), nil)
	wantCode(t, resp, mcp.ErrorCodeInvalidRequest)
	if !strings.Contains(resp.Error.Message, "session") {
		t.Errorf("message %q does not say a session is needed", resp.Error.Message)
	}

	sessionID := f.initialize(t)
	resp = f.subscribe(t, sessionID, staticURI)
	wantCode(t, resp, mcp.ErrorCodeInvalidParams)
	resp = f.subscribe(t, sessionID, "gw://gamma/"+staticURI)
	wantCode(t, resp, mcp.ErrorCodeConnectorNotFound)
	if f.backend.StreamGETs() != 0 {
		t.Fatal("a refused subscribe opened a connector stream")
	}
}

func TestAConnectorWithoutAStreamIsAClearErrorAndIsNotRetried(t *testing.T) {
	f := newFixture(t, fixtureOptions{NoStream: true})
	sessionID := f.initialize(t)

	for i := 0; i < 3; i++ {
		resp := f.subscribe(t, sessionID, gwStaticURI)
		wantCode(t, resp, mcp.ErrorCodeMethodNotFound)
		if !strings.Contains(resp.Error.Message, "no server-to-client stream") {
			t.Fatalf("message %q does not say why", resp.Error.Message)
		}
	}
	if f.backend.Subscribed(staticURI) {
		t.Fatal("the subscribe was forwarded although no update could ever be relayed")
	}
	// One GET, recorded as "no stream", never retried: not on the later
	// subscribes, not in the background.
	time.Sleep(1500 * time.Millisecond)
	if n := f.backend.StreamGETs(); n != 1 {
		t.Fatalf("backend saw %d GETs, want exactly 1", n)
	}
	if streams, goroutines := f.plane.UpstreamStreams(); streams != 0 || goroutines != 0 {
		t.Fatalf("plane holds %d streams / %d goroutines, want 0 / 0", streams, goroutines)
	}

	// Other methods on that connector are unaffected.
	_, resp := f.call(t, rpc(mcp.MethodResourcesRead, 2, `{"uri":"`+gwStaticURI+`"}`), nil)
	if resp.Error != nil {
		t.Fatalf("resources/read: %+v", resp.Error)
	}
}

func TestEndingTheSessionClosesItsConnectorStreams(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.addConnector(t, "beta", catalogOptions())
	sessionID := f.initialize(t)

	for _, uri := range []string{gwStaticURI, gwReportURI, "gw://beta/" + staticURI} {
		if resp := f.subscribe(t, sessionID, uri); resp.Error != nil {
			t.Fatalf("subscribe %s: %+v", uri, resp.Error)
		}
	}
	if streams, goroutines := f.plane.UpstreamStreams(); streams != 2 || goroutines != 2 {
		t.Fatalf("plane holds %d streams / %d goroutines, want 2 / 2 (one per connector)", streams, goroutines)
	}

	req, err := http.NewRequest(http.MethodDelete, f.server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set(mcp.HeaderSessionID, sessionID)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	eventually(t, "every upstream goroutine to exit", func() bool {
		streams, goroutines := f.plane.UpstreamStreams()
		return streams == 0 && goroutines == 0
	})
	eventually(t, "the connector to see its stream closed", func() bool { return f.backend.OpenStreams() == 0 })
}

func TestAnIdleOrDroppedStreamLeavesTheConnectorHealthy(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	ctx := context.Background()

	// A tight timeout: an idle stream outliving it must not count as a
	// timeout.
	f.conn.TimeoutMS = 150
	if err := f.store.Connectors().Update(ctx, f.conn); err != nil {
		t.Fatal(err)
	}
	sessionID := f.initialize(t)
	frames := f.openStream(t, sessionID)
	if resp := f.subscribe(t, sessionID, gwStaticURI); resp.Error != nil {
		t.Fatal(resp.Error)
	}

	time.Sleep(600 * time.Millisecond)
	if n := f.backend.StreamGETs(); n != 1 {
		t.Fatalf("an idle stream was reopened: %d GETs", n)
	}
	status := func() string {
		conn, err := f.store.Connectors().Get(ctx, tenantID, f.conn.ID)
		if err != nil {
			t.Fatal(err)
		}
		return conn.Status
	}
	if got := status(); got != "healthy" {
		t.Fatalf("connector status after an idle stream = %q, want healthy", got)
	}

	// The connector drops the stream; an update sent while the gateway is
	// away is replayed on the reconnect, which resumes by Last-Event-ID.
	f.backend.ResourceUpdated(staticURI)
	if _, ok := nextNotification(t, frames, 5*time.Second); !ok {
		t.Fatal("first update never arrived")
	}
	f.backend.DropStreams()
	f.backend.ResourceUpdated(staticURI)

	note, ok := nextNotification(t, frames, 10*time.Second)
	if !ok {
		t.Fatal("the update sent while the stream was down was never replayed")
	}
	if got := updatedURI(t, note); got != gwStaticURI {
		t.Fatalf("replayed uri = %q", got)
	}
	ids := f.backend.LastEventIDs()
	if len(ids) != 2 || ids[0] != "" || ids[1] == "" {
		t.Fatalf("Last-Event-ID per GET = %q, want the reconnect to resume", ids)
	}
	if got := status(); got != "healthy" {
		t.Fatalf("connector status after a dropped stream = %q, want healthy", got)
	}
}

func TestSubscriptionAndStreamCapsAreEnforced(t *testing.T) {
	f := newFixture(t, fixtureOptions{MaxSubscriptions: 2, MaxUpstreamStreams: 1})
	f.addConnector(t, "beta", catalogOptions())
	sessionID := f.initialize(t)

	if resp := f.subscribe(t, sessionID, gwStaticURI); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	// Re-subscribing to a held uri is idempotent and costs nothing.
	if resp := f.subscribe(t, sessionID, gwStaticURI); resp.Error != nil {
		t.Fatal(resp.Error)
	}

	// A second connector needs a second stream: over the stream cap.
	resp := f.subscribe(t, sessionID, "gw://beta/"+staticURI)
	wantCode(t, resp, mcp.ErrorCodeInvalidRequest)
	if !strings.Contains(resp.Error.Message, "too many connector streams") {
		t.Fatalf("message = %q", resp.Error.Message)
	}

	if resp := f.subscribe(t, sessionID, gwReportURI); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	resp = f.subscribe(t, sessionID, "gw://alpha/test://dynamic/resource/9")
	wantCode(t, resp, mcp.ErrorCodeInvalidRequest)
	if !strings.Contains(resp.Error.Message, "too many resource subscriptions") {
		t.Fatalf("message = %q", resp.Error.Message)
	}
	if f.backend.Subscribed("test://dynamic/resource/9") {
		t.Fatal("a subscribe over the cap reached the connector")
	}
}

func TestListChangedIsRelayedAndAConnectorRequestIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	sessionID := f.initialize(t)
	frames := f.openStream(t, sessionID)
	if resp := f.subscribe(t, sessionID, gwStaticURI); resp.Error != nil {
		t.Fatal(resp.Error)
	}

	for _, method := range []string{mcp.NotificationPromptsListChanged, mcp.NotificationResourcesListChanged} {
		f.backend.Notify(method, nil)
		note, ok := nextNotification(t, frames, 5*time.Second)
		if !ok || note.Method != method {
			t.Fatalf("got %+v (%v), want %s relayed", note, ok, method)
		}
	}

	// tools/list_changed becomes the tenant-wide signal, after the
	// connector's tools are re-listed.
	before := f.backend.Calls(mcp.MethodToolsList)
	f.backend.Notify(mcp.NotificationToolsListChanged, nil)
	deadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case frame := <-frames:
			got = strings.Contains(frame, mcp.NotificationToolsListChanged)
		case <-deadline:
			t.Fatal("tools/list_changed never reached the stream")
		}
	}
	if f.backend.Calls(mcp.MethodToolsList) <= before {
		t.Error("the connector's tools were not re-listed after it announced a change")
	}

	// A server-to-client request the connector's policy does not permit
	// gets -32601 under the same id.
	id := f.backend.Request("sampling/createMessage", map[string]any{"messages": []any{}})
	eventually(t, "the gateway's answer", func() bool { return len(f.backend.Replies()) == 1 })
	reply := f.backend.Replies()[0]
	if mcp.FormatID(reply.ID) != id || reply.Error == nil || reply.Error.Code != mcp.ErrorCodeMethodNotFound ||
		!strings.Contains(reply.Error.Message, "not permitted for this connector") {
		t.Fatalf("reply = %+v, want -32601 for %s", reply, id)
	}
}
