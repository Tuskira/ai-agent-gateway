//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/orchestrator"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// prefixObserver records what the MCP plane reports about each call: the
// access log row of every request and the tool-call event of every routed
// tools/call.
type prefixObserver struct {
	mu    sync.Mutex
	logs  []*sink.AccessLog
	calls []orchestrator.ToolCall
}

func (o *prefixObserver) WriteAccess(l *sink.AccessLog) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.logs = append(o.logs, l)
}
func (o *prefixObserver) WriteLLMCall(*sink.LLMCall) {}
func (o *prefixObserver) Close() error               { return nil }
func (o *prefixObserver) onToolCall(_ context.Context, c orchestrator.ToolCall) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, c)
}

// accessFor returns the access rows for a method whose ToolName is name,
// waiting briefly because the plane writes them as it finishes a request.
func (o *prefixObserver) accessFor(t *testing.T, method, name string) []*sink.AccessLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		o.mu.Lock()
		var out []*sink.AccessLog
		for _, l := range o.logs {
			if l.Method == method && l.ToolName == name {
				out = append(out, l)
			}
		}
		o.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (o *prefixObserver) toolCallsFor(server string) []orchestrator.ToolCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []orchestrator.ToolCall
	for _, c := range o.calls {
		if c.Server == server {
			out = append(out, c)
		}
	}
	return out
}

// newPrefixHarness is a gateway over a real Postgres schema with its MCP
// plane (tool cache ON, so a rename has to go through cache invalidation)
// and control-plane API, an admin key and a recording log sink. No
// connector exists yet: each test registers its own through the API.
func newPrefixHarness(t *testing.T) (*harness, *prefixObserver) {
	t.Helper()
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the end-to-end tests")
	}
	ctx := context.Background()
	st := newIsolatedStore(t, rawURL)

	tenant := &store.Tenant{Slug: "pfx-" + uuid.NewString()[:8], Name: "Prefix Tenant"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.APIKeys().Create(ctx, &store.APIKey{TenantID: tenant.ID, Name: "e2e", Role: "admin", KeyHash: hash, KeyPrefix: prefix, CreatedBy: "e2e"}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	obs := &prefixObserver{}
	registry := dpheaders.NewRegistry()
	cfg := config.Default()
	cfg.Service.Version = "e2e"
	plane, err := dataplane.New(dataplane.Deps{
		Config:        cfg,
		Store:         st,
		Headers:       registry,
		Authenticator: apikey.New(st.APIKeys(), apikey.Options{}),
		Sink:          obs,
		OnToolCall:    obs.onToolCall,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build mcp plane: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	serve := func(h http.Handler) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h, ReadTimeout: 30 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(c)
		})
		return ln.Addr().String()
	}
	apiHandler := api.NewRouter(api.Deps{
		ServiceVersion: "e2e",
		Authenticator:  apikey.New(st.APIKeys(), apikey.Options{}),
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:          st,
		Headers:        registry,
		ConnectorOps:   plane.ConnectorOps,
		CacheOps:       plane.CacheOps,
		ProfileOps:     plane.ProfileOps,
	})
	h := &harness{
		store:      st,
		tenant:     tenant,
		adminKey:   plaintext,
		baseURL:    "http://" + serve(plane.Handler),
		apiBaseURL: "http://" + serve(apiHandler),
	}
	return h, obs
}

func (h *harness) apiRaw(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.apiBaseURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

type connView struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	ToolPrefix string `json:"tool_prefix"`
}

// addConnector registers a connector through the API and returns its view.
func (h *harness) addConnector(t *testing.T, name, endpoint string, prefix *string) connView {
	t.Helper()
	body := map[string]any{"name": name, "endpoint": endpoint}
	if prefix != nil {
		body["tool_prefix"] = *prefix
	}
	code, raw := h.apiRaw(t, http.MethodPost, "/api/v1/connectors", body)
	if code != http.StatusCreated {
		t.Fatalf("create connector %q: %d %s", name, code, raw)
	}
	var v connView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func ptr(s string) *string { return &s }

func (h *harness) initSession(t *testing.T) string {
	t.Helper()
	resp, decoded, raw := h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
	}, rpcOptions{})
	if decoded.Error != nil {
		t.Fatalf("initialize failed: %s", raw)
	}
	return resp.Header.Get(mcp.HeaderSessionID)
}

// toolNames lists tools/list names, following the plane's paging.
func (h *harness) toolNames(t *testing.T, opts rpcOptions) []string {
	t.Helper()
	_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList}, opts)
	if decoded.Error != nil {
		t.Fatalf("tools/list failed: %s", raw)
	}
	var res mcp.ToolsListResult
	if err := json.Unmarshal(decoded.Result, &res); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func (h *harness) callTool(t *testing.T, name string, args map[string]any, opts rpcOptions) mcp.Response {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	_, decoded, _ := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 3, Method: mcp.MethodToolsCall, Params: params}, opts)
	return decoded
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func hasNamePrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func toolText(t *testing.T, r mcp.Response) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("tools/call failed: %+v", r.Error)
	}
	var res mcp.ToolsCallResult
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// prefixBackend says what a backend under test offers: its endpoint, a
// tool that echoes "message", and a tool the profile will not grant.
type prefixBackend struct {
	endpoint   string
	ungranted  string
	ungrantedA map[string]any
	// counts, when non-nil, reports how many tools/call requests the
	// backend itself has served.
	toolCalls func() int
}

// TestToolPrefixEndToEnd is the connector tool_prefix feature, driven the
// way a client does it: over real sockets, through the control-plane API,
// against a real Postgres and a real MCP backend. Connector "plain" has no
// prefix and must behave exactly as before; connector "pfx" has one.
func TestToolPrefixEndToEnd(t *testing.T) {
	backend := dptest.NewBackend(dptest.BackendOptions{
		Tools: []mcp.Tool{
			{Name: "echo", Description: "Echo a message back", InputSchema: dptest.ObjectSchema([]string{"message"}, "message")},
			{Name: "danger", Description: "Should stay out of reach", InputSchema: dptest.ObjectSchema(nil, "target")},
		},
		Handlers: map[string]dptest.ToolFunc{
			"echo": func(args map[string]any) (mcp.ToolsCallResult, error) {
				msg, _ := args["message"].(string)
				return mcp.ToolsCallResult{Content: []mcp.Content{mcp.TextContent("echo: " + msg)}}, nil
			},
		},
		Prompts:           dptest.SamplePrompts(),
		Resources:         dptest.SampleResources(),
		ResourceTemplates: dptest.SampleResourceTemplates(),
		RequireSession:    true,
	})
	t.Cleanup(backend.Close)

	runToolPrefixScenario(t, prefixBackend{
		endpoint:   backend.URL,
		ungranted:  "danger",
		ungrantedA: map[string]any{"target": "x"},
		toolCalls:  func() int { return backend.Calls(mcp.MethodToolsCall) },
	})
}

// TestToolPrefixRealMCPServer runs the same scenario against the reference
// "everything" MCP server (@modelcontextprotocol/server-everything), the
// one examples/01-quickstart starts. Set GATEWAY_TEST_MCP_EVERYTHING_URL to
// its streamable-HTTP endpoint, for example http://127.0.0.1:18975/mcp.
func TestToolPrefixRealMCPServer(t *testing.T) {
	endpoint := os.Getenv("GATEWAY_TEST_MCP_EVERYTHING_URL")
	if endpoint == "" {
		t.Skip("GATEWAY_TEST_MCP_EVERYTHING_URL not set; skipping the real-MCP-server tool_prefix test")
	}
	runToolPrefixScenario(t, prefixBackend{endpoint: endpoint, ungranted: "get-sum", ungrantedA: map[string]any{"a": 1, "b": 2}})
}

func runToolPrefixScenario(t *testing.T, be prefixBackend) {
	h, obs := newPrefixHarness(t)
	ctx := context.Background()

	const vtPrefix = "virustotal.threat_intel"
	plain := h.addConnector(t, "plain", be.endpoint, nil)
	pfx := h.addConnector(t, "pfx", be.endpoint, ptr(vtPrefix))
	if plain.ToolPrefix != "" || pfx.ToolPrefix != vtPrefix {
		t.Fatalf("views: plain %+v, pfx %+v", plain, pfx)
	}
	var got connView
	code, raw := h.apiRaw(t, http.MethodGet, "/api/v1/connectors/"+pfx.ID, nil)
	if code != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.ToolPrefix != vtPrefix {
		t.Fatalf("GET connector: %d %s", code, raw)
	}

	// A profile created and granted through the API the way the console does
	// it: tool_name is the backend's own (bare) name and tool_namespace is
	// the qualifier the cached tool list showed. The list is populated by a
	// discovery first, as it is once a connector has been used.
	for _, c := range []connView{plain, pfx} {
		if code, raw := h.apiRaw(t, http.MethodPost, "/api/v1/connectors/"+c.ID+"/discover", nil); code != http.StatusOK {
			t.Fatalf("discover %s: %d %s", c.Slug, code, raw)
		}
	}
	var profView struct {
		ID string `json:"id"`
	}
	code, raw = h.apiRaw(t, http.MethodPost, "/api/v1/profiles", map[string]any{"name": "Reader"})
	if code != http.StatusCreated || json.Unmarshal(raw, &profView) != nil {
		t.Fatalf("create profile: %d %s", code, raw)
	}
	var grants []map[string]any
	for _, c := range []connView{plain, pfx} {
		var page struct {
			Items []struct {
				ToolName      string `json:"tool_name"`
				ToolNamespace string `json:"tool_namespace"`
			} `json:"items"`
		}
		code, raw := h.apiRaw(t, http.MethodGet, "/api/v1/connectors/"+c.ID+"/tools", nil)
		if code != http.StatusOK || json.Unmarshal(raw, &page) != nil {
			t.Fatalf("list tools of %s: %d %s", c.Slug, code, raw)
		}
		for _, tl := range page.Items {
			if tl.ToolName == "echo" {
				grants = append(grants, map[string]any{"connector_id": c.ID, "tool_name": tl.ToolName, "tool_namespace": tl.ToolNamespace})
			}
		}
	}
	if len(grants) != 2 {
		t.Fatalf("expected the echo tool on both connectors in the cache, got %v", grants)
	}
	if code, raw := h.apiRaw(t, http.MethodPut, "/api/v1/profiles/"+profView.ID+"/tools", map[string]any{"tools": grants}); code != http.StatusOK {
		t.Fatalf("set profile tools: %d %s", code, raw)
	}
	reader := func(sess string) rpcOptions { return rpcOptions{SessionID: sess, ProfileName: "Reader"} }
	sess := h.initSession(t)

	t.Run("tools/list advertises the prefix and leaves the other connector alone", func(t *testing.T) {
		names := h.toolNames(t, rpcOptions{SessionID: sess})
		for _, want := range []string{"plain__echo", vtPrefix + "__echo"} {
			if !contains(names, want) {
				t.Errorf("tools/list lacks %q: %v", want, names)
			}
		}
		if hasNamePrefix(names, "pfx__") {
			t.Errorf("tools/list still advertises the slug of the prefixed connector: %v", names)
		}
	})

	t.Run("profile grants by bare name work under the prefix", func(t *testing.T) {
		names := h.toolNames(t, reader(sess))
		if len(names) != 2 || !contains(names, "plain__echo") || !contains(names, vtPrefix+"__echo") {
			t.Fatalf("profile tools/list = %v, want exactly plain__echo and %s__echo", names, vtPrefix)
		}
		r := h.callTool(t, vtPrefix+"__"+be.ungranted, be.ungrantedA, reader(sess))
		if r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed {
			t.Fatalf("an ungranted tool under the prefix: %+v, want -32003", r.Error)
		}
	})

	t.Run("tools/call routes by prefix, and by slug as before for the plain connector", func(t *testing.T) {
		before := 0
		if be.toolCalls != nil {
			before = be.toolCalls()
		}
		for _, name := range []string{vtPrefix + "__echo", "plain__echo"} {
			if txt := toolText(t, h.callTool(t, name, map[string]any{"message": "hi " + name}, reader(sess))); !strings.Contains(strings.ToLower(txt), "hi "+name) {
				t.Errorf("%s -> %q", name, txt)
			}
		}
		// Capitals in the call are matched like the prefix's uniqueness.
		if txt := toolText(t, h.callTool(t, "VirusTotal.Threat_Intel__echo", map[string]any{"message": "cased"}, reader(sess))); !strings.Contains(txt, "cased") {
			t.Errorf("case-insensitive prefix call -> %q", txt)
		}
		if be.toolCalls != nil && be.toolCalls()-before != 3 {
			t.Errorf("backend served %d calls, want 3", be.toolCalls()-before)
		}
		// The prefixed connector's slug keeps routing too (order: prefix, slug, name).
		if txt := toolText(t, h.callTool(t, "pfx__echo", map[string]any{"message": "by slug"}, rpcOptions{SessionID: sess})); !strings.Contains(txt, "by slug") {
			t.Errorf("pfx__echo -> %q", txt)
		}
		if r := h.callTool(t, "nope__echo", map[string]any{"message": "x"}, rpcOptions{SessionID: sess}); r.Error == nil {
			t.Errorf("unknown connector call succeeded")
		}
	})

	t.Run("access logs and tool-call events carry the prefix", func(t *testing.T) {
		rows := obs.accessFor(t, "tools/call", vtPrefix+"__echo")
		if len(rows) == 0 {
			t.Fatalf("no tools/call access row named %s__echo", vtPrefix)
		}
		for _, r := range rows {
			if r.ConnectorID != pfx.ID {
				t.Errorf("access row connector = %q, want %q", r.ConnectorID, pfx.ID)
			}
		}
		if rows := obs.accessFor(t, "tools/call", "plain__echo"); len(rows) == 0 || rows[0].ConnectorID != plain.ID {
			t.Errorf("plain connector access row: %+v", rows)
		}
		hasEvent := func(server, connID, tool, outcome string) bool {
			for _, e := range obs.toolCallsFor(server) {
				if e.ConnectorID == connID && e.Tool == tool && e.Outcome == outcome {
					return true
				}
			}
			return false
		}
		if !hasEvent(vtPrefix, pfx.ID, "echo", orchestrator.ToolCallAllowed) || !hasEvent(vtPrefix, pfx.ID, be.ungranted, orchestrator.ToolCallDenied) {
			t.Errorf("tool-call events for the prefix: %+v", obs.toolCallsFor(vtPrefix))
		}
		if !hasEvent("plain", plain.ID, "echo", orchestrator.ToolCallAllowed) {
			t.Errorf("tool-call events for the plain connector (named by slug, as before): %+v", obs.toolCallsFor("plain"))
		}
		// A call that reaches the prefixed connector by its slug is still
		// logged under the name the connector is advertised by.
		if evs := obs.toolCallsFor("pfx"); len(evs) != 0 {
			t.Errorf("tool-call events under the slug of the prefixed connector: %d, want 0", len(evs))
		}
	})

	t.Run("prompts and resources use the prefix", func(t *testing.T) {
		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 20, Method: mcp.MethodPromptsList}, reader(sess))
		var prompts mcp.PromptsListResult
		if decoded.Error != nil || json.Unmarshal(decoded.Result, &prompts) != nil {
			t.Fatalf("prompts/list: %s", raw)
		}
		var pick string
		for _, p := range prompts.Prompts {
			if hasNamePrefix([]string{p.Name}, "pfx__") {
				t.Errorf("prompt %q is under the slug", p.Name)
			}
			required := false
			for _, a := range p.Arguments {
				required = required || a.Required
			}
			if pick == "" && strings.HasPrefix(p.Name, vtPrefix+"__") && !required {
				pick = p.Name
			}
		}
		if pick == "" {
			t.Fatalf("no prompt without required arguments under %s__: %s", vtPrefix, raw)
		}
		params, _ := json.Marshal(map[string]any{"name": pick})
		if _, decoded, raw = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 21, Method: mcp.MethodPromptsGet, Params: params}, reader(sess)); decoded.Error != nil {
			t.Fatalf("prompts/get %s: %s", pick, raw)
		}

		_, decoded, raw = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 22, Method: mcp.MethodResourcesList}, reader(sess))
		var resources mcp.ResourcesListResult
		if decoded.Error != nil || json.Unmarshal(decoded.Result, &resources) != nil || len(resources.Resources) == 0 {
			t.Fatalf("resources/list: %s", raw)
		}
		var uri string
		for _, r := range resources.Resources {
			if strings.HasPrefix(r.URI, "gw://pfx/") {
				t.Errorf("resource %q is under the slug", r.URI)
			}
			if uri == "" && strings.HasPrefix(r.URI, "gw://"+vtPrefix+"/") {
				uri = r.URI
			}
		}
		if uri == "" {
			t.Fatalf("no resource under gw://%s/: %s", vtPrefix, raw)
		}
		params, _ = json.Marshal(mcp.ResourcesReadParams{URI: uri})
		if _, decoded, raw = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 23, Method: mcp.MethodResourcesRead, Params: params}, reader(sess)); decoded.Error != nil {
			t.Fatalf("resources/read %s: %s", uri, raw)
		}
	})

	t.Run("changing the prefix refreshes the cached list and tells open streams", func(t *testing.T) {
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		frames := h.sessionStream(t, sctx, sess)
		// Drain whatever the stream already carries before the change.
	drain:
		for {
			select {
			case <-frames:
			case <-time.After(300 * time.Millisecond):
				break drain
			}
		}

		code, raw := h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+pfx.ID, map[string]any{
			"name": "pfx", "endpoint": be.endpoint, "tool_prefix": "renamed.feed",
		})
		if code != http.StatusOK || !strings.Contains(string(raw), `"tool_prefix":"renamed.feed"`) {
			t.Fatalf("PUT new prefix: %d %s", code, raw)
		}
		// The push comes from the PUT itself: wait for it before any list
		// call that could cause one of its own.
		var sawChanged bool
		deadline := time.After(5 * time.Second)
		for !sawChanged {
			select {
			case f := <-frames:
				sawChanged = strings.Contains(f, mcp.NotificationToolsListChanged)
			case <-deadline:
				t.Fatal("no notifications/tools/list_changed reached the open stream after the prefix change")
			}
		}

		names := h.toolNames(t, rpcOptions{SessionID: sess})
		if !contains(names, "renamed.feed__echo") || hasNamePrefix(names, vtPrefix+"__") {
			t.Fatalf("tools/list after the change = %v", names)
		}
		if r := h.callTool(t, vtPrefix+"__echo", map[string]any{"message": "old"}, rpcOptions{SessionID: sess}); r.Error == nil {
			t.Errorf("the old prefix still routes")
		}
		// The profile's grants (stored with the old namespace) still apply.
		if rn := h.toolNames(t, reader(sess)); len(rn) != 2 || !contains(rn, "renamed.feed__echo") || !contains(rn, "plain__echo") {
			t.Errorf("profile tools/list after the change = %v", rn)
		}
		if txt := toolText(t, h.callTool(t, "renamed.feed__echo", map[string]any{"message": "new"}, reader(sess))); !strings.Contains(txt, "new") {
			t.Errorf("new prefix call -> %q", txt)
		}
		// A PUT that leaves tool_prefix out keeps it; "" clears it and the slug is advertised again.
		if code, raw = h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+pfx.ID, map[string]any{"name": "pfx", "endpoint": be.endpoint}); code != http.StatusOK ||
			!strings.Contains(string(raw), `"tool_prefix":"renamed.feed"`) {
			t.Fatalf("PUT without tool_prefix: %d %s", code, raw)
		}
		if code, raw = h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+pfx.ID, map[string]any{"name": "pfx", "endpoint": be.endpoint, "tool_prefix": ""}); code != http.StatusOK ||
			!strings.Contains(string(raw), `"tool_prefix":""`) {
			t.Fatalf("PUT clearing tool_prefix: %d %s", code, raw)
		}
		names = h.toolNames(t, rpcOptions{SessionID: sess})
		if !contains(names, "pfx__echo") || hasNamePrefix(names, "renamed.feed__") {
			t.Fatalf("tools/list after clearing = %v", names)
		}
	})

	t.Run("collisions are 409 and bad prefixes 400", func(t *testing.T) {
		if code, raw := h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+pfx.ID, map[string]any{"name": "pfx", "endpoint": be.endpoint, "tool_prefix": "Taken.Prefix"}); code != http.StatusOK {
			t.Fatalf("set Taken.Prefix: %d %s", code, raw)
		}
		create := func(name, prefix string) int {
			code, _ := h.apiRaw(t, http.MethodPost, "/api/v1/connectors", map[string]any{"name": name, "endpoint": be.endpoint, "tool_prefix": prefix})
			return code
		}
		for _, c := range []struct {
			prefix string
			want   int
		}{
			{"Taken.Prefix", http.StatusConflict},
			{"taken.prefix", http.StatusConflict},
			{"TAKEN.PREFIX", http.StatusConflict},
			{"plain", http.StatusConflict}, // another connector's slug
			{"PLAIN", http.StatusConflict}, // ... that differs only in case
			{"gateway", http.StatusBadRequest},
			{"Gateway", http.StatusBadRequest},
			{"a b", http.StatusBadRequest},
			{"a__b", http.StatusBadRequest},
			{"a/b", http.StatusBadRequest},
			{strings.Repeat("x", 101), http.StatusBadRequest},
		} {
			if got := create("c-"+uuid.NewString()[:6], c.prefix); got != c.want {
				t.Errorf("create with tool_prefix %q: %d, want %d", c.prefix, got, c.want)
			}
		}
		// The reverse: a slug equal to another connector's prefix (any
		// case), through POST and through PUT.
		if code, raw := h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+pfx.ID, map[string]any{"name": "pfx", "endpoint": be.endpoint, "tool_prefix": "Reverse-Slug"}); code != http.StatusOK {
			t.Fatalf("set Reverse-Slug: %d %s", code, raw)
		}
		if code, _ := h.apiRaw(t, http.MethodPost, "/api/v1/connectors", map[string]any{"name": "rev", "slug": "reverse-slug", "endpoint": be.endpoint}); code != http.StatusConflict {
			t.Errorf("POST with a slug equal to another connector's prefix: %d, want 409", code)
		}
		other := h.addConnector(t, "other", be.endpoint, nil)
		if code, _ := h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+other.ID, map[string]any{"name": "other", "slug": "reverse-slug", "endpoint": be.endpoint}); code != http.StatusConflict {
			t.Errorf("PUT slug equal to another connector's prefix: %d, want 409", code)
		}
		if code, raw := h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+pfx.ID, map[string]any{"name": "pfx", "endpoint": be.endpoint, "tool_prefix": "Slug.Holder"}); code != http.StatusOK {
			t.Fatalf("set Slug.Holder: %d %s", code, raw)
		}
		// A PUT onto another connector's slug or prefix is a conflict as well.
		for _, p := range []string{"plain", "other"} {
			if code, _ := h.apiRaw(t, http.MethodPut, "/api/v1/connectors/"+pfx.ID, map[string]any{"name": "pfx", "endpoint": be.endpoint, "tool_prefix": p}); code != http.StatusConflict {
				t.Errorf("PUT tool_prefix %q: %d, want 409", p, code)
			}
		}
	})

	t.Run("two concurrent creates with one prefix: one 201, one 409", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			prefix := fmt.Sprintf("Race.%d.%s", round, uuid.NewString()[:6])
			codes := make([]int, 2)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range codes {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					b, _ := json.Marshal(map[string]any{"name": fmt.Sprintf("race-%d-%d", round, i), "endpoint": be.endpoint, "tool_prefix": prefix})
					req, _ := http.NewRequest(http.MethodPost, h.apiBaseURL+"/api/v1/connectors", bytes.NewReader(b))
					req.Header.Set("Authorization", "Bearer "+h.adminKey)
					req.Header.Set("Content-Type", "application/json")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						codes[i] = -1
						return
					}
					resp.Body.Close()
					codes[i] = resp.StatusCode
				}()
			}
			close(start)
			wg.Wait()
			created, conflicts := 0, 0
			for _, c := range codes {
				switch c {
				case http.StatusCreated:
					created++
				case http.StatusConflict:
					conflicts++
				}
			}
			if created != 1 || conflicts != 1 {
				t.Errorf("round %d prefix %q: codes %v, want one 201 and one 409", round, prefix, codes)
			}
		}
	})

	t.Run("a deleted connector frees its prefix", func(t *testing.T) {
		v := h.addConnector(t, "temp", be.endpoint, ptr("Temp.Prefix"))
		if code, raw := h.apiRaw(t, http.MethodDelete, "/api/v1/connectors/"+v.ID, nil); code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", code, raw)
		}
		h.addConnector(t, "temp2", be.endpoint, ptr("temp.prefix"))
	})
}
