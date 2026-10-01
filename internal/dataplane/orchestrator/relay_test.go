package orchestrator

import (
	"encoding/json"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/upstream"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestDeclaredCapabilitiesKeepsOnlyTheRequestInvitingOnesRaw(t *testing.T) {
	got := declaredCapabilities(json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{` +
		`"sampling":{},"roots":{ "listChanged" : true },"elicitation":null,"experimental":{"x":{}}}}`))
	if len(got) != 2 || string(got["sampling"]) != `{}` || string(got["roots"]) != `{ "listChanged" : true }` {
		t.Fatalf("declared = %s, want sampling and roots verbatim, and nothing null or unrelated", got)
	}
	if got := declaredCapabilities(json.RawMessage(`{"capabilities":{}}`)); got != nil {
		t.Fatalf("declared = %s for none, want nil", got)
	}
}

func TestAdvertisedCapabilitiesIsTheIntersection(t *testing.T) {
	sess := &session.Session{ClientCapabilities: map[string]json.RawMessage{
		"sampling": json.RawMessage(`{}`), "roots": json.RawMessage(`{"listChanged":true}`),
	}}
	conn := &store.Connector{Metadata: map[string]any{
		"server_requests": map[string]any{"sampling": false, "elicitation": true, "roots": true},
	}}
	got := advertisedCapabilities(sess, conn)
	if len(got) != 1 || string(got["roots"]) != `{"listChanged":true}` {
		t.Fatalf("advertised = %s, want roots only", got)
	}
	// No session, or its anonymous handles: nothing.
	if got := advertisedCapabilities(session.NewAnonymous(), conn); got != nil {
		t.Fatalf("advertised = %s without a session, want nil", got)
	}
}

func TestPendingTableIsBoundedAndResolvesOnlyForItsSession(t *testing.T) {
	table := newPendingTable()
	a := upstream.SessionRef{TenantID: "t", SessionID: "a"}
	b := upstream.SessionRef{TenantID: "t", SessionID: "b"}
	otherTenant := upstream.SessionRef{TenantID: "u", SessionID: "a"}

	p1, ok1 := table.add(a, 2)
	_, ok2 := table.add(a, 2)
	_, ok3 := table.add(a, 2)
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("add = %v %v %v, want the third refused at the bound", ok1, ok2, ok3)
	}
	if _, ok := table.add(b, 2); !ok {
		t.Fatal("another session's bound was consumed")
	}

	resp := &mcp.Response{JSONRPC: mcp.Version, ID: p1.id, Result: json.RawMessage(`{}`)}
	if table.resolve(b, p1.id, resp) || table.resolve(otherTenant, p1.id, resp) {
		t.Fatal("a response on another session (or tenant) resolved the request")
	}
	if !table.resolve(a, p1.id, resp) {
		t.Fatal("the owning session's response did not resolve it")
	}
	if out := <-p1.reply; out.resp != resp {
		t.Fatalf("outcome = %+v", out)
	}
	if table.resolve(a, p1.id, resp) || table.remove(p1) {
		t.Fatal("a resolved request was still pending")
	}

	table.endSession(a)
	if n := table.count(a); n != 0 {
		t.Fatalf("%d requests still pending after the session ended", n)
	}
	if n := table.count(b); n != 1 {
		t.Fatalf("ending one session touched another's: %d pending", n)
	}
}
