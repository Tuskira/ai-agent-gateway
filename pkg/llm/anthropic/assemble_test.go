package anthropic

import (
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// sseOf frames events as an Anthropic stream: pairs of event name and data.
func sseOf(pairs ...string) []byte {
	var b strings.Builder
	for i := 0; i < len(pairs); i += 2 {
		b.WriteString("event: " + pairs[i] + "\ndata: " + pairs[i+1] + "\n\n")
	}
	return []byte(b.String())
}

func TestAssembleStream_Text(t *testing.T) {
	body := sseOf(
		"message_start", `{"type":"message_start","message":{"id":"msg_test_text","type":"message","role":"assistant","model":"claude-test-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"ping", `{"type": "ping"}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, "}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a <b> & \"c\" é"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	got, ok := AssembleStream(body)
	if !ok {
		t.Fatal("not assembled")
	}
	want := `{"id":"msg_test_text","type":"message","role":"assistant","model":"claude-test-1","content":[{"type":"text","text":"Hello, a <b> & \"c\" é"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":9}}`
	if string(got) != want {
		t.Errorf("assembled\n got %s\nwant %s", got, want)
	}
}

// A thinking block (empty text, then a signature) and three tool_use blocks
// whose arguments arrive in fragments, the first empty. Fields the gateway has
// no slot for (container, diagnostics, service_tier, caller, stop_details,
// context_management, a nested usage detail) survive; a delta's
// estimated_tokens, which no message has, is not added.
func TestAssembleStream_ThinkingAndToolCalls(t *testing.T) {
	body := sseOf(
		"message_start", `{"type":"message_start","message":{"model":"claude-test-1","id":"msg_test_tools","type":"message","role":"assistant","content":[],"container":null,"stop_reason":null,"stop_sequence":null,"stop_details":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5},"output_tokens":2,"service_tier":"standard"},"diagnostics":null,"context_management":null}              }`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}   }`,
		"ping", `{"type": "ping"}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"","estimated_tokens":50}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"","estimated_tokens":null}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"ZmFrZS1zaWduYXR1cmU="}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_test_0001","name":"Bash","input":{},"caller":{"type":"direct"}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"ls -"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"la\", \"description\""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":": \"List files\"}"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":1}`,
		"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_test_0002","name":"Bash","input":{},"caller":{"type":"direct"}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"echo <hi> & done\"}"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":2}`,
		"content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_test_0003","name":"Read","input":{},"caller":{"type":"direct"}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":" \"/tmp/example.txt\"}"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":3}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null,"stop_details":null,"container":null},"usage":{"input_tokens":2,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":40,"output_tokens_details":{"thinking_tokens":6}},"context_management":{"applied_edits":[]}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	got, ok := AssembleStream(body)
	if !ok {
		t.Fatal("not assembled")
	}
	want := `{"model":"claude-test-1","id":"msg_test_tools","type":"message","role":"assistant","content":[` +
		`{"type":"thinking","thinking":"","signature":"ZmFrZS1zaWduYXR1cmU="},` +
		`{"type":"tool_use","id":"toolu_test_0001","name":"Bash","input":{"command":"ls -la","description":"List files"},"caller":{"type":"direct"}},` +
		`{"type":"tool_use","id":"toolu_test_0002","name":"Bash","input":{"command":"echo <hi> & done"},"caller":{"type":"direct"}},` +
		`{"type":"tool_use","id":"toolu_test_0003","name":"Read","input":{"file_path":"/tmp/example.txt"},"caller":{"type":"direct"}}` +
		`],"container":null,"stop_reason":"tool_use","stop_sequence":null,"stop_details":null,` +
		`"usage":{"input_tokens":2,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5},"output_tokens":40,"service_tier":"standard","output_tokens_details":{"thinking_tokens":6}},` +
		`"diagnostics":null,"context_management":{"applied_edits":[]}}`
	if string(got) != want {
		t.Errorf("assembled\n got %s\nwant %s", got, want)
	}
}

func TestAssembleStream_ToolUseWithoutInputFragments(t *testing.T) {
	body := sseOf(
		"message_start", `{"type":"message_start","message":{"id":"msg_test_noargs","type":"message","role":"assistant","model":"claude-test-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_test_0004","name":"Now","input":{}}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_test_0005","name":"Now"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`,
		"content_block_stop", `{"type":"content_block_stop","index":1}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	got, ok := AssembleStream(body)
	if !ok {
		t.Fatal("not assembled")
	}
	want := `{"id":"msg_test_noargs","type":"message","role":"assistant","model":"claude-test-1","content":[` +
		`{"type":"tool_use","id":"toolu_test_0004","name":"Now","input":{}},` +
		`{"type":"tool_use","id":"toolu_test_0005","name":"Now","input":{}}` +
		`],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":3}}`
	if string(got) != want {
		t.Errorf("assembled\n got %s\nwant %s", got, want)
	}
}

// Arguments that never became valid JSON are kept, as a string.
func TestAssembleStream_UnparseableArguments(t *testing.T) {
	body := sseOf(
		"message_start", `{"type":"message_start","message":{"id":"msg_test_bad","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_test_0006","name":"X","input":{}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\": "}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	got, ok := AssembleStream(body)
	if !ok {
		t.Fatal("not assembled")
	}
	if want := `"input":"{\"a\": "}`; !strings.Contains(string(got), want) {
		t.Errorf("assembled %s, want it to contain %s", got, want)
	}
}

func TestAssembleStream_Citations(t *testing.T) {
	body := sseOf(
		"message_start", `{"type":"message_start","message":{"id":"msg_test_cite","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","citations":null}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"see source"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com/a"}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com/b"}}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	got, ok := AssembleStream(body)
	if !ok {
		t.Fatal("not assembled")
	}
	want := `"content":[{"type":"text","text":"see source","citations":[{"type":"web_search_result_location","url":"https://example.com/a"},{"type":"web_search_result_location","url":"https://example.com/b"}]}]`
	if !strings.Contains(string(got), want) {
		t.Errorf("assembled %s, want it to contain %s", got, want)
	}
}

// Everything the caller must keep raw: ok is false and nothing panics.
func TestAssembleStream_NotAssembled(t *testing.T) {
	start := `{"type":"message_start","message":{"id":"msg_test_x","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`
	text0 := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	delta0 := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`
	stop0 := `{"type":"content_block_stop","index":0}`
	stop := `{"type":"message_stop"}`
	for name, body := range map[string][]byte{
		"cut before message_stop":   sseOf("message_start", start, "content_block_start", text0, "content_block_delta", delta0, "content_block_stop", stop0),
		"cut mid event":             []byte("event: message_start\ndata: " + start + "\n\nevent: content_block_start\ndata: {\"type\":\"content_bl"),
		"no message_start":          sseOf("content_block_start", text0, "content_block_stop", stop0, "message_stop", stop),
		"error event":               sseOf("message_start", start, "error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
		"error event after content": sseOf("message_start", start, "content_block_start", text0, "content_block_delta", delta0, "error", `{"type":"error","error":{"type":"api_error","message":"x"}}`, "message_stop", stop),
		"frame not JSON":            sseOf("message_start", start, "content_block_start", "{not json", "message_stop", stop),
		"frame not an object":       sseOf("message_start", start, "content_block_start", `[1,2]`, "message_stop", stop),
		"duplicate key in frame":    sseOf("message_start", start, "content_block_start", `{"type":"content_block_start","index":0,"index":1,"content_block":{"type":"text","text":""}}`, "message_stop", stop),
		"second message_start":      sseOf("message_start", start, "message_start", start, "message_stop", stop),
		"block index skipped":       sseOf("message_start", start, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, "message_stop", stop),
		"block index repeated":      sseOf("message_start", start, "content_block_start", text0, "content_block_start", text0, "message_stop", stop),
		"delta for unknown block":   sseOf("message_start", start, "content_block_delta", delta0, "message_stop", stop),
		"delta after block stop":    sseOf("message_start", start, "content_block_start", text0, "content_block_stop", stop0, "content_block_delta", delta0, "message_stop", stop),
		"stop for unknown block":    sseOf("message_start", start, "content_block_stop", stop0, "message_stop", stop),
		"block left open":           sseOf("message_start", start, "content_block_start", text0, "content_block_delta", delta0, "message_stop", stop),
		"unknown delta type":        sseOf("message_start", start, "content_block_start", text0, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"future_delta","x":1}}`, "content_block_stop", stop0, "message_stop", stop),
		"json args on a text block": sseOf("message_start", start, "content_block_start", text0, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, "content_block_stop", stop0, "message_stop", stop),
		"non-SSE JSON body":         []byte(`{"id":"msg_test_x","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`),
		"OpenAI stream":             []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"),
		"garbage":                   []byte("\x00\x01\xff\xfe not a stream \x1b[0m"),
		"empty":                     nil,
		"only a ping":               sseOf("ping", `{"type": "ping"}`),
		"line over the frame cap":   []byte("data: " + strings.Repeat("a", llm.MaxFrameBytes+10) + "\n\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := AssembleStream(body); ok || got != nil {
				t.Errorf("assembled %q, want ok=false", got)
			}
		})
	}
}

// CRLF line ends and stray comment lines are SSE and must not stop assembly.
func TestAssembleStream_CRLFAndComments(t *testing.T) {
	body := ": keep-alive\r\n\r\n" +
		"event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test_crlf\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\r\n\r\n" +
		"event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	got, ok := AssembleStream([]byte(body))
	if !ok {
		t.Fatal("not assembled")
	}
	want := `{"id":"msg_test_crlf","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`
	if string(got) != want {
		t.Errorf("assembled\n got %s\nwant %s", got, want)
	}
}

func TestAssembleStream_Deterministic(t *testing.T) {
	body := sseOf(
		"message_start", `{"type":"message_start","message":{"id":"msg_test_det","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","zeta":1,"alpha":2},"usage":{"output_tokens":4,"zz":1,"aa":2}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	first, ok := AssembleStream(body)
	if !ok {
		t.Fatal("not assembled")
	}
	for i := 0; i < 20; i++ {
		if again, _ := AssembleStream(body); string(again) != string(first) {
			t.Fatalf("run %d differs:\n%s\n%s", i, again, first)
		}
	}
	// Keys a later event adds come after the existing ones, in arrival order.
	want := `{"id":"msg_test_det","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":4,"zz":1,"aa":2},"stop_reason":"end_turn","zeta":1,"alpha":2}`
	if string(first) != want {
		t.Errorf("assembled\n got %s\nwant %s", first, want)
	}
}

// The registry reaches the assembler through the Reader's name.
func TestAssembleStream_ThroughRegistry(t *testing.T) {
	body := sseOf(
		"message_start", `{"type":"message_start","message":{"id":"msg_test_reg","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	if got, ok := llm.AssembleStream(Name, body); !ok || !strings.Contains(string(got), `"msg_test_reg"`) {
		t.Errorf("llm.AssembleStream(%q) = %q, %v", Name, got, ok)
	}
	if got, ok := llm.AssembleStream(CompleteReaderName, body); ok {
		t.Errorf("a Reader that cannot assemble returned %q", got)
	}
	if _, ok := llm.AssembleStream("no_such_reader", body); ok {
		t.Error("unknown reader assembled")
	}
}
