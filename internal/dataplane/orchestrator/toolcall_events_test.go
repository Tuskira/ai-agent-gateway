package orchestrator

import (
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// A restricted key (containment) is refused every tools/call before routing,
// with the restriction as the reason; the same call from an unrestricted key
// goes through.
func TestRestrictedKeyRefusesToolCalls(t *testing.T) {
	h := newHarness(t)
	sess := h.initialize(t)

	call := func(restricted bool) *mcp.Response {
		p := *h.principal
		p.ToolsRestricted = restricted
		in := Request{JSONRPC: slowCall(7, `{"name":"alpha__slow","arguments":{"steps":0}}`), Principal: &p,
			Session: sess, Backends: sess}
		return h.orch.Handle(h.ctx(), in).Response
	}
	if r := call(false); r.Error != nil {
		t.Fatalf("unrestricted call refused: %+v", r.Error)
	}
	r := call(true)
	if r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed || r.Error.Message != restrictedMessage {
		t.Fatalf("restricted call = %+v", r)
	}
}
