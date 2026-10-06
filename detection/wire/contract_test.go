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

	for _, name := range []string{"turn.json", "turn_no_response.json", "turn_batch.json"} {
		var got Turn
		if err := json.Unmarshal(read(name), &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.V != TurnVersion {
			t.Errorf("%s: v = %d", name, got.V)
		}
		sameJSON(name, got)
	}

	conv := &Conversation{
		Version: ConversationVersion, System: []ContentBlock{{Type: ContentText, Text: "be brief"}},
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: ContentText, Text: "hello"}}}},
		Tools:    []string{"get_time"}, History: HistoryFull,
	}
	turn := Turn{
		V: 1, ID: "req_0123456789abcdef", TenantID: "tenant-1", SessionID: "session-1", KeyID: "key-1",
		Principal: "user-1", Model: "example-model", Path: "/v1/messages",
		At:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		StatusCode: 429,
		Request: []byte(`{"model":"example-model","max_tokens":64,"system":"be brief",` +
			`"tools":[{"name":"get_time","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`),
		Dialect: "anthropic", Op: OpGenerate, Conversation: conv,
	}
	sameJSON("turn_no_response.json", turn)
	turn.StatusCode = 200
	turn.Response = []byte(`{"type":"message","content":[{"type":"text","text":"hi"},` +
		`{"type":"tool_use","id":"toolu_1","name":"get_time","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":1}}`)
	turn.Answer = &Answer{Content: []ContentBlock{
		{Type: ContentText, Text: "hi"},
		{Type: ContentToolUse, ID: "toolu_1", Name: "get_time", Input: json.RawMessage(`{}`)},
	}, StopReason: "tool_use"}
	sameJSON("turn.json", turn)

	text := func(s string) []ContentBlock { return []ContentBlock{{Type: ContentText, Text: s}} }
	batch := Turn{
		V: 1, ID: "req_0123456789abcdef", TenantID: "tenant-1", SessionID: "session-1", KeyID: "key-1",
		Principal: "user-1", Path: "/v1/messages/batches",
		At:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		StatusCode: 200,
		Request: []byte(`{"requests":[` +
			`{"custom_id":"q1","params":{"model":"example-model","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":"hello"}]}},` +
			`{"custom_id":"q2","params":{"model":"example-model","max_tokens":64,"messages":[{"role":"user","content":"what time is it?"}]}}]}`),
		Response: []byte(`{"id":"msgbatch_1","type":"message_batch","processing_status":"in_progress",` +
			`"request_counts":{"processing":2,"succeeded":0,"errored":0,"canceled":0,"expired":0}}`),
		Dialect: "anthropic_batch", Op: OpBatch,
		Items: []TurnItem{
			{CustomID: "q1", Conversation: Conversation{Version: ConversationVersion, System: text("be brief"),
				Messages: []Message{{Role: "user", Content: text("hello")}}, History: HistoryFull}},
			{CustomID: "q2", Conversation: Conversation{Version: ConversationVersion,
				Messages: []Message{{Role: "user", Content: text("what time is it?")}}, History: HistoryFull}},
		},
	}
	sameJSON("turn_batch.json", batch)
}

// Call hands package turn the canonical forms only when the gateway read
// them without error, in a version this agent knows.
func TestTurnCall(t *testing.T) {
	c := &Conversation{Version: ConversationVersion, History: HistoryFull,
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: ContentText, Text: "hi"}}}}}
	a := &Answer{Content: []ContentBlock{{Type: ContentText, Text: "ok"}}}
	base := Turn{Request: []byte(`{"messages":[]}`), Response: []byte(`{}`), Conversation: c, Answer: a}
	if got := base.Call(); got.Conversation != c || got.Answer != a || string(got.Request) != string(base.Request) ||
		string(got.Response) != string(base.Response) {
		t.Errorf("read turn: %+v", got)
	}
	failed := base
	failed.NormalizeError = "decode response: cut"
	newer := base
	newer.Conversation = &Conversation{Version: ConversationVersion + 1}
	for name, tr := range map[string]Turn{"normalize_error": failed, "newer version": newer, "no conversation": {Request: base.Request}} {
		if got := tr.Call(); got.Conversation != nil || got.Answer != nil || string(got.Request) != string(base.Request) {
			t.Errorf("%s: %+v; want the raw bodies only", name, got)
		}
	}
}
