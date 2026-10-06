package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The gateway-facing Turn is pinned by the fixtures in testdata/gateway,
// byte-identical copies of the gateway's internal/llmplane/testdata/detection
// (`make detection-contract` fails when they drift). This mirrors the
// gateway-side contract test: each fixture decodes into Turn and encodes
// back to the same JSON value, and a Turn built from fixed inputs encodes to
// the fixture.
func TestGatewayContractFixtures(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", "gateway", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// sameJSON compares by value, not by bytes (key order, spacing).
	sameJSON := func(name string, got any) {
		t.Helper()
		gb, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var g, w map[string]any
		if err := json.Unmarshal(gb, &g); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(read(name), &w); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s: marshaled = %s\nwant %s", name, gb, read(name))
		}
	}

	for _, name := range []string{"turn.json", "turn_no_response.json"} {
		var got Turn
		if err := json.Unmarshal(read(name), &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.V != TurnVersion {
			t.Errorf("%s: v = %d", name, got.V)
		}
		sameJSON(name, got)
	}

	turn := Turn{
		V: 1, ID: "req_0123456789abcdef", TenantID: "tenant-1", SessionID: "session-1", KeyID: "key-1",
		Principal: "user-1", Model: "example-model", Path: "/v1/messages",
		At:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		StatusCode: 429,
		Request:    []byte(`{"model":"example-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`),
	}
	sameJSON("turn_no_response.json", turn)
	turn.StatusCode = 200
	turn.Response = []byte(`{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	sameJSON("turn.json", turn)
}
