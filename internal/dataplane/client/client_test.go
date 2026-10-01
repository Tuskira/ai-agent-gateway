package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

// newTestClient builds a Client whose retry backoff costs no wall-clock
// time, so a three-attempt path runs in microseconds.
func newTestClient(t *testing.T, registry *dpheaders.Registry) *Client {
	t.Helper()
	return New(Options{
		Headers: registry,
		sleep:   func(context.Context, time.Duration) error { return nil },
	})
}

func connector(endpoint string) *store.Connector {
	return &store.Connector{
		ID: "conn-1", TenantID: "tenant-a", Name: "alpha", Slug: "alpha",
		Endpoint: endpoint, TimeoutMS: 2000,
	}
}

func authedContext() context.Context {
	return pkgauth.WithPrincipal(context.Background(), &pkgauth.Principal{
		Subject: "key-1", TenantID: "tenant-a", Email: "a@example.com",
		Roles: []string{"agent"}, AuthMethod: "apikey", KeyID: "key-1",
		RawCredential: "gk_secret",
	})
}

func pingCall(conn *store.Connector) Call {
	return Call{
		Connector: conn,
		Request:   &mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing},
		Trace:     trace.Context{TraceID: trace.GenerateTraceID(), SpanID: trace.GenerateSpanID(), Flags: trace.FlagsSampled},
	}
}

func writeOK(w http.ResponseWriter, id any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcp.NewSuccessResponse(id, mcp.PingResult{}))
}

// ---------------------------------------------------------------------------
// retry policy
// ---------------------------------------------------------------------------

func TestRetriesA5xxUpToThreeAttempts(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeOK(w, 1)
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	result, err := c.Do(authedContext(), pingCall(connector(srv.URL)))
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Response == nil || result.Response.Error != nil {
		t.Fatalf("unexpected response: %+v", result.Response)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestGivesUpAfterThreeAttempts(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	if _, err := c.Do(authedContext(), pingCall(connector(srv.URL))); err == nil {
		t.Fatal("Do = nil error, want a failure")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want exactly 3", got)
	}
}

func TestDoesNotRetryA4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	if _, err := c.Do(authedContext(), pingCall(connector(srv.URL))); err == nil {
		t.Fatal("Do = nil error, want a failure")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a 403 is not going to fix itself", got)
	}
}

func TestDoesNotRetryATimeout(t *testing.T) {
	// The backend is still working on the original request. A retry
	// adds load to a server that is already too slow, and risks running
	// a side-effecting tool twice.
	var attempts atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	conn := connector(srv.URL)
	conn.TimeoutMS = 100

	c := newTestClient(t, nil)
	_, err := c.Do(authedContext(), pingCall(conn))
	if err == nil {
		t.Fatal("Do = nil error, want a timeout")
	}
	if !IsTimeout(err) {
		t.Fatalf("Do = %v, want a timeout classified as ErrTimeout", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a timeout is never retried", got)
	}
}

// ---------------------------------------------------------------------------
// 401 recovery
// ---------------------------------------------------------------------------

// recordingProvider is an ExternalProvider that counts evictions.
type recordingProvider struct {
	mu          sync.Mutex
	value       string
	invalidated []string
}

func (p *recordingProvider) Type() string                    { return "external" }
func (p *recordingProvider) ProviderID() string              { return "recording" }
func (p *recordingProvider) ProviderName() string            { return "Recording" }
func (p *recordingProvider) ConfigSchema() map[string]any    { return map[string]any{} }
func (p *recordingProvider) Validate(_ map[string]any) error { return nil }
func (p *recordingProvider) Resolve(context.Context, map[string]any) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value, nil
}

func (p *recordingProvider) Invalidate(_ context.Context, principalID, connectorSlug string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invalidated = append(p.invalidated, principalID+"/"+connectorSlug)
	// Re-minting after eviction is what makes the replay worth doing.
	p.value = "fresh-token"
}

func (p *recordingProvider) evictions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.invalidated...)
}

var _ pkgheaders.Invalidator = (*recordingProvider)(nil)

func TestA401EvictsCredentialsAndReplaysExactlyOnce(t *testing.T) {
	provider := &recordingProvider{value: "stale-token"}
	registry := dpheaders.NewRegistry()
	if err := registry.RegisterExternal(provider); err != nil {
		t.Fatal(err)
	}

	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Token"))
		mu.Unlock()
		if r.Header.Get("X-Token") != "fresh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeOK(w, 1)
	}))
	defer srv.Close()

	conn := connector(srv.URL)
	conn.Metadata = map[string]any{"headers": map[string]any{
		"X-Token": map[string]any{"type": "external", "provider": "recording", "config": map[string]any{}},
	}}

	c := newTestClient(t, registry)
	result, err := c.Do(authedContext(), pingCall(conn))
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Response == nil || result.Response.Error != nil {
		t.Fatalf("unexpected response: %+v", result.Response)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "stale-token" || seen[1] != "fresh-token" {
		t.Fatalf("backend saw %v, want [stale-token fresh-token]", seen)
	}
	if evicted := provider.evictions(); len(evicted) != 1 || evicted[0] != "key-1/alpha" {
		t.Fatalf("evictions = %v, want one for key-1/alpha", evicted)
	}
}

func TestASecondConsecutive401IsNotReplayedAgain(t *testing.T) {
	// Thrashing on a genuinely broken customer configuration helps
	// nobody: the second 401 is surfaced.
	provider := &recordingProvider{value: "stale-token"}
	registry := dpheaders.NewRegistry()
	if err := registry.RegisterExternal(provider); err != nil {
		t.Fatal(err)
	}

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	conn := connector(srv.URL)
	conn.Metadata = map[string]any{"headers": map[string]any{
		"X-Token": map[string]any{"type": "external", "provider": "recording", "config": map[string]any{}},
	}}

	c := newTestClient(t, registry)
	if _, err := c.Do(authedContext(), pingCall(conn)); err == nil {
		t.Fatal("Do = nil error, want a failure")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (original + one replay)", got)
	}
}

func TestA401WithNoInvalidatorIsNotReplayed(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, dpheaders.NewRegistry())
	if _, err := c.Do(authedContext(), pingCall(connector(srv.URL))); err == nil {
		t.Fatal("Do = nil error, want a failure")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: nothing to evict, nothing to gain from a replay", got)
	}
}

// ---------------------------------------------------------------------------
// header resolution order
// ---------------------------------------------------------------------------

func TestGatewayOwnedHeadersWinOverConnectorConfiguration(t *testing.T) {
	// A connector's header config is operator data. If it could set
	// X-Tenant-Id, a connector row would be a tenant-impersonation
	// primitive, so the gateway writes it last, unconditionally.
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		writeOK(w, 1)
	}))
	defer srv.Close()

	conn := connector(srv.URL)
	conn.Metadata = map[string]any{"headers": map[string]any{
		HeaderTenantID:      map[string]any{"type": "static", "value": "tenant-somebody-else"},
		mcp.HeaderSessionID: map[string]any{"type": "static", "value": "hijacked-session"},
		"X-Static":          map[string]any{"type": "static", "value": "plain"},
		"X-Caller":          map[string]any{"type": "token_field", "field": "email"},
		"X-Bearer":          map[string]any{"type": "token_field", "field": "bearer_token", "prefix": "Bearer "},
	}}

	call := pingCall(conn)
	call.SessionID = "backend-session-1"

	reg := dpheaders.NewRegistry()
	reg.SetAllowBearerForwarding(true)
	c := newTestClient(t, reg)
	if _, err := c.Do(authedContext(), call); err != nil {
		t.Fatal(err)
	}

	if v := got.Get(HeaderTenantID); v != "tenant-a" {
		t.Errorf("X-Tenant-Id = %q, want the principal's tenant", v)
	}
	if v := got.Get(mcp.HeaderSessionID); v != "backend-session-1" {
		t.Errorf("mcp-session-id = %q, want the negotiated backend session", v)
	}
	if v := got.Get("X-Static"); v != "plain" {
		t.Errorf("X-Static = %q", v)
	}
	if v := got.Get("X-Caller"); v != "a@example.com" {
		t.Errorf("X-Caller = %q", v)
	}
	if v := got.Get("X-Bearer"); v != "Bearer gk_secret" {
		t.Errorf("X-Bearer = %q", v)
	}
	if v := got.Get(trace.Header); v == "" {
		t.Error("traceparent was not propagated to the backend")
	}
	if v := got.Get(mcp.HeaderProtocolVersion); v != mcp.ProtocolVersion {
		t.Errorf("protocol version header = %q, want %q", v, mcp.ProtocolVersion)
	}
}

func TestAnUnresolvableHeaderIsSkippedNotFatal(t *testing.T) {
	// One optional enrichment header that cannot be resolved must not
	// take the whole connector down; the backend decides whether the
	// missing header matters, by answering 401.
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		writeOK(w, 1)
	}))
	defer srv.Close()

	conn := connector(srv.URL)
	conn.Metadata = map[string]any{"headers": map[string]any{
		"X-Good": map[string]any{"type": "static", "value": "ok"},
		"X-Bad":  map[string]any{"type": "no-such-resolver"},
	}}

	c := newTestClient(t, dpheaders.NewRegistry())
	if _, err := c.Do(authedContext(), pingCall(conn)); err != nil {
		t.Fatalf("Do = %v, want the call to proceed", err)
	}
	if got.Get("X-Good") != "ok" {
		t.Error("the resolvable header was dropped along with the broken one")
	}
	if got.Get("X-Bad") != "" {
		t.Error("an unresolvable header was sent anyway")
	}
}

// ---------------------------------------------------------------------------
// protocol negotiation and response decoding
// ---------------------------------------------------------------------------

func TestFallsBackToTheLegacyProtocolVersionOnce(t *testing.T) {
	var versions []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get(mcp.HeaderProtocolVersion)
		mu.Lock()
		versions = append(versions, v)
		mu.Unlock()
		if v != mcp.ProtocolVersionLegacy {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeOK(w, 1)
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	result, err := c.Do(authedContext(), pingCall(connector(srv.URL)))
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.ProtocolVersion != mcp.ProtocolVersionLegacy {
		t.Errorf("negotiated %q, want %q", result.ProtocolVersion, mcp.ProtocolVersionLegacy)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{mcp.ProtocolVersion, mcp.ProtocolVersionLegacy}
	if len(versions) != 2 || versions[0] != want[0] || versions[1] != want[1] {
		t.Fatalf("versions offered = %v, want %v", versions, want)
	}
}

func TestAPinnedProtocolVersionIsNotDowngraded(t *testing.T) {
	// Once a version is known to work, a 400 is about the payload.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	call := pingCall(connector(srv.URL))
	call.ProtocolVersion = mcp.ProtocolVersion

	c := newTestClient(t, nil)
	if _, err := c.Do(authedContext(), call); err == nil {
		t.Fatal("Do = nil error, want a failure")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestDecodesAnSSEReply(t *testing.T) {
	// MCP's Streamable HTTP transport may answer one request with a
	// single SSE frame rather than a JSON body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set(mcp.HeaderSessionID, "sse-session")
		_, _ = w.Write([]byte("id: 1\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	result, err := c.Do(authedContext(), pingCall(connector(srv.URL)))
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Response == nil || result.Response.Error != nil {
		t.Fatalf("unexpected response: %+v", result.Response)
	}
	if result.SessionID != "sse-session" {
		t.Errorf("SessionID = %q, want the one the backend issued", result.SessionID)
	}
}

func TestNotificationsAcceptA202AndReadNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	call := pingCall(connector(srv.URL))
	call.Request = &mcp.Request{JSONRPC: mcp.Version, Method: mcp.MethodInitialized, Params: json.RawMessage(`{}`)}

	c := newTestClient(t, nil)
	result, err := c.Do(authedContext(), call)
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Response != nil {
		t.Fatalf("a notification must produce no response, got %+v", result.Response)
	}
}

// ---------------------------------------------------------------------------
// tools/list shaping
// ---------------------------------------------------------------------------

func TestListToolsPrefixesNamesAndScrubsOverriddenArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcp.NewSuccessResponse("tools/list", mcp.ToolsListResult{
			Tools: []mcp.Tool{{
				Name:        "search",
				Description: "Search",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"query":      map[string]any{"type": "string"},
						"project_id": map[string]any{"type": "string"},
					},
					Required: []string{"query", "project_id"},
				},
			}},
		}))
	}))
	defer srv.Close()

	conn := connector(srv.URL)
	conn.Metadata = map[string]any{"tool_arg_overrides": map[string]any{"project_id": "p-1"}}

	c := newTestClient(t, nil)
	tools, _, err := c.ListTools(authedContext(), Call{Connector: conn})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	if tools[0].Name != "alpha__search" {
		t.Errorf("name = %q, want %q", tools[0].Name, "alpha__search")
	}
	// Advertising an argument the gateway is going to overwrite only
	// invites a model to guess at a tenant constant.
	if _, present := tools[0].InputSchema.Properties["project_id"]; present {
		t.Error("an overridden argument was left in the advertised schema")
	}
	if _, present := tools[0].InputSchema.Properties["query"]; !present {
		t.Error("a real argument was scrubbed")
	}
	if len(tools[0].InputSchema.Required) != 1 || tools[0].InputSchema.Required[0] != "query" {
		t.Errorf("required = %v, want [query]", tools[0].InputSchema.Required)
	}
}

// ---------------------------------------------------------------------------
// prompts and resources
// ---------------------------------------------------------------------------

func TestListPromptsFollowsEveryPageAndPrefixesNames(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcp.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		var params mcp.PaginatedParams
		_ = json.Unmarshal(req.Params, &params)
		cursors = append(cursors, params.Cursor)

		page := mcp.PromptsListResult{Prompts: []mcp.Prompt{{Name: "one"}}, NextCursor: "p2"}
		if params.Cursor == "p2" {
			page = mcp.PromptsListResult{Prompts: []mcp.Prompt{{Name: "two"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcp.NewSuccessResponse(req.ID, page))
	}))
	defer srv.Close()

	prompts, _, err := newTestClient(t, nil).ListPrompts(authedContext(), Call{Connector: connector(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || prompts[0].Name != "alpha__one" || prompts[1].Name != "alpha__two" {
		t.Fatalf("prompts = %+v, want alpha__one, alpha__two", prompts)
	}
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "p2" {
		t.Errorf("cursors sent = %q, want [\"\" \"p2\"]", cursors)
	}
}

func TestAListThatRepeatsItsCursorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcp.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcp.NewSuccessResponse(req.ID, mcp.ResourcesListResult{NextCursor: "again"}))
	}))
	defer srv.Close()

	if _, _, err := newTestClient(t, nil).ListResources(authedContext(), Call{Connector: connector(srv.URL)}); err == nil {
		t.Fatal("a backend that loops on its cursor was paged forever or silently accepted")
	}
}

func TestListResourcesWrapsURIsAndTemplates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcp.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		var result any = mcp.ResourcesListResult{Resources: []mcp.Resource{{URI: "test://static/1", Name: "one"}}}
		if req.Method == mcp.MethodResourcesTemplatesList {
			result = mcp.ResourceTemplatesListResult{ResourceTemplates: []mcp.ResourceTemplate{{URITemplate: "test://dyn/{id}", Name: "dyn"}}}
		}
		_ = json.NewEncoder(w).Encode(mcp.NewSuccessResponse(req.ID, result))
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	resources, _, err := c.ListResources(authedContext(), Call{Connector: connector(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].URI != "gw://alpha/test://static/1" {
		t.Errorf("resources = %+v", resources)
	}
	templates, _, err := c.ListResourceTemplates(authedContext(), Call{Connector: connector(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || templates[0].URITemplate != "gw://alpha/test://dyn/{id}" {
		t.Errorf("templates = %+v", templates)
	}
}

func TestReadResourceReportsABackendRefusalAsAnRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcp.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcp.NewErrorResponse(req.ID, mcp.NewError(-32002, "resource not found", nil)))
	}))
	defer srv.Close()

	_, _, err := newTestClient(t, nil).ReadResource(authedContext(), Call{Connector: connector(srv.URL)}, "test://missing")
	rpcErr, ok := AsRPCError(err)
	if !ok || rpcErr.Err.Code != -32002 {
		t.Fatalf("err = %v, want an RPCError carrying -32002", err)
	}
}

func TestABearerForwardingHeaderFailsTheCallWhenForwardingIsOff(t *testing.T) {
	// A connector row that predates the gate must never forward the
	// caller's credential, and must say why instead of a confusing 401.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		writeOK(w, 1)
	}))
	defer srv.Close()

	conn := connector(srv.URL)
	conn.Metadata = map[string]any{"headers": map[string]any{
		"Authorization": map[string]any{"type": "token_field", "field": "bearer_token", "prefix": "Bearer "},
	}}
	c := newTestClient(t, dpheaders.NewRegistry())
	_, err := c.Do(authedContext(), pingCall(conn))
	if !errors.Is(err, dpheaders.ErrBearerForwardingDisabled) {
		t.Fatalf("Do err = %v, want ErrBearerForwardingDisabled", err)
	}
	if hit {
		t.Error("the backend was called; the credential must not be forwarded")
	}
}
