package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	// Registers the "memory" session driver dataplane.New opens by default.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const (
	tenantID = "tenant-a"
	apiKey   = "gk_test-key"
)

// staticAuth is a one-key Authenticator: authentication itself is tested
// in internal/auth, and this keeps the transport's tests about the
// transport.
type staticAuth struct{}

func (staticAuth) Name() string { return "static" }

func (staticAuth) Authenticate(_ context.Context, r *http.Request) (*pkgauth.Principal, error) {
	token := r.Header.Get("X-Gateway-Key")
	if token == "" {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if token == "" {
		return nil, pkgauth.ErrNoCredential
	}
	if token != apiKey {
		return nil, pkgauth.ErrInvalid
	}
	return &pkgauth.Principal{
		Subject: "key-1", TenantID: tenantID, Email: "agent@example.com",
		Roles: []string{"agent"}, AuthMethod: "apikey", KeyID: "key-1", RawCredential: token,
	}, nil
}

// recordingSink collects the access-log records the plane writes.
type recordingSink struct {
	mu      sync.Mutex
	records []*sink.AccessLog
}

func (s *recordingSink) WriteAccess(a *sink.AccessLog) {
	s.mu.Lock()
	s.records = append(s.records, a)
	s.mu.Unlock()
}
func (s *recordingSink) WriteLLMCall(*sink.LLMCall) {}
func (s *recordingSink) Close() error               { return nil }

func (s *recordingSink) all() []*sink.AccessLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*sink.AccessLog(nil), s.records...)
}

type fixture struct {
	server  *httptest.Server
	backend *dptest.Backend
	store   *dptest.Store
	sink    *recordingSink
	conn    *store.Connector
	plane   *dataplane.Plane
	cfg     *config.Config
}

type fixtureOptions struct {
	RequireProfile bool
	// ProfileTools is the allow-list of the profile named "Reader".
	ProfileTools []string
	// ConnectorMetadata overrides the connector's metadata.
	ConnectorMetadata map[string]any
	// StoreBodies turns on access-log payload capture.
	StoreBodies bool
	// MaxResponseBytes, when set, overrides the captured-response cap.
	MaxResponseBytes int
	// NoCatalog leaves the backend without prompts and resources, so it
	// declares only the tools capability.
	NoCatalog bool
	// PlainCatalogCapabilities has the backend declare prompts and
	// resources with no subscribe and no listChanged.
	PlainCatalogCapabilities bool
	// NoStream has the backend refuse GET with 405.
	NoStream bool
	// MaxUpstreamStreams and MaxSubscriptions override the plane's
	// per-session caps when non-zero.
	MaxUpstreamStreams int
	MaxSubscriptions   int
	// MaxPendingServerRequests and ServerRequestTimeout override the
	// relay's per-session cap and wait when non-zero.
	MaxPendingServerRequests int
	ServerRequestTimeout     time.Duration
	// ConnectorTimeoutMS overrides the connector's timeout_ms (3000).
	ConnectorTimeoutMS int
	// RedisAddr, when set, puts sessions in the redis driver there and
	// runs the plane's routines, so newReplica can add a second replica.
	RedisAddr string
}

func newFixture(t *testing.T, opts fixtureOptions) *fixture {
	t.Helper()
	ctx := context.Background()

	backendOpts := dptest.BackendOptions{
		Tools: []mcp.Tool{
			{Name: "echo", Description: "Echo a message", InputSchema: dptest.ObjectSchema([]string{"message"}, "message")},
			{Name: "secret", Description: "Not for everyone", InputSchema: dptest.ObjectSchema(nil, "x")},
		},
		RequireSession:           true,
		NoStream:                 opts.NoStream,
		PlainCatalogCapabilities: opts.PlainCatalogCapabilities,
	}
	if !opts.NoCatalog {
		backendOpts.Prompts = dptest.SamplePrompts()
		backendOpts.Resources = dptest.SampleResources()
		backendOpts.ResourceTemplates = dptest.SampleResourceTemplates()
		// One item per page, so every list is a multi-page walk.
		backendOpts.PageSize = 1
	}
	backend := dptest.NewBackend(backendOpts)
	t.Cleanup(backend.Close)

	st := dptest.New()
	tenant := &store.Tenant{ID: tenantID, Slug: "a", Name: "A"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	metadata := opts.ConnectorMetadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	timeoutMS := 3000
	if opts.ConnectorTimeoutMS > 0 {
		timeoutMS = opts.ConnectorTimeoutMS
	}
	conn := &store.Connector{
		TenantID: tenantID, Name: "alpha", Slug: "alpha",
		Endpoint: backend.URL, TimeoutMS: timeoutMS, Status: "unknown", Metadata: metadata,
	}
	if err := st.Connectors().Create(ctx, conn); err != nil {
		t.Fatal(err)
	}

	prof := &store.AgentProfile{TenantID: tenantID, Name: "Reader", Slug: profile.Slug(tenantID, "Reader")}
	if err := st.AgentProfiles().Create(ctx, prof); err != nil {
		t.Fatal(err)
	}
	tools := make([]store.ProfileTool, 0, len(opts.ProfileTools))
	for _, name := range opts.ProfileTools {
		tools = append(tools, store.ProfileTool{AgentProfileID: prof.ID, ConnectorID: conn.ID, ToolName: name})
	}
	if err := st.AgentProfiles().SetTools(ctx, tenantID, prof.ID, tools); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.MCP.RequireProfile = opts.RequireProfile
	cfg.Capture.StoreBodies = opts.StoreBodies
	if opts.MaxResponseBytes > 0 {
		cfg.Capture.MaxResponseBytes = opts.MaxResponseBytes
	}
	if opts.MaxUpstreamStreams > 0 {
		cfg.MCP.MaxUpstreamStreamsPerSession = opts.MaxUpstreamStreams
	}
	if opts.MaxSubscriptions > 0 {
		cfg.MCP.MaxSubscriptionsPerSession = opts.MaxSubscriptions
	}
	if opts.MaxPendingServerRequests > 0 {
		cfg.MCP.MaxPendingServerRequestsPerSession = opts.MaxPendingServerRequests
	}
	// A test that fails while a relayed request is pending must not
	// hold the server's Close for the production default of 5m.
	cfg.MCP.ServerRequestTimeout = 10 * time.Second
	if opts.ServerRequestTimeout > 0 {
		cfg.MCP.ServerRequestTimeout = opts.ServerRequestTimeout
	}
	cfg.Service.Version = "test"
	if opts.RedisAddr != "" {
		cfg.Sessions.Store = "redis"
		cfg.Redis.Addr = opts.RedisAddr
	}

	f := &fixture{backend: backend, store: st, sink: &recordingSink{}, conn: conn, cfg: cfg}
	f.plane, f.server = f.newReplica(t)
	return f
}

// newReplica builds one more mcp replica over the fixture's store,
// backend and config, and serves it. With a shared session driver
// (RedisAddr) its routines run, cross-replica bridge included.
func (f *fixture) newReplica(t *testing.T) (*dataplane.Plane, *httptest.Server) {
	t.Helper()
	plane, err := dataplane.New(dataplane.Deps{
		Config:        f.cfg,
		Store:         f.store,
		Headers:       headers.NewRegistry(),
		Authenticator: staticAuth{},
		Sink:          f.sink,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	if f.cfg.Sessions.Store == "redis" {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for _, r := range plane.Routines {
			if err := r.Init(ctx); err != nil {
				cancel()
				t.Fatalf("routine %s Init: %v", r.Name(), err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = r.Run(ctx)
			}()
		}
		t.Cleanup(func() {
			for _, r := range plane.Routines {
				_ = r.Stop(context.Background())
			}
			cancel()
			wg.Wait()
		})
	}

	srv := httptest.NewServer(plane.Handler)
	t.Cleanup(srv.Close)
	return plane, srv
}

// call posts a JSON-RPC request and returns the HTTP response together
// with the decoded body (which is empty for a 204).
func (f *fixture) call(t *testing.T, req mcp.Request, headers map[string]string) (*http.Response, mcp.Response) {
	t.Helper()

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, f.server.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range headers {
		if v == "" {
			httpReq.Header.Del(k)
			continue
		}
		httpReq.Header.Set(k, v)
	}

	resp, err := f.server.Client().Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded mcp.Response
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("response body is not JSON-RPC: %v (%s)", err, raw)
		}
	}
	return resp, decoded
}

// initialize runs the handshake and returns the issued session id.
func (f *fixture) initialize(t *testing.T) string {
	t.Helper()
	return f.initializeWith(t, `{}`)
}

// initializeWith runs the handshake declaring the given client
// capabilities (a JSON object) and returns the issued session id.
func (f *fixture) initializeWith(t *testing.T, capabilities string) string {
	t.Helper()
	resp, decoded := f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":` + capabilities + `,"clientInfo":{"name":"test","version":"1"}}`),
	}, nil)

	if decoded.Error != nil {
		t.Fatalf("initialize failed: %+v", decoded.Error)
	}
	id := resp.Header.Get(mcp.HeaderSessionID)
	if id == "" {
		t.Fatal("initialize returned no Mcp-Session-Id header")
	}
	return id
}

func toolNames(t *testing.T, resp mcp.Response) []string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("tools/list failed: %+v", resp.Error)
	}
	var result mcp.ToolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// ---------------------------------------------------------------------------

func TestHealthIsOpen(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	resp, err := f.server.Client().Get(f.server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200 with no credential", resp.StatusCode)
	}
}

func TestUnauthenticatedRequestGets401AndJSONRPC32001(t *testing.T) {
	// The status code is the exception to "everything is a 200": there
	// is no JSON-RPC layer yet when a credential is refused. The BODY is
	// still JSON-RPC, because that is what an MCP client parses.
	f := newFixture(t, fixtureOptions{})

	resp, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing},
		map[string]string{"Authorization": ""})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeUnauthorized {
		t.Fatalf("error = %+v, want code %d", decoded.Error, mcp.ErrorCodeUnauthorized)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("a 401 must carry WWW-Authenticate")
	}
}

func TestABadCredentialIsIndistinguishableFromAMissingOne(t *testing.T) {
	// Otherwise a caller could enumerate which keys exist.
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing},
		map[string]string{"Authorization": "Bearer gk_wrong"})

	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeUnauthorized {
		t.Fatalf("error = %+v", decoded.Error)
	}
	if strings.Contains(strings.ToLower(decoded.Error.Message), "unknown") ||
		strings.Contains(strings.ToLower(decoded.Error.Message), "revoked") {
		t.Errorf("the 401 body leaks the failure reason: %q", decoded.Error.Message)
	}
}

func TestTheGatewayKeyHeaderAuthenticatesToo(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	resp, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing},
		map[string]string{"Authorization": "", "X-Gateway-Key": apiKey})

	if resp.StatusCode != http.StatusOK || decoded.Error != nil {
		t.Fatalf("status %d, error %+v", resp.StatusCode, decoded.Error)
	}
}

func TestMalformedJSONIsAParseErrorOverHTTP200(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/mcp", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: clients parse the body, not the status", resp.StatusCode)
	}
	var decoded mcp.Response
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeParseError {
		t.Fatalf("error = %+v, want -32700", decoded.Error)
	}
}

func TestAWrongJSONRPCVersionIsAnInvalidRequest(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, mcp.Request{JSONRPC: "1.0", ID: 1, Method: mcp.MethodPing}, nil)
	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeInvalidRequest {
		t.Fatalf("error = %+v, want -32600", decoded.Error)
	}
}

func TestAnUnknownMethodIsMethodNotFound(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: "tools/teleport"}, nil)
	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeMethodNotFound {
		t.Fatalf("error = %+v, want -32601", decoded.Error)
	}
}

func TestANotificationGets204AndNoBody(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	resp, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, Method: mcp.MethodInitialized}, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if decoded.JSONRPC != "" || decoded.Error != nil {
		t.Fatalf("a notification must produce no body, got %+v", decoded)
	}
}

func TestInitializeIssuesASessionAndAdvertisesListChanged(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	resp, decoded := f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`),
	}, nil)

	if decoded.Error != nil {
		t.Fatalf("initialize failed: %+v", decoded.Error)
	}
	if resp.Header.Get(mcp.HeaderSessionID) == "" {
		t.Fatal("no Mcp-Session-Id was issued")
	}

	var result mcp.InitializeResult
	if err := json.Unmarshal(decoded.Result, &result); err != nil {
		t.Fatal(err)
	}
	// A client that sees an empty capability object skips tools/list
	// entirely, which would hide every tool behind a cold-start race.
	if result.Capabilities.Tools == nil || !result.Capabilities.Tools.ListChanged {
		t.Fatalf("capabilities = %+v, want tools.listChanged", result.Capabilities)
	}
	if f.backend.Calls(mcp.MethodInitialize) == 0 {
		t.Error("initialize did not fan out to the connector")
	}
	if f.backend.Calls(mcp.MethodInitialized) == 0 {
		t.Error("the post-handshake notification was not sent to the connector")
	}

	conn, err := f.store.Connectors().Get(context.Background(), tenantID, f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Status != "healthy" {
		t.Errorf("connector status = %q, want healthy after a successful handshake", conn.Status)
	}
	if conn.Capabilities["tools"] != true {
		t.Errorf("connector capabilities = %v, want tools recorded", conn.Capabilities)
	}
}

func TestAnUnknownSessionIDIsRejectedWith32000(t *testing.T) {
	// A client that thinks it holds a session must be told the session
	// is gone, not silently served statelessly.
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList},
		map[string]string{mcp.HeaderSessionID: "never-issued"})

	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeSessionNotFound {
		t.Fatalf("error = %+v, want -32000", decoded.Error)
	}
}

func TestAnIssuedSessionIDIsAccepted(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	sessionID := f.initialize(t)

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList},
		map[string]string{mcp.HeaderSessionID: sessionID})

	if names := toolNames(t, decoded); len(names) != 2 {
		t.Fatalf("tools = %v, want both", names)
	}
}

func TestToolsListWithoutASessionWorks(t *testing.T) {
	// A stateless client that never calls initialize must still work:
	// the gateway does the backend handshake on its behalf.
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList}, nil)

	names := toolNames(t, decoded)
	if len(names) != 2 || names[0] != "alpha__echo" {
		t.Fatalf("tools = %v, want the connector-prefixed pair", names)
	}
	if f.backend.Calls(mcp.MethodInitialize) == 0 {
		t.Error("the lazy backend handshake did not happen")
	}
}

func TestToolsListIsIntersectedWithTheProfile(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList},
		map[string]string{profile.Header: "Reader"})

	names := toolNames(t, decoded)
	if len(names) != 1 || names[0] != "alpha__echo" {
		t.Fatalf("tools = %v, want exactly [alpha__echo]", names)
	}
}

func TestAnUnknownProfileYieldsAnEmptyList(t *testing.T) {
	// The gateway's predecessor fell back to every tool in the tenant.
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList},
		map[string]string{profile.Header: "No Such Profile"})

	if names := toolNames(t, decoded); len(names) != 0 {
		t.Fatalf("tools = %v, want []", names)
	}
	// It must also be [] on the wire, not null.
	if !bytes.Contains(decoded.Result, []byte(`"tools":[]`)) {
		t.Errorf("result = %s, want an empty array", decoded.Result)
	}
}

func TestToolsCallOnAGrantedToolReachesTheBackend(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})

	resp, decoded := f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 5, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"hi"}}`),
	}, map[string]string{profile.Header: "Reader"})

	if decoded.Error != nil {
		t.Fatalf("tools/call failed: %+v", decoded.Error)
	}
	var result mcp.ToolsCallResult
	if err := json.Unmarshal(decoded.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) == 0 {
		t.Fatal("tools/call returned no content")
	}
	if got := resp.Header.Get("X-Connector-ID"); got != f.conn.ID {
		t.Errorf("X-Connector-ID = %q, want %q", got, f.conn.ID)
	}
	// The backend is called with its OWN name, not the prefixed one.
	if f.backend.ToolArgs("echo") == nil {
		t.Error("the backend was not called with the unprefixed tool name")
	}
	if got := f.backend.LastHeaders().Get("X-Tenant-Id"); got != tenantID {
		t.Errorf("backend saw X-Tenant-Id = %q, want %q", got, tenantID)
	}
}

func TestToolsCallOnAnUngrantedToolIsDeniedWith32003(t *testing.T) {
	// Filtering a list an agent can ignore is a suggestion; denying the
	// call is the control.
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})

	_, decoded := f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 6, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__secret","arguments":{}}`),
	}, map[string]string{profile.Header: "Reader"})

	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("error = %+v, want -32003", decoded.Error)
	}
	if f.backend.ToolArgs("secret") != nil {
		t.Fatal("a denied tool still reached the backend")
	}
}

func TestAnUnknownProfileDeniesEveryToolsCall(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})

	_, decoded := f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 7, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"hi"}}`),
	}, map[string]string{profile.Header: "Typo"})

	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("error = %+v, want -32003", decoded.Error)
	}
}

func TestRequireProfileRejectsARequestWithNoProfileHeader(t *testing.T) {
	f := newFixture(t, fixtureOptions{RequireProfile: true, ProfileTools: []string{"echo"}})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList}, nil)
	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("error = %+v, want -32003", decoded.Error)
	}

	// With the header present it works as usual.
	_, decoded = f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList},
		map[string]string{profile.Header: "Reader"})
	if names := toolNames(t, decoded); len(names) != 1 {
		t.Fatalf("tools = %v, want one", names)
	}
}

func TestToolArgumentOverridesAreStampedAndScrubbed(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		ConnectorMetadata: map[string]any{
			"tool_arg_overrides": map[string]any{"message": "gateway-owned"},
		},
	})

	// The override is removed from the advertised schema...
	_, listed := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList}, nil)
	var listResult mcp.ToolsListResult
	if err := json.Unmarshal(listed.Result, &listResult); err != nil {
		t.Fatal(err)
	}
	for _, tool := range listResult.Tools {
		if _, present := tool.InputSchema.Properties["message"]; present {
			t.Errorf("tool %q still advertises the overridden argument", tool.Name)
		}
	}

	// ...and stamped over whatever the caller supplied.
	_, decoded := f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"caller-supplied"}}`),
	}, nil)
	if decoded.Error != nil {
		t.Fatalf("tools/call failed: %+v", decoded.Error)
	}
	if got := f.backend.ToolArgs("echo")["message"]; got != "gateway-owned" {
		t.Fatalf("backend saw message = %v, want the connector's override", got)
	}
}

func TestPingNeedsNeitherSessionNorProfile(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing}, nil)
	if decoded.Error != nil {
		t.Fatalf("ping failed: %+v", decoded.Error)
	}
}

func TestAccessLogRecordsEveryRequestIncludingRejections(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})

	f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing}, nil)
	f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__secret","arguments":{}}`),
	}, map[string]string{profile.Header: "Reader"})
	f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 3, Method: mcp.MethodPing},
		map[string]string{"Authorization": ""})

	records := f.sink.all()
	if len(records) != 3 {
		t.Fatalf("got %d access-log records, want 3", len(records))
	}

	ping := records[0]
	if ping.Method != mcp.MethodPing || ping.JSONRPCID != "1" || ping.TenantID != tenantID || ping.KeyID != "key-1" {
		t.Errorf("ping record = %+v", ping)
	}
	if ping.RequestID == "" || ping.TraceID == "" || ping.CorrelationID == "" {
		t.Errorf("ping record is missing correlation handles: %+v", ping)
	}
	if ping.BytesIn == 0 || ping.Bytes == 0 {
		t.Errorf("ping record has no byte counts: in=%d out=%d", ping.BytesIn, ping.Bytes)
	}

	denied := records[1]
	if denied.ErrorCode != "-32003" {
		t.Errorf("denied record error_code = %q, want -32003", denied.ErrorCode)
	}
	if denied.ToolName != "alpha__secret" || denied.ConnectorID != f.conn.ID {
		t.Errorf("denied record did not attribute the tool/connector: %+v", denied)
	}

	// A burst of 401s is exactly what an operator needs the log for.
	rejected := records[2]
	if rejected.StatusCode != http.StatusUnauthorized {
		t.Errorf("rejected record status = %d, want 401", rejected.StatusCode)
	}
	if rejected.TenantID != "" {
		t.Errorf("a rejected request has no tenant, got %q", rejected.TenantID)
	}
}

func TestAccessLogCapturesBothBodiesAndClientSessionID(t *testing.T) {
	f := newFixture(t, fixtureOptions{StoreBodies: true})

	f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing},
		map[string]string{"X-Session-Id": "s-1"})

	records := f.sink.all()
	if len(records) != 1 {
		t.Fatalf("got %d access-log records, want 1", len(records))
	}
	rec := records[0]
	if !bytes.Contains(rec.RequestBody, []byte(`"ping"`)) {
		t.Errorf("request body = %q", rec.RequestBody)
	}
	if !bytes.Contains(rec.ResponseBody, []byte(`"jsonrpc"`)) {
		t.Errorf("response body = %q", rec.ResponseBody)
	}
	// No Mcp-Session-Id was ever negotiated for this call (a bare ping, no
	// initialize), so SessionID is empty; the caller-claimed X-Session-Id
	// is recorded only as ClientSessionID -- it is never promoted to
	// SessionID (see TestAccessLogSessionIDIsAlwaysNegotiated).
	if rec.SessionID != "" {
		t.Errorf("session id = %q, want empty (no negotiated session)", rec.SessionID)
	}
	if rec.ClientSessionID != "s-1" {
		t.Errorf("client session id = %q, want s-1", rec.ClientSessionID)
	}
	if rec.Headers["X-Session-Id"] != "s-1" {
		t.Errorf("captured X-Session-Id = %q, want the real value", rec.Headers["X-Session-Id"])
	}
	if v := rec.Headers["Authorization"]; v != "****" {
		t.Errorf("captured Authorization = %q, want the masked marker", v)
	}
}

func TestAccessLogTruncatesResponseBodyAtCap(t *testing.T) {
	f := newFixture(t, fixtureOptions{StoreBodies: true, MaxResponseBytes: 8})

	f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing}, nil)

	rec := f.sink.all()[0]
	if len(rec.ResponseBody) != 8 || !rec.Truncated {
		t.Errorf("response body = %q (truncated=%v), want 8 bytes and truncated", rec.ResponseBody, rec.Truncated)
	}
}

func TestAccessLogOmitsBodiesWhenCaptureOff(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing}, nil)

	rec := f.sink.all()[0]
	if rec.RequestBody != nil || rec.ResponseBody != nil || rec.Headers != nil {
		t.Errorf("capture off but got request=%q response=%q headers=%v", rec.RequestBody, rec.ResponseBody, rec.Headers)
	}
}

// TestAccessLogSessionIDIsAlwaysNegotiated is the regression test for the
// session-timeline-poisoning fix: SessionID must always be the
// gateway-negotiated Mcp-Session-Id, whatever a caller sends as
// X-Session-Id/X-Claude-Code-Session-Id -- a caller-controlled header must
// never be able to write its calls into another session's timeline. The
// header's value is still recorded, but only as ClientSessionID.
func TestAccessLogSessionIDIsAlwaysNegotiated(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	sid := f.initialize(t)
	list := mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList}

	for _, tc := range []struct {
		name       string
		headers    map[string]string
		wantClient string
	}{
		{"no client session header", map[string]string{mcp.HeaderSessionID: sid}, ""},
		{"claude code session header", map[string]string{mcp.HeaderSessionID: sid, "X-Claude-Code-Session-Id": "cc-1"}, "cc-1"},
		{"x-session-id wins over claude code header", map[string]string{mcp.HeaderSessionID: sid, "X-Claude-Code-Session-Id": "cc-1", "X-Session-Id": "s-1"}, "s-1"},
		{"attack: x-session-id claims another session's id", map[string]string{mcp.HeaderSessionID: sid, "X-Session-Id": "someone-elses-session"}, "someone-elses-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.sink.all())
			f.call(t, list, tc.headers)
			records := f.sink.all()
			if len(records) != before+1 {
				t.Fatalf("got %d new records, want 1", len(records)-before)
			}
			rec := records[len(records)-1]
			if rec.SessionID != sid {
				t.Errorf("session id = %q, want the negotiated %q regardless of client headers", rec.SessionID, sid)
			}
			if rec.ClientSessionID != tc.wantClient {
				t.Errorf("client session id = %q, want %q", rec.ClientSessionID, tc.wantClient)
			}
		})
	}
}

// TestAccessLogClientSessionIDIsCapped guards the ClientSessionID length
// cap (sanitizeSessionTag, see also the whitebox TestSanitizeSessionTag):
// an oversized header can't bloat a log row. Control characters can't be
// exercised here -- net/http's own client refuses to send them as a
// header value -- see TestSanitizeSessionTag for that case.
func TestAccessLogClientSessionIDIsCapped(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	oversized := strings.Repeat("x", 200)
	f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPing},
		map[string]string{"X-Session-Id": oversized})

	rec := f.sink.all()[0]
	if len(rec.ClientSessionID) != 128 {
		t.Errorf("client session id length = %d, want exactly 128 (capped)", len(rec.ClientSessionID))
	}
}

func TestInboundTraceparentIsContinuedAndPropagated(t *testing.T) {
	const inbound = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	f := newFixture(t, fixtureOptions{})

	f.call(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"hi"}}`),
	}, map[string]string{"traceparent": inbound})

	records := f.sink.all()
	if len(records) == 0 || records[0].TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("the access log did not continue the inbound trace: %+v", records)
	}
	backendTP := f.backend.LastHeaders().Get("traceparent")
	if !strings.Contains(backendTP, "0af7651916cd43dd8448eb211c80319c") {
		t.Fatalf("backend saw traceparent %q, want the inbound trace id", backendTP)
	}
}

func TestSSEStreamSendsKeepAlivesAndNotifications(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/mcp/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// A refresh pushes notifications/tools/list_changed to every stream
	// open for the tenant. Signal repeatedly until the read below
	// returns, since the handler subscribes asynchronously and a
	// notification is an edge, not a queue.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				f.plane.NotifyToolsListChanged(tenantID)
			}
		}
	}()

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if !strings.Contains(string(buf[:n]), mcp.NotificationToolsListChanged) {
		t.Fatalf("stream frame = %q, want a tools/list_changed notification", buf[:n])
	}
}

func TestStreamRequiresAuthentication(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	resp, err := f.server.Client().Get(f.server.URL + "/mcp/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
