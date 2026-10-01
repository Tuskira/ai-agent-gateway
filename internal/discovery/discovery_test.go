package discovery

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const anthropicJSON = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4",
"content":[{"type":"text","text":"on it"},
{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"  Review-PR "}},
{"type":"tool_use","id":"toolu_2","name":"mcp__langfuse__get_trace","input":{"id":"t1"}},
{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"ls"}}],
"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`

func TestScan_AnthropicJSON(t *testing.T) {
	got := Scan([]byte(anthropicJSON))
	want := Result{Skills: []string{"review-pr"}, MCPTools: []string{"langfuse__get_trace"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Skill","input":{}}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"ski"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ll\": \"com"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"mit\"}"}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t2","name":"mcp__gw__langfuse__get_trace","input":{}}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":2}` + "\n\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n\n"

func TestScan_AnthropicSSE(t *testing.T) {
	want := Result{Skills: []string{"commit"}, MCPTools: []string{"gw__langfuse__get_trace"}}
	if got := Scan([]byte(anthropicSSE)); !reflect.DeepEqual(got, want) {
		t.Fatalf("whole: got %+v want %+v", got, want)
	}
	// Split at every possible byte boundary, in two chunks: the result must
	// not depend on how the relay chunked the stream.
	for i := 1; i < len(anthropicSSE); i++ {
		s := NewScanner()
		_, _ = s.Write([]byte(anthropicSSE[:i]))
		_, _ = s.Write([]byte(anthropicSSE[i:]))
		if got := s.Result(); !reflect.DeepEqual(got, want) {
			t.Fatalf("split at %d: got %+v want %+v", i, got, want)
		}
	}
}

func TestScan_SSEWithoutTrailingNewlineAndUnstoppedBlock(t *testing.T) {
	sse := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"mcp__fs__read"}}`
	if got := Scan([]byte(sse)); !reflect.DeepEqual(got.MCPTools, []string{"fs__read"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestScan_OpenAIJSON(t *testing.T) {
	body := `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
{"id":"a","type":"function","function":{"name":"Skill","arguments":"{\"command\":\"/Deploy\"}"}},
{"id":"b","type":"function","function":{"name":"mcp__github__create_issue","arguments":"{}"}}]}}]}`
	want := Result{Skills: []string{"deploy"}, MCPTools: []string{"github__create_issue"}}
	if got := Scan([]byte(body)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestScan_OpenAIStream(t *testing.T) {
	sse := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"a","function":{"name":"Skill","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"name\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"lint\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"mcp__slack__post","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	want := Result{Skills: []string{"lint"}, MCPTools: []string{"slack__post"}}
	if got := Scan([]byte(sse)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSplitMCPName(t *testing.T) {
	cases := []struct {
		in           string
		server, tool string
		ok           bool
	}{
		{"mcp__langfuse__get_trace", "langfuse", "get_trace", true},
		{"mcp__gw__langfuse__get_trace", "gw", "langfuse__get_trace", true},
		{"mcp__Claude_AI__Search", "claude_ai", "Search", true},
		{"mcp__only", "", "", false},
		{"mcp____tool", "", "", false},
		{"mcp__srv__", "", "", false},
		{"Bash", "", "", false},
		{"mcp__" + strings.Repeat("s", 129) + "__t", "", "", false},
	}
	for _, c := range cases {
		s, tl, ok := SplitMCPName(c.in)
		if s != c.server || tl != c.tool || ok != c.ok {
			t.Errorf("%q: got (%q,%q,%v)", c.in, s, tl, ok)
		}
	}
}

func TestScan_Bounds(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"content":[`)
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, `{"type":"tool_use","name":"Skill","input":{"skill":"s%d"}},`, i)
	}
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&b, `{"type":"tool_use","name":"mcp__srv__t%d","input":{}},`, i)
	}
	// duplicates collapse
	b.WriteString(`{"type":"tool_use","name":"Skill","input":{"skill":"s0"}},`)
	fmt.Fprintf(&b, `{"type":"tool_use","name":"Skill","input":{"skill":"%s"}}`, strings.Repeat("x", 129))
	b.WriteString(`]}`)
	got := Scan([]byte(b.String()))
	if len(got.Skills) != MaxSkills || len(got.MCPTools) != MaxMCPRefs {
		t.Fatalf("skills=%d mcp=%d", len(got.Skills), len(got.MCPTools))
	}
	if got.Skills[0] != "s0" || got.MCPTools[0] != "srv__t0" {
		t.Fatalf("first-seen order lost: %v %v", got.Skills[0], got.MCPTools[0])
	}
}

func TestScan_OversizedInputBlock(t *testing.T) {
	// A Skill input beyond the per-block bound is dropped, an MCP name
	// (which needs no input) still counts, and nothing panics.
	var b strings.Builder
	b.WriteString(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"Skill"}}` + "\n")
	chunk := strings.Repeat("a", 8<<10)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"%s"}}`+"\n", chunk)
	}
	b.WriteString(`data: {"type":"content_block_stop","index":0}` + "\n")
	b.WriteString(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"mcp__a__b"}}` + "\n")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"%s"}}`+"\n", chunk)
	}
	b.WriteString(`data: {"type":"content_block_stop","index":1}` + "\n")
	got := Scan([]byte(b.String()))
	if len(got.Skills) != 0 || !reflect.DeepEqual(got.MCPTools, []string{"a__b"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestScan_Malformed(t *testing.T) {
	for _, in := range []string{
		"", "   ", "not json", "{", `{"content":"tool_use"}`, `{"content":[{"type":"tool_use","name":"Skill","input":"x"}]}`,
		`{"content":[{"type":"tool_use","name":"Skill","input":{"skill":5}}]}`,
		`{"content":[{"type":"tool_use","name":"Skill","input":{}}]}`,
		"data: {not json tool_use\n\n", "data: [DONE]\n\n", ": keepalive\n\n",
		`data: {"type":"content_block_delta","index":9,"delta":{"type":"input_json_delta","partial_json":"x"}}` + "\n",
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"Skill","arguments":"{{"}}]}}]}`,
	} {
		if got := Scan([]byte(in)); !got.Empty() {
			t.Errorf("%q: expected nothing, got %+v", in, got)
		}
	}
}

func TestScanner_WriteNeverFails(t *testing.T) {
	s := NewScanner()
	in := []byte("data: {\"type\":\"content_block_start\"\n")
	n, err := s.Write(in)
	if n != len(in) || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	// Writes after Result are accepted and ignored.
	s.Result()
	if n, err := s.Write([]byte("xyz")); n != 3 || err != nil {
		t.Fatalf("after result: n=%d err=%v", n, err)
	}
}

func TestScan_HugeSSELineIgnored(t *testing.T) {
	huge := "data: " + strings.Repeat("x", 2<<20) + "\n"
	sse := huge + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"mcp__a__b"}}` + "\n"
	if got := Scan([]byte(sse)); !reflect.DeepEqual(got.MCPTools, []string{"a__b"}) {
		t.Fatalf("got %+v", got)
	}
}
