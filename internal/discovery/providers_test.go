package discovery

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"reflect"
	"strings"
	"testing"
)

// esFrameBytes hand-builds one AWS event-stream message (string headers,
// CRC32 prelude and message checksums), the wire format Bedrock streams use:
// https://docs.aws.amazon.com/transcribe/latest/dg/event-stream.html
func esFrameBytes(msgType, evType string, payload []byte) []byte {
	var h []byte
	for _, kv := range [][2]string{{":message-type", msgType}, {":event-type", evType}, {":content-type", "application/json"}} {
		h = append(h, byte(len(kv[0])))
		h = append(h, kv[0]...)
		h = append(h, 7, byte(len(kv[1])>>8), byte(len(kv[1])))
		h = append(h, kv[1]...)
	}
	total := 12 + len(h) + len(payload) + 4
	f := make([]byte, 12, total)
	binary.BigEndian.PutUint32(f[0:], uint32(total))
	binary.BigEndian.PutUint32(f[4:], uint32(len(h)))
	binary.BigEndian.PutUint32(f[8:], crc32.ChecksumIEEE(f[:8]))
	f = append(f, h...)
	f = append(f, payload...)
	return binary.BigEndian.AppendUint32(f, crc32.ChecksumIEEE(f))
}

// chunkFrame wraps an Anthropic stream event the way InvokeModelWithResponseStream
// does: {"bytes": "<base64 of the event JSON>"}.
func chunkFrame(event string) []byte {
	p := fmt.Sprintf(`{"bytes":"%s"}`, base64.StdEncoding.EncodeToString([]byte(event)))
	return esFrameBytes("event", "chunk", []byte(p))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Bedrock InvokeModel (non-stream) on an Anthropic model returns the
// Anthropic Messages body:
// https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-anthropic-claude-messages.html
const bedrockInvokeJSON = `{"id":"msg_bdrk_01","type":"message","role":"assistant","model":"claude-sonnet-4-20250514",
"content":[{"type":"tool_use","id":"toolu_bdrk_1","name":"Skill","input":{"skill":"Deploy"}},
{"type":"tool_use","id":"toolu_bdrk_2","name":"mcp__Jira__create_issue","input":{}}],
"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`

// Bedrock Converse response, ToolUseBlock under output.message.content:
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ToolUseBlock.html
const converseJSON = `{"output":{"message":{"role":"assistant","content":[
{"text":"checking"},
{"toolUse":{"toolUseId":"tooluse_1","name":"Skill","input":{"skill":"/Lint"}}},
{"toolUse":{"toolUseId":"tooluse_2","name":"mcp__gh__list_prs","input":{"repo":"x"}}}]}},
"stopReason":"tool_use","usage":{"inputTokens":3,"outputTokens":4,"totalTokens":7},"metrics":{"latencyMs":50}}`

// Gemini generateContent, functionCall part {name, args(object)}:
// https://ai.google.dev/api/caching#FunctionCall
const geminiJSON = `{"candidates":[{"content":{"role":"model","parts":[
{"text":"ok"},
{"functionCall":{"name":"Skill","args":{"skill":"Review-PR"}}},
{"functionCall":{"name":"mcp__linear__get_issue","args":{"id":"A-1"}}},
{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP","index":0}],
"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3,"totalTokenCount":12}}`

// streamGenerateContent?alt=sse: one data: line per chunk.
const geminiSSE = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}` + "\r\n\r\n" +
	`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"Skill","args":{"skill":"commit"}}}]},"index":0}]}` + "\r\n\r\n" +
	`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"mcp__fs__read_file","args":{"p":"/a"}}}]},"finishReason":"STOP","index":0}]}` + "\r\n\r\n"

// streamGenerateContent without alt=sse: a JSON array of generateContent objects.
const geminiArray = `[{
  "candidates": [{"content": {"role": "model", "parts": [{"text": "x"}]}}]
},
{
  "candidates": [{"content": {"role": "model", "parts": [{"functionCall": {"name": "Skill", "args": {"skill": "commit"}}}]}}]
},
{
  "candidates": [{"content": {"role": "model", "parts": [{"functionCall": {"name": "mcp__fs__read_file", "args": {"p": "/a"}}}]}, "finishReason": "STOP"}]
}
]`

// OpenAI Responses API output items (function_call, mcp_call):
// https://developers.openai.com/api/docs/guides/function-calling
const responsesJSON = `{"id":"resp_1","object":"response","status":"completed","output":[
{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}]},
{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Skill","arguments":"{\"skill\":\"Deploy\"}"},
{"id":"fc_2","type":"function_call","call_id":"call_2","name":"mcp__gw__langfuse__get_trace","arguments":"{}"},
{"id":"mcp_1","type":"mcp_call","server_label":"DeepWiki","name":"ask_question","arguments":"{\"q\":\"x\"}","output":"y"}],
"usage":{"input_tokens":1,"output_tokens":2}}`

// Responses stream events (same URL): arguments split across deltas, an
// mcp_call item, and a plain function.
const responsesSSE = "event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
	`data: {"type":"response.output_item.added","response_id":"resp_1","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Skill","arguments":""}}` + "\n\n" +
	`data: {"type":"response.function_call_arguments.delta","response_id":"resp_1","item_id":"fc_1","output_index":0,"delta":"{\"ski"}` + "\n\n" +
	`data: {"type":"response.function_call_arguments.delta","response_id":"resp_1","item_id":"fc_1","output_index":0,"delta":"ll\":\"Dep"}` + "\n\n" +
	`data: {"type":"response.function_call_arguments.delta","response_id":"resp_1","item_id":"fc_1","output_index":0,"delta":"loy\"}"}` + "\n\n" +
	`data: {"type":"response.function_call_arguments.done","response_id":"resp_1","item_id":"fc_1","output_index":0,"arguments":"{\"skill\":\"Deploy\"}"}` + "\n\n" +
	`data: {"type":"response.output_item.done","response_id":"resp_1","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Skill","arguments":"{\"skill\":\"Deploy\"}"}}` + "\n\n" +
	`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"mcp__gw__langfuse__get_trace","arguments":""}}` + "\n\n" +
	`data: {"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":1,"delta":"{}"}` + "\n\n" +
	`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"mcp__gw__langfuse__get_trace","arguments":"{}"}}` + "\n\n" +
	`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"mcp_call","id":"mcp_1","server_label":"DeepWiki","name":"ask_question","arguments":""}}` + "\n\n" +
	`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"mcp_call","id":"mcp_1","server_label":"DeepWiki","name":"ask_question","arguments":"{}","output":"z"}}` + "\n\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1"}}` + "\n\n"

// Anthropic events as they sit inside Bedrock chunk frames.
var bedrockEvents = []string{
	`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Skill","input":{}}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"ski"}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ll\":\"Dep"}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"loy\"}"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t2","name":"mcp__gw__langfuse__get_trace","input":{}}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
}

func bedrockStream() []byte {
	var out []byte
	for _, e := range bedrockEvents {
		out = append(out, chunkFrame(e)...)
	}
	return out
}

// ConverseStream: event payloads are the member objects, event type is in
// the :event-type header:
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html
func converseStream() []byte {
	return concat(
		esFrameBytes("event", "messageStart", []byte(`{"role":"assistant"}`)),
		esFrameBytes("event", "contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"text":"hi"}}`)),
		esFrameBytes("event", "contentBlockStop", []byte(`{"contentBlockIndex":0}`)),
		esFrameBytes("event", "contentBlockStart", []byte(`{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tu_1","name":"Skill"}}}`)),
		esFrameBytes("event", "contentBlockDelta", []byte(`{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"skill\""}}}`)),
		esFrameBytes("event", "contentBlockDelta", []byte(`{"contentBlockIndex":1,"delta":{"toolUse":{"input":":\"Lint\"}"}}}`)),
		esFrameBytes("event", "contentBlockStop", []byte(`{"contentBlockIndex":1}`)),
		esFrameBytes("event", "contentBlockStart", []byte(`{"contentBlockIndex":2,"start":{"toolUse":{"toolUseId":"tu_2","name":"mcp__gh__list_prs"}}}`)),
		esFrameBytes("event", "contentBlockDelta", []byte(`{"contentBlockIndex":2,"delta":{"toolUse":{"input":"{}"}}}`)),
		esFrameBytes("event", "contentBlockStop", []byte(`{"contentBlockIndex":2}`)),
		esFrameBytes("event", "messageStop", []byte(`{"stopReason":"tool_use"}`)),
	)
}

func TestScan_Providers(t *testing.T) {
	skillDeploy := Result{Skills: []string{"deploy"}, MCPTools: []string{"gw__langfuse__get_trace"}}
	tests := []struct {
		name string
		body []byte
		want Result
	}{
		{"bedrock invoke json", []byte(bedrockInvokeJSON), Result{Skills: []string{"deploy"}, MCPTools: []string{"jira__create_issue"}}},
		{"bedrock invoke-with-response-stream", bedrockStream(), skillDeploy},
		{"bedrock converse json", []byte(converseJSON), Result{Skills: []string{"lint"}, MCPTools: []string{"gh__list_prs"}}},
		{"bedrock converse-stream", converseStream(), Result{Skills: []string{"lint"}, MCPTools: []string{"gh__list_prs"}}},
		{"gemini json", []byte(geminiJSON), Result{Skills: []string{"review-pr"}, MCPTools: []string{"linear__get_issue"}}},
		{"gemini sse", []byte(geminiSSE), Result{Skills: []string{"commit"}, MCPTools: []string{"fs__read_file"}}},
		{"gemini json array stream", []byte(geminiArray), Result{Skills: []string{"commit"}, MCPTools: []string{"fs__read_file"}}},
		{"responses json", []byte(responsesJSON), Result{Skills: []string{"deploy"}, MCPTools: []string{"gw__langfuse__get_trace", "deepwiki__ask_question"}}},
		{"responses stream", []byte(responsesSSE), Result{Skills: []string{"deploy"}, MCPTools: []string{"gw__langfuse__get_trace", "deepwiki__ask_question"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Scan(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("whole: got %+v want %+v", got, tc.want)
			}
			// The result must not depend on how the relay chunked the body.
			for i := 1; i < len(tc.body); i++ {
				s := NewScanner()
				_, _ = s.Write(tc.body[:i])
				_, _ = s.Write(tc.body[i:])
				if got := s.Result(); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("split at %d: got %+v want %+v", i, got, tc.want)
				}
			}
			// One byte at a time.
			s := NewScanner()
			for i := range tc.body {
				_, _ = s.Write(tc.body[i : i+1])
			}
			if got := s.Result(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("bytewise: got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestScan_NoToolCalls(t *testing.T) {
	for name, body := range map[string][]byte{
		"gemini plain fn": []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{}}}]}}]}`),
		"responses plain": []byte(`{"output":[{"type":"function_call","name":"search","arguments":"{}"}]}`),
		"converse text":   []byte(`{"output":{"message":{"content":[{"text":"hi"}]}}}`),
		"es no tools":     chunkFrame(`{"type":"message_start","message":{"id":"m"}}`),
		"skill no input":  []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"Skill","args":{}}}]}}]}`),
	} {
		if got := Scan(body); !got.Empty() {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}

func TestScan_EventStreamMalformed(t *testing.T) {
	good := chunkFrame(bedrockEvents[3])
	// A frame with a bad message CRC is skipped (its length is trustworthy,
	// the bytes after it still parse).
	bad := append([]byte(nil), chunkFrame(`{"type":"content_block_start","index":9,"content_block":{"type":"tool_use","name":"mcp__bad__tool"}}`)...)
	bad[len(bad)-1] ^= 0xff
	stream := concat(bad, bedrockStream())
	if got := Scan(stream); !reflect.DeepEqual(got, Result{Skills: []string{"deploy"}, MCPTools: []string{"gw__langfuse__get_trace"}}) {
		t.Errorf("bad message CRC: got %+v", got)
	}
	// A corrupted prelude loses framing: nothing after it is read, nothing panics.
	corrupt := append([]byte(nil), bedrockStream()...)
	corrupt[9] ^= 0xff
	if got := Scan(corrupt); !got.Empty() {
		t.Errorf("bad prelude CRC: got %+v", got)
	}
	// A chunk whose base64 or JSON is broken is ignored, later frames still count.
	junk := concat(
		esFrameBytes("event", "chunk", []byte(`{"bytes":"!!!not-base64"}`)),
		esFrameBytes("event", "chunk", []byte(`not json`)),
		esFrameBytes("exception", "throttlingException", []byte(`{"message":"slow down"}`)),
		bedrockStream(),
	)
	if got := Scan(junk); len(got.Skills) != 1 {
		t.Errorf("junk frames: got %+v", got)
	}
	// Truncated stream: a tool block never closed is still recorded; a cut frame is dropped.
	tr := concat(good, chunkFrame(bedrockEvents[4]))
	if got := Scan(tr[:len(tr)-5]); !got.Empty() {
		t.Errorf("truncated: got %+v", got) // skill input incomplete -> no name
	}
}

func TestScan_ProviderBounds(t *testing.T) {
	// More than MaxSkills / MaxMCPRefs distinct names in one Gemini response.
	var parts []string
	for i := 0; i < MaxSkills+20; i++ {
		parts = append(parts, fmt.Sprintf(`{"functionCall":{"name":"Skill","args":{"skill":"s%d"}}}`, i))
	}
	for i := 0; i < MaxMCPRefs+20; i++ {
		parts = append(parts, fmt.Sprintf(`{"functionCall":{"name":"mcp__srv__t%d","args":{}}}`, i))
	}
	got := Scan([]byte(`{"candidates":[{"content":{"parts":[` + strings.Join(parts, ",") + `]}}]}`))
	if len(got.Skills) != MaxSkills || len(got.MCPTools) != MaxMCPRefs {
		t.Errorf("caps: %d skills, %d mcp", len(got.Skills), len(got.MCPTools))
	}

	// 64 KiB input cap: a Skill whose arguments exceed it is dropped, in every shape.
	pad := strings.Repeat("a", maxInputPerBlock)
	big := fmt.Sprintf(`{"skill":"big","pad":"%s"}`, pad)
	if r := Scan([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"Skill","args":` + big + `}}]}}]}`)); !r.Empty() {
		t.Errorf("gemini oversized args: %+v", r)
	}
	esc := strings.ReplaceAll(big, `"`, `\"`)
	if r := Scan([]byte(`{"output":[{"type":"function_call","name":"Skill","arguments":"` + esc + `"}]}`)); !r.Empty() {
		t.Errorf("responses json oversized args: %+v", r)
	}
	var sse strings.Builder
	sse.WriteString(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc","name":"Skill","arguments":""}}` + "\n\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&sse, `data: {"type":"response.function_call_arguments.delta","item_id":"fc","output_index":0,"delta":"%s"}`+"\n\n", strings.Repeat("a", 30000))
	}
	sse.WriteString(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc","name":"Skill","arguments":""}}` + "\n\n")
	if r := Scan([]byte(sse.String())); !r.Empty() {
		t.Errorf("responses stream oversized args: %+v", r)
	}
	// Converse stream over the cap.
	cs := esFrameBytes("event", "contentBlockStart", []byte(`{"contentBlockIndex":0,"start":{"toolUse":{"name":"Skill"}}}`))
	for i := 0; i < 3; i++ {
		cs = append(cs, esFrameBytes("event", "contentBlockDelta", []byte(fmt.Sprintf(`{"contentBlockIndex":0,"delta":{"toolUse":{"input":"%s"}}}`, strings.Repeat("a", 30000))))...)
	}
	cs = append(cs, esFrameBytes("event", "contentBlockStop", []byte(`{"contentBlockIndex":0}`))...)
	if r := Scan(cs); !r.Empty() {
		t.Errorf("converse oversized args: %+v", r)
	}
	// An oversized event-stream frame is skipped without losing sync.
	huge := esFrameBytes("event", "chunk", []byte(`{"bytes":"`+strings.Repeat("A", maxFrameBytes)+`"}`))
	if r := Scan(concat(huge, bedrockStream())); len(r.Skills) != 1 {
		t.Errorf("after oversized frame: %+v", r)
	}
}
