package client

import (
	"encoding/json"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// A tools/call must always carry an "arguments" object: omitting it for a
// no-argument tool makes strict MCP servers answer -32602.
func TestToolsCallParamsAlwaysCarryArguments(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"empty map": {},
		"nil map":   nil,
	} {
		raw, err := json.Marshal(mcp.ToolsCallParams{Name: "t", Arguments: args})
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if string(got["arguments"]) != "{}" {
			t.Errorf("%s: arguments = %s, want {}", name, got["arguments"])
		}
	}
}
