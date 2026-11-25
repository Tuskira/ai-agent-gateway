package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
)

// refreshRecorder is a ConnectorOps that also implements ops.ConnectorRefresher
// and records the connectors it was told changed.
type refreshRecorder struct {
	*fakeConnectorOps
	mu      sync.Mutex
	changed []string
}

var _ ops.ConnectorRefresher = (*refreshRecorder)(nil)

func (r *refreshRecorder) ConnectorChanged(_ context.Context, _, connectorID string) {
	r.mu.Lock()
	r.changed = append(r.changed, connectorID)
	r.mu.Unlock()
}

func (r *refreshRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.changed
	r.changed = nil
	return out
}

// A create, and an edit that changes the endpoint, the headers, the slug or
// enabled, tell the data plane the connector changed; an edit that changes
// none of those (name, timeout, description, metadata left out) does not.
func TestConnectors_ChangeRefreshesTheConnector(t *testing.T) {
	deps := restrictExemptDeps()
	rec := &refreshRecorder{fakeConnectorOps: &fakeConnectorOps{store: deps.Store.Connectors()}}
	deps.ConnectorOps = rec
	h := Connectors{Deps: deps}

	w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors",
		`{"name":"alpha","endpoint":"https://a.example.com/mcp","metadata":{"headers":{"X-A":{"type":"static","value":"1"}}}}`, "admin"))
	var c connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if got := rec.take(); len(got) != 1 || got[0] != c.ID {
		t.Fatalf("create notified %v, want [%s]", got, c.ID)
	}

	const base = `"name":"alpha","endpoint":"https://a.example.com/mcp"`
	const hdr = `"headers":{"X-A":{"type":"static","value":"1"}}`
	put := func(body string) {
		t.Helper()
		w := serve(http.MethodPut, "/connectors/{id}", h.Update, connectorReq(http.MethodPut, "/connectors/"+c.ID, "{"+body+"}", "admin"))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT {%s}: %d %s", body, w.Code, w.Body.String())
		}
	}

	// Order matters (each step edits the previous state), so run in a list.
	steps := []struct {
		name    string
		body    string
		refresh bool
	}{
		{"same everything", base + `,"metadata":{` + hdr + `}`, false},
		{"metadata left out", base + `,"timeout_ms":5000`, false},
		{"name and timeout", `"name":"renamed","endpoint":"https://a.example.com/mcp","timeout_ms":9000,"metadata":{` + hdr + `}`, false},
		{"description", `"name":"renamed","endpoint":"https://a.example.com/mcp","description":"d","metadata":{` + hdr + `}`, false},
		{"header value", `"name":"renamed","endpoint":"https://a.example.com/mcp","metadata":{"headers":{"X-A":{"type":"static","value":"2"}}}`, true},
		{"header added", `"name":"renamed","endpoint":"https://a.example.com/mcp","metadata":{"headers":{"X-A":{"type":"static","value":"2"},"X-B":{"type":"static","value":"b"}}}`, true},
		{"header removed", `"name":"renamed","endpoint":"https://a.example.com/mcp","metadata":{"headers":{"X-A":{"type":"static","value":"2"}}}`, true},
		{"all headers removed", `"name":"renamed","endpoint":"https://a.example.com/mcp","metadata":{}`, true},
		{"endpoint", `"name":"renamed","endpoint":"https://b.example.com/mcp","metadata":{}`, true},
		{"slug", `"name":"renamed","slug":"alpha-2","endpoint":"https://b.example.com/mcp","metadata":{}`, true},
		{"disabled", `"name":"renamed","endpoint":"https://b.example.com/mcp","metadata":{"enabled":false}`, true},
		{"still disabled", `"name":"renamed","endpoint":"https://b.example.com/mcp","metadata":{"enabled":false}`, false},
		{"enabled", `"name":"renamed","endpoint":"https://b.example.com/mcp","metadata":{}`, true},
	}
	rec.take()
	for _, st := range steps {
		put(st.body)
		got := rec.take()
		if st.refresh && (len(got) != 1 || got[0] != c.ID) {
			t.Errorf("%s: notified %v, want [%s]", st.name, got, c.ID)
		}
		if !st.refresh && len(got) != 0 {
			t.Errorf("%s: notified %v, want nothing", st.name, got)
		}
	}
}
