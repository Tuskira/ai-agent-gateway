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
		Headers: map[string]string{"user-agent": "claude-cli/2.0.0 (external, cli)", "x-stainless-lang": "js"},
	}
	sameJSON("turn_no_response.json", turn)
	turn.StatusCode = 200
	turn.Response = []byte(`{"type":"message","content":[{"type":"text","text":"hi"},` +
		`{"type":"tool_use","id":"toolu_1","name":"get_time","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":1}}`)
	turn.Answer = &Answer{Content: []ContentBlock{
		{Type: ContentText, Text: "hi"},
		{Type: ContentToolUse, ID: "toolu_1", Name: "get_time", Input: json.RawMessage(`{}`)},
	}, StopReason: "tool_use"}
	turn.ResponseType, turn.RequestJudged = "application/json", true
	turn.ClientIP = "192.0.2.10"
	sameJSON("turn.json", turn)
	if m := MetaOfTurn(turn); m.ClientIP != "192.0.2.10" {
		t.Errorf("MetaOfTurn: client_ip %q", m.ClientIP)
	}

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

	// The inline request stage (POST /v1/turns/request): the raw body with
	// the gateway's reading of it, here a Bedrock converse request.
	var gotReq TurnRequest
	if err := json.Unmarshal(read("turn_request.json"), &gotReq); err != nil {
		t.Fatal(err)
	}
	sameJSON("turn_request.json", gotReq)
	req := TurnRequest{
		ID: "req_0123456789abcdef", TenantID: "tenant-1", SessionID: "session-1", KeyID: "key-1",
		Principal: "user-1", ClientIP: "192.0.2.10", Model: "amazon.nova-pro-v1:0", Path: "/model/amazon.nova-pro-v1:0/converse",
		At:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Request: []byte(`{"system":[{"text":"be brief"}],"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":64}}`),
		Dialect: "bedrock_converse", Op: OpGenerate,
		Conversation: &Conversation{Version: ConversationVersion, System: text("be brief"),
			Messages: []Message{{Role: "user", Content: text("hello")}}, History: HistoryFull},
	}
	sameJSON("turn_request.json", req)
	if c := req.Call(); c.Conversation != req.Conversation || string(c.Request) != string(req.Request) {
		t.Errorf("Call of the fixture: %+v", c)
	}
	if m := MetaOf(req); m.Dialect != "bedrock_converse" || m.Op != OpGenerate || m.Model != "amazon.nova-pro-v1:0" || m.RequestID != req.ID || m.ClientIP != "192.0.2.10" {
		t.Errorf("MetaOf the fixture: %+v", m)
	}
}

// The inline tool-call check (POST /v1/tool-calls/check) is pinned by
// tool_call_check.json, a byte-identical copy of the gateway's
// internal/detectioncheck/testdata/tool_call_check.json.
func TestToolCallCheckFixture(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "gateway", "tool_call_check.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got ToolCallCheck
	if err := json.Unmarshal(want, &got); err != nil {
		t.Fatal(err)
	}
	built := ToolCallCheck{V: 1, ID: "req_0123456789abcdef", TenantID: "tenant-1", SessionID: "mcp-session-1",
		KeyID: "key-1", Principal: "user-1", ClientIP: "192.0.2.10", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Profile: "reader", ConnectorID: "connector-1", Server: "alpha", Tool: "lookup", Outcome: OutcomeMonitored,
		Arguments: `{"host":"db-1","limit":5}`, ArgumentsTruncated: true}
	if !reflect.DeepEqual(got, built) {
		t.Fatalf("fixture decodes to %+v\nwant %+v", got, built)
	}
	gb, _ := json.Marshal(built)
	var g, w map[string]any
	_ = json.Unmarshal(gb, &g)
	_ = json.Unmarshal(want, &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("marshaled = %s\nwant %s", gb, want)
	}
}

// TurnRequest.Call follows Turn.Call's rule: the canonical conversation
// only when the gateway read it without error, in a version this agent
// knows; else (an older gateway, which sends none) the raw body alone.
func TestTurnRequestCall(t *testing.T) {
	c := &Conversation{Version: ConversationVersion, History: HistoryFull,
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: ContentText, Text: "hi"}}}}}
	base := TurnRequest{Request: []byte(`{"messages":[]}`), Conversation: c}
	if got := base.Call(); got.Conversation != c || string(got.Request) != string(base.Request) || got.Answer != nil {
		t.Errorf("read request: %+v", got)
	}
	failed := base
	failed.NormalizeError = "decode request: cut"
	newer := base
	newer.Conversation = &Conversation{Version: ConversationVersion + 1}
	for name, tr := range map[string]TurnRequest{"normalize_error": failed, "newer version": newer, "older gateway": {Request: base.Request}} {
		if got := tr.Call(); got.Conversation != nil || string(got.Request) != string(base.Request) {
			t.Errorf("%s: %+v; want the raw body only", name, got)
		}
	}
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
