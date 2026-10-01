package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
)

func createConnector(t *testing.T, h Connectors, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(body)), "tenant-a", "admin")
	return serve(http.MethodPost, "/connectors", h.Create, req)
}

func TestConnectors_Create_EgressBlocked(t *testing.T) {
	netguard.SetPolicy(nil) // production default: everything internal blocked
	t.Cleanup(netguardtest.AllowLoopback)
	h := Connectors{Deps: newTestDeps()}

	for _, ep := range []string{
		"http://127.0.0.1:28233/mcp", "http://169.254.169.254/latest/meta-data", "http://10.255.255.1/mcp",
		"http://[::1]:8080/mcp", "http://2130706433:28233/mcp", "http://0x7f000001/mcp", "http://[::ffff:127.0.0.1]/mcp",
	} {
		w := createConnector(t, h, `{"name":"x","endpoint":"`+ep+`"}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"type":"egress_blocked"`) {
			t.Errorf("%s: status = %d, body = %s; want 400 egress_blocked", ep, w.Code, w.Body.String())
		}
	}
	// A hostname is checked at dial time, so it is accepted here.
	if w := createConnector(t, h, `{"name":"pub","endpoint":"https://mcp.example.com/mcp"}`); w.Code != http.StatusCreated {
		t.Errorf("public hostname: status = %d, body = %s", w.Code, w.Body.String())
	}
	// Allowlisted loopback is accepted.
	p, _ := netguard.NewPolicy([]string{"127.0.0.0/8"}, nil)
	netguard.SetPolicy(p)
	if w := createConnector(t, h, `{"name":"local","endpoint":"http://127.0.0.1:28233/mcp"}`); w.Code != http.StatusCreated {
		t.Errorf("allowlisted loopback: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestConnectors_BearerTokenForwardingGate(t *testing.T) {
	const body = `{"name":"fwd","endpoint":"https://mcp.example.com/mcp","metadata":{"headers":{"Authorization":{"type":"token_field","field":"bearer_token","prefix":"Bearer "}}}}`

	deps := newTestDeps()
	deps.Headers = dataplaneheaders.NewRegistry()
	h := Connectors{Deps: deps}
	w := createConnector(t, h, body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"type":"bearer_forwarding_disabled"`) {
		t.Fatalf("flag off: status = %d, body = %s", w.Code, w.Body.String())
	}
	// Other token_field fields stay allowed.
	ok := strings.Replace(strings.Replace(body, "bearer_token", "email", 1), `"fwd"`, `"fwd-email"`, 1)
	if w := createConnector(t, h, ok); w.Code != http.StatusCreated {
		t.Errorf("email field: status = %d, body = %s", w.Code, w.Body.String())
	}

	deps.Headers.SetAllowBearerForwarding(true)
	if w := createConnector(t, h, body); w.Code != http.StatusCreated {
		t.Errorf("flag on: status = %d, body = %s", w.Code, w.Body.String())
	}
}
