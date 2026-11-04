package gateway

import (
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
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/detectioncheck"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/detectionevents"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const checkTenant = "00000000-0000-0000-0000-0000000000c1"

type checkAgentAuth struct{}

func (checkAgentAuth) Name() string { return "agent" }
func (checkAgentAuth) Authenticate(context.Context, *http.Request) (*pkgauth.Principal, error) {
	return &pkgauth.Principal{Subject: "key-1", KeyID: "key-1", TenantID: checkTenant, Roles: []string{"agent"}, AuthMethod: "apikey"}, nil
}

// standInAgent is a stand-in detection agent for the MCP plane: it answers
// the policy, the tool-call check (with check, which may fail in any way),
// and records the checks and events it is sent.
type standInAgent struct {
	url    string
	mu     sync.Mutex
	checks []map[string]any
	events []map[string]any
}

func newStandInAgent(t *testing.T, inlineToolCalls bool, check func(w http.ResponseWriter)) *standInAgent {
	t.Helper()
	a := &standInAgent{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/policy":
			_ = json.NewEncoder(w).Encode(map[string]any{"inline": false, "want_response": true, "inline_tool_calls": inlineToolCalls})
		case "POST /v1/tool-calls/check":
			var c map[string]any
			_ = json.Unmarshal(b, &c)
			a.mu.Lock()
			a.checks = append(a.checks, c)
			a.mu.Unlock()
			check(w)
		case "POST /v1/events":
			var batch struct {
				Events []map[string]any `json:"events"`
			}
			_ = json.Unmarshal(b, &batch)
			a.mu.Lock()
			a.events = append(a.events, batch.Events...)
			a.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func (a *standInAgent) snapshot() (checks, events []map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.checks...), append([]map[string]any(nil), a.events...)
}

// checkPlane is an MCP plane with one connector ("alpha", the dptest
// backend), the inline check through agent, and its events to agent.
type checkPlane struct {
	plane   *dataplane.Plane
	backend *dptest.Backend
	checker *detectioncheck.Checker
	events  *detectionevents.Emitter
}

func newCheckPlane(t *testing.T, agentURL string) *checkPlane {
	t.Helper()
	netguardtest.AllowLoopback()
	ctx := context.Background()
	backend := dptest.NewBackend(dptest.BackendOptions{Tools: []mcp.Tool{dptest.SlowToolDef()}})
	t.Cleanup(backend.Close)
	st := dptest.New()
	if err := st.Tenants().Create(ctx, &store.Tenant{ID: checkTenant, Slug: "c", Name: "C"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Connectors().Create(ctx, &store.Connector{TenantID: checkTenant, Name: "alpha", Slug: "alpha",
		Endpoint: backend.URL, TimeoutMS: 5000, Status: "unknown", Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Service.Version = "test"
	cfg.ToolCache.Enabled = false
	checker := detectioncheck.New(detectioncheck.Config{AgentURL: agentURL, Timeout: 200 * time.Millisecond})
	events := detectionevents.New(detectionevents.Config{AgentURL: agentURL, FlushInterval: 10 * time.Millisecond})
	p, err := dataplane.New(dataplane.Deps{
		Config: cfg, Store: st, Headers: headers.NewRegistry(), Authenticator: checkAgentAuth{},
		Authorizer: pkgauth.NewRoleAuthorizer(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientIP:        func(*http.Request) string { return "198.51.100.7" },
		OnToolCall:      toolCallEvents(events),
		CheckToolCall:   toolCallCheck(checker),
		DetectionStatus: detectionStatus(checker),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return &checkPlane{plane: p, backend: backend, checker: checker, events: events}
}

func (p *checkPlane) toolsCall(t *testing.T, args string) mcp.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"alpha__slow","arguments":` + args + `}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	p.plane.Handler.ServeHTTP(rec, req)
	var out mcp.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

// flush posts the queued events.
func (p *checkPlane) flush() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.events.Close(ctx)
}

// A blocking verdict from the agent refuses the call with -32003; the
// backend is never called; the tool_call event says blocked and checked,
// under the id the check was sent with; /health counts it.
func TestMCPToolCallCheck_Block(t *testing.T) {
	agent := newStandInAgent(t, true, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"block":{"rule_id":"r1","owasp":"LLM06","title":"Out of scope","threshold":0,"detail":"prod is not in scope"}}`))
	})
	p := newCheckPlane(t, agent.url)
	r := p.toolsCall(t, `{"steps":0,"target":"prod"}`)
	want := "Blocked by detection rule r1 (Out of scope): prod is not in scope. The tool call was not sent to the connector."
	if r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed || r.Error.Message != want {
		t.Fatalf("response = %+v", r)
	}
	if n := p.backend.Calls(mcp.MethodToolsCall); n != 0 {
		t.Fatalf("backend called %d times", n)
	}
	p.flush()
	checks, events := agent.snapshot()
	if len(checks) != 1 || len(events) != 1 {
		t.Fatalf("checks %v events %v", checks, events)
	}
	c, ev := checks[0], events[0]
	if c["server"] != "alpha" || c["tool"] != "slow" || c["outcome"] != "allowed" || c["client_ip"] != "198.51.100.7" ||
		c["key_id"] != "key-1" || c["arguments"] != `{"steps":0,"target":"prod"}` || c["id"] == "" || c["v"] != float64(1) {
		t.Errorf("check = %v", c)
	}
	if ev["outcome"] != "blocked" || ev["checked"] != true || ev["id"] != c["id"] || ev["client_ip"] != "198.51.100.7" {
		t.Errorf("event = %v; want blocked, checked, id %v", ev, c["id"])
	}
	if st := p.checker.Status(); st["blocked"] != uint64(1) || st["checked"] != uint64(1) {
		t.Errorf("status = %v", st)
	}

	rec := httptest.NewRecorder()
	p.plane.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var health struct {
		Detection struct {
			ToolChecks map[string]any `json:"tool_checks"`
		} `json:"detection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil || health.Detection.ToolChecks["blocked"] != float64(1) {
		t.Errorf("health = %s", rec.Body.String())
	}
}

// A verdict with no block forwards the call, and the event says checked.
func TestMCPToolCallCheck_Allow(t *testing.T) {
	agent := newStandInAgent(t, true, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{}`)) })
	p := newCheckPlane(t, agent.url)
	if r := p.toolsCall(t, `{"steps":0}`); r.Error != nil {
		t.Fatalf("refused: %+v", r.Error)
	}
	if n := p.backend.Calls(mcp.MethodToolsCall); n != 1 {
		t.Fatalf("backend called %d times", n)
	}
	p.flush()
	_, events := agent.snapshot()
	if len(events) != 1 || events[0]["outcome"] != "allowed" || events[0]["checked"] != true {
		t.Errorf("events = %v", events)
	}
}

// Every agent failure fails open: the call is forwarded, the event is not
// checked (so the engine still judges it from the event), and the failure
// is counted. A 503 is how the agent says the engine did not judge the call.
func TestMCPToolCallCheck_FailsOpen(t *testing.T) {
	for name, check := range map[string]func(w http.ResponseWriter){
		"500": func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
		// What the agent answers when the engine did not judge the call
		// (busy, engine failure, policy off, arguments dropped).
		"503 not judged": func(w http.ResponseWriter) {
			http.Error(w, "agent: tool-call check not judged", http.StatusServiceUnavailable)
		},
		"bad json": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`not json`)) },
		"timeout": func(w http.ResponseWriter) {
			time.Sleep(time.Second)
			_, _ = w.Write([]byte(`{"block":{"rule_id":"late"}}`))
		},
	} {
		agent := newStandInAgent(t, true, check)
		p := newCheckPlane(t, agent.url)
		if r := p.toolsCall(t, `{"steps":0}`); r.Error != nil {
			t.Fatalf("%s: refused %+v", name, r.Error)
		}
		if n := p.backend.Calls(mcp.MethodToolsCall); n != 1 {
			t.Fatalf("%s: backend called %d times", name, n)
		}
		p.flush()
		_, events := agent.snapshot()
		if len(events) != 1 || events[0]["outcome"] != "allowed" || events[0]["checked"] != nil {
			t.Errorf("%s: events = %v; want allowed, not checked", name, events)
		}
		if st := p.checker.Status(); st["failed"] != uint64(1) {
			t.Errorf("%s: status %v", name, st)
		}
	}

	// Agent down: forwarded.
	p := newCheckPlane(t, "http://127.0.0.1:1")
	if r := p.toolsCall(t, `{"steps":0}`); r.Error != nil {
		t.Fatalf("agent down: refused %+v", r.Error)
	}
	if n := p.backend.Calls(mcp.MethodToolsCall); n != 1 {
		t.Fatalf("agent down: backend called %d times", n)
	}
}

// A tenant whose policy does not ask is not checked: no POST.
func TestMCPToolCallCheck_PolicyOff(t *testing.T) {
	agent := newStandInAgent(t, false, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"block":{"rule_id":"r1"}}`)) })
	p := newCheckPlane(t, agent.url)
	if r := p.toolsCall(t, `{"steps":0}`); r.Error != nil {
		t.Fatalf("refused: %+v", r.Error)
	}
	if checks, _ := agent.snapshot(); len(checks) != 0 {
		t.Fatalf("checks = %v; want none", checks)
	}
}

// Arguments over 64 KiB are cut, with arguments_truncated set.
func TestMCPToolCallCheck_ArgumentsCut(t *testing.T) {
	agent := newStandInAgent(t, true, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{}`)) })
	p := newCheckPlane(t, agent.url)
	big := strings.Repeat("x", 80<<10)
	if r := p.toolsCall(t, `{"steps":0,"blob":"`+big+`"}`); r.Error != nil {
		t.Fatalf("refused: %+v", r.Error)
	}
	checks, _ := agent.snapshot()
	if len(checks) != 1 {
		t.Fatalf("checks = %d", len(checks))
	}
	args, _ := checks[0]["arguments"].(string)
	if len(args) != detectioncheck.MaxArguments || checks[0]["arguments_truncated"] != true {
		t.Errorf("arguments len %d truncated %v; want %d and true", len(args), checks[0]["arguments_truncated"], detectioncheck.MaxArguments)
	}
}
