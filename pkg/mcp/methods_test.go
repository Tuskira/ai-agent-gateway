package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

func TestCancellationAndProgressAreValidMethods(t *testing.T) {
	for _, m := range []string{mcp.NotificationCancelled, mcp.NotificationProgress} {
		if !mcp.IsValidMethod(m) {
			t.Errorf("mcp.IsValidMethod(%q) = false", m)
		}
	}
}

func TestProgressAndCancelledParamsKeepIDsVerbatim(t *testing.T) {
	var c mcp.CancelledParams
	if err := json.Unmarshal([]byte(`{"requestId":7,"reason":"stop"}`), &c); err != nil {
		t.Fatal(err)
	}
	if string(c.RequestID) != "7" || c.Reason != "stop" {
		t.Fatalf("mcp.CancelledParams = %+v", c)
	}

	total := 10.0
	raw, err := json.Marshal(mcp.ProgressParams{ProgressToken: json.RawMessage(`"tok"`), Progress: 2, Total: &total})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"progressToken":"tok","progress":2,"total":10}` {
		t.Fatalf("mcp.ProgressParams = %s", raw)
	}

	raw, err = json.Marshal(mcp.ToolsCallParams{Name: "x", Meta: json.RawMessage(`{"progressToken":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"name":"x","arguments":{},"_meta":{"progressToken":1}}` {
		t.Fatalf("mcp.ToolsCallParams = %s", raw)
	}
}
