package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

const headerEnvVar = "GW_TEST_HEADERS_REQUIRED_TOKEN"

// useHeaderClient swaps the harness client for one that resolves connector
// headers through a real registry and the real env provider, and gives the
// connector one header read from headerEnvVar.
func useHeaderClient(t *testing.T, h *harness, failOnUnresolved bool, meta map[string]any) {
	t.Helper()
	reg := dpheaders.NewRegistry()
	if err := reg.RegisterExternal(secrets.NewEnvProvider(true)); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl := client.New(client.Options{Headers: reg, FailOnUnresolvedHeaders: failOnUnresolved, Logger: logger})
	t.Cleanup(func() { _ = cl.Close() })
	h.orch.deps.Client = cl

	meta["headers"] = map[string]any{
		"Authorization": map[string]any{
			"type": "external", "provider": "env", "prefix": "Bearer ",
			"config": map[string]any{"var": headerEnvVar},
		},
	}
	h.conn.Metadata = meta
	if err := h.store.Connectors().Update(context.Background(), h.conn); err != nil {
		t.Fatal(err)
	}
}

func backendRequests(h *harness) int {
	return h.backend.Calls(mcp.MethodInitialize) + h.backend.Calls(mcp.MethodToolsCall) +
		h.backend.Calls(mcp.MethodInitialized)
}

func callAlphaSlow(h *harness, sess *session.Session) *mcp.Response {
	in := Request{JSONRPC: slowCall(7, `{"name":"alpha__slow","arguments":{"steps":0}}`), Principal: h.principal}
	if sess != nil {
		in.Session, in.Backends = sess, sess
	}
	return h.orch.Handle(h.ctx(), in).Response
}

// A connector with headers_required refuses a call whose header cannot be
// resolved with -32002 naming the header, and the backend receives nothing.
// The same call to a connector without the flag goes out without the header.
func TestHeadersRequired_RefusesUnresolvedHeader(t *testing.T) {
	_ = os.Unsetenv(headerEnvVar)

	for _, tc := range []struct {
		name       string
		failConfig bool
		meta       map[string]any
		wantRefuse bool
	}{
		{"connector flag", false, map[string]any{"headers_required": true}, true},
		{"config flag", true, map[string]any{}, true},
		{"flag false overridden by config", true, map[string]any{"headers_required": false}, true},
		{"default is skip", false, map[string]any{}, false},
		{"flag false is skip", false, map[string]any{"headers_required": false}, false},
		{"non-bool is not required", false, map[string]any{"headers_required": "true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			useHeaderClient(t, h, tc.failConfig, tc.meta)

			r := callAlphaSlow(h, nil)
			if tc.wantRefuse {
				if r.Error == nil || r.Error.Code != mcp.ErrorCodeToolExecution || r.Error.Message != "credential header Authorization unavailable" {
					t.Fatalf("response = %+v; want -32002 \"credential header Authorization unavailable\"", r)
				}
				if n := backendRequests(h); n != 0 {
					t.Errorf("backend received %d requests; want 0", n)
				}
				if got := h.backend.LastHeaders(); len(got) != 0 {
					t.Errorf("backend saw headers %v; want no request at all", got)
				}
				// Not the backend's fault: no unhealthy cool-down.
				c, err := h.store.Connectors().Get(context.Background(), testTenant, h.conn.ID)
				if err != nil {
					t.Fatal(err)
				}
				if c.Status == "unhealthy" {
					t.Errorf("connector was marked unhealthy by a header failure")
				}
				return
			}
			if r.Error != nil {
				t.Fatalf("call refused: %+v; want it forwarded without the header", r.Error)
			}
			if got := h.backend.LastHeaders().Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q; want it left off", got)
			}
		})
	}
}

// With the credential available the call succeeds and carries the header; if
// it then disappears mid-session (the credential service goes away) the next
// call is refused and nothing more reaches the backend.
func TestHeadersRequired_RefusesWhenCredentialDisappearsMidSession(t *testing.T) {
	t.Setenv(headerEnvVar, "tok-1")
	h := newHarness(t)
	useHeaderClient(t, h, false, map[string]any{"headers_required": true})
	sess := h.initialize(t)

	if r := callAlphaSlow(h, sess); r.Error != nil {
		t.Fatalf("call with the credential available: %+v", r.Error)
	}
	if got := h.backend.LastHeaders().Get("Authorization"); got != "Bearer tok-1" {
		t.Fatalf("Authorization = %q; want the resolved credential", got)
	}

	if err := os.Unsetenv(headerEnvVar); err != nil {
		t.Fatal(err)
	}
	before := backendRequests(h)
	r := callAlphaSlow(h, sess)
	if r.Error == nil || r.Error.Code != mcp.ErrorCodeToolExecution || r.Error.Message != "credential header Authorization unavailable" {
		t.Fatalf("response = %+v; want -32002", r)
	}
	if n := backendRequests(h) - before; n != 0 {
		t.Errorf("backend received %d requests after the credential vanished; want 0", n)
	}

	// The credential comes back: the connector works again at once (it was
	// not left in an unhealthy cool-down).
	t.Setenv(headerEnvVar, "tok-2")
	if r := callAlphaSlow(h, sess); r.Error != nil {
		t.Fatalf("call after recovery: %+v", r.Error)
	}
	if got := h.backend.LastHeaders().Get("Authorization"); got != "Bearer tok-2" {
		t.Errorf("Authorization = %q after recovery", got)
	}
}

// prompts/get and resources/read are refused the same way as tools/call,
// and do not mark the connector unhealthy.
func TestHeadersRequired_RefusesPromptsAndResources(t *testing.T) {
	_ = os.Unsetenv(headerEnvVar)
	h := newHarness(t)
	useHeaderClient(t, h, false, map[string]any{"headers_required": true})

	for name, req := range map[string]*mcp.Request{
		"prompts/get":    {JSONRPC: mcp.Version, ID: 5, Method: mcp.MethodPromptsGet, Params: json.RawMessage(`{"name":"alpha__p"}`)},
		"resources/read": {JSONRPC: mcp.Version, ID: 6, Method: mcp.MethodResourcesRead, Params: json.RawMessage(`{"uri":"gw://alpha/file:///x"}`)},
	} {
		r := h.orch.Handle(h.ctx(), Request{JSONRPC: req, Principal: h.principal}).Response
		if r == nil || r.Error == nil || r.Error.Code != mcp.ErrorCodeToolExecution || r.Error.Message != "credential header Authorization unavailable" {
			t.Errorf("%s: response = %+v; want -32002 credential header Authorization unavailable", name, r)
		}
	}
	if n := backendRequests(h); n != 0 {
		t.Errorf("backend received %d requests; want 0", n)
	}
	c, err := h.store.Connectors().Get(context.Background(), testTenant, h.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status == "unhealthy" {
		t.Error("connector was marked unhealthy by a header refusal")
	}
}
