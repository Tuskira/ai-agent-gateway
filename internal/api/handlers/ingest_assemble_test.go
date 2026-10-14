package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/anthropic"
)

// ingestStreamEvents is a short streamed Anthropic reply with one MCP tool
// call, as an interceptor captures it.
var ingestStreamEvents = [][2]string{
	{"message_start", `{"type":"message_start","message":{"id":"msg_test_ingest","type":"message","role":"assistant","model":"claude-test-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`},
	{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_test_ingest","name":"mcp__demo__lookup","input":{},"caller":{"type":"direct"}}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\": \"wid"}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"get\"}"}}`},
	{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":12}}`},
	{"message_stop", `{"type":"message_stop"}`},
}

const ingestAssembled = `{"id":"msg_test_ingest","type":"message","role":"assistant","model":"claude-test-1","content":[` +
	`{"type":"tool_use","id":"toolu_test_ingest","name":"mcp__demo__lookup","input":{"q":"widget"},"caller":{"type":"direct"}}` +
	`],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":12}}`

func ingestStreamBody(events [][2]string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: " + e[0] + "\ndata: " + e[1] + "\n\n")
	}
	return b.String()
}

func ingestOneLLMCall(t *testing.T, assemble bool, maxResp int, call map[string]any) (stored string, truncated bool, mcp []string) {
	t.Helper()
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	d.LLMCapture = config.LLMCapture{StoreBodies: true, MaxRequestBytes: 1000, MaxResponseBytes: maxResp, AssembleStreams: assemble}
	call["timestamp"], call["request_id"], call["status_code"] = "2026-01-15T12:00:00Z", "r1", 200
	payload, _ := json.Marshal(map[string]any{"schema_version": 1, "llm_calls": []map[string]any{call}})
	doIngest(t, d, ingestRequestWithPrincipal(string(payload), "tenant-a", false))
	if len(sinkFake.gotLLM) != 1 {
		t.Fatalf("got %d llm calls", len(sinkFake.gotLLM))
	}
	l := sinkFake.gotLLM[0]
	return string(l.ResponseBody), l.Truncated, l.MCPToolsUsed
}

func TestIngest_StreamedResponseStoredAssembled(t *testing.T) {
	raw := ingestStreamBody(ingestStreamEvents)
	cut := ingestStreamBody(ingestStreamEvents[:len(ingestStreamEvents)-1])
	plain := `{"id":"msg_test_plain","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`
	cases := []struct {
		name      string
		assemble  bool
		maxResp   int
		call      map[string]any
		want      string
		truncated bool
	}{
		{"stream flag set", true, 1000, map[string]any{"stream": true, "response_body": raw}, ingestAssembled, false},
		{"stream flag omitted, body is SSE", true, 1000, map[string]any{"response_body": raw}, ingestAssembled, false},
		{"switch off", false, 10000, map[string]any{"stream": true, "response_body": raw}, raw, false},
		{"stream cut short", true, 10000, map[string]any{"stream": true, "response_body": cut}, cut, false},
		{"non-stream body", true, 1000, map[string]any{"response_body": plain}, plain, false},
		// The cap applies to what is stored: the stream is longer than it,
		// the message is not.
		{"assembled before the cap", true, len(ingestAssembled), map[string]any{"stream": true, "response_body": raw}, ingestAssembled, false},
		{"message over the cap is cut", true, 40, map[string]any{"stream": true, "response_body": raw}, ingestAssembled[:40], true},
		{"raw stream over the cap is cut", false, 40, map[string]any{"stream": true, "response_body": raw}, raw[:40], true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, truncated, mcp := ingestOneLLMCall(t, c.assemble, c.maxResp, c.call)
			if got != c.want || truncated != c.truncated {
				t.Errorf("stored (truncated=%v) %q\nwant (truncated=%v) %q", truncated, got, c.truncated, c.want)
			}
			// Discovery reads the response as received, whatever is stored.
			if body, _ := c.call["response_body"].(string); body == raw && (len(mcp) != 1 || mcp[0] != "demo__lookup") {
				t.Errorf("mcp tools used = %v, want [demo__lookup]", mcp)
			}
		})
	}
}
