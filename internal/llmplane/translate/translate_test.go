package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/anthropic"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/openaicompat"
)

// These tests pin the Anthropic <-> Chat Completions translation (the
// anthropic Dialect composed with the openai_compat Provider) below the
// capability check; check_test.go covers what the engine refuses first.

var (
	dialect  = anthropic.Dialect{}
	provider = openaicompat.Provider{}
)

// anthropicToChat parses an Anthropic body and builds the Chat body for
// model; echo selects a vendor that wants its reasoning back.
func anthropicToChat(body []byte, model string, echo bool) ([]byte, bool, error) {
	req, err := dialect.ParseRequest(httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body)))
	if err != nil {
		return nil, false, err
	}
	vendor := "openai"
	if echo {
		vendor = "deepseek"
	}
	up, err := provider.BuildRequest(context.Background(), req, llm.Target{Vendor: vendor, BaseURL: "https://x.test/v1", Model: model})
	if err != nil {
		return nil, false, err
	}
	out, _ := io.ReadAll(up.Body)
	return out, req.Stream, nil
}

// chatToAnthropic parses a Chat response and renders the Anthropic message
// for the client-facing model.
func chatToAnthropic(body []byte, model string) ([]byte, error) {
	r, err := provider.ParseResponse(&http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))})
	if err != nil {
		return nil, err
	}
	r.Model = model
	return dialect.RenderResponse(r)
}

func chatErrorToAnthropic(status int, body []byte) []byte {
	return dialect.RenderError(provider.ParseError(status, body))
}

// streamChatToAnthropic runs the engine's stream translation.
func streamChatToAnthropic(dst io.Writer, src io.Reader, model string) error {
	c := &Call{Request: &llm.Request{Model: model}, d: dialect, p: provider}
	return c.stream(dst, src)
}

// jsonEqual compares two JSON documents structurally.
func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON %s: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestTranslateAnthropicToChat(t *testing.T) {
	cases := []struct {
		name, model, in, want string
		stream, echo          bool // echo: route wants reasoning back
	}{
		{
			name:  "system string and user string are byte-preserved",
			model: "glm-4.6",
			in:    `{"model":"x","max_tokens":100,"system":"  be brief  ","messages":[{"role":"user","content":" hi "}]}`,
			want:  `{"model":"glm-4.6","max_tokens":100,"messages":[{"role":"system","content":"  be brief  "},{"role":"user","content":" hi "}]}`,
		},
		{
			name:  "system blocks joined, attribution skipped, cache_control ignored (drop list)",
			model: "m",
			in: `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=1; cch=abc;"},
				{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}},{"type":"text","text":" rules "}],
				"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"}]}]}`,
			want: `{"model":"m","messages":[{"role":"system","content":"You are Claude Code.\n rules "},{"role":"user","content":"a\nb"}]}`,
		},
		{
			name:  "system only",
			model: "m",
			in:    `{"system":"s","messages":[]}`,
			want:  `{"model":"m","messages":[{"role":"system","content":"s"}]}`,
		},
		{
			name:  "all messages empty falls back to one empty user",
			model: "m",
			in:    `{"messages":[{"role":"user","content":"   "},{"role":"assistant","content":[]}]}`,
			want:  `{"model":"m","messages":[{"role":"user","content":""}]}`,
		},
		{
			name:  "mid-conversation system role becomes user",
			model: "m",
			in:    `{"messages":[{"role":"user","content":"a"},{"role":"system","content":"note"}]}`,
			want:  `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"user","content":"note"}]}`,
		},
		{
			name:  "images: base64 data URI, default media type, url, non-image dropped",
			model: "m",
			in: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":" QUJD "}},
				{"type":"image","source":{"type":"base64","data":"WFla"}},
				{"type":"image","source":{"type":"url","url":"https://e.x/a.png"}},
				{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}},
				{"type":"image","source":{"type":"file","file_id":"f1"}}]}]}`,
			want: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,WFla"}},
				{"type":"image_url","image_url":{"url":"https://e.x/a.png"}}]}]}`,
		},
		{
			name:  "documents: text source kept, pdf dropped; unknown blocks dropped",
			model: "m",
			in: `{"messages":[{"role":"user","content":[
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"doc body"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}},
				{"type":"search_result","content":[]},
				{"type":"text","text":"q"}]}]}`,
			want: `{"model":"m","messages":[{"role":"user","content":"doc body\nq"}]}`,
		},
		{
			name:  "tool results come first, in block order, then remaining user text",
			model: "m",
			in: `{"messages":[{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"A","content":"  raw  "},
				{"type":"text","text":"x"},
				{"type":"tool_result","tool_use_id":"B","content":[{"type":"text","text":"b1"},{"type":"text","text":"b2"}]},
				{"type":"tool_result","content":"no id"}]}]}`,
			want: `{"model":"m","messages":[
				{"role":"tool","tool_call_id":"A","content":"  raw  "},
				{"role":"tool","tool_call_id":"B","content":"b1\nb2"},
				{"role":"user","content":"x"}]}`,
		},
		{
			name:  "tool_result variants: error, image hoisted, tool_reference, scalar, empty",
			model: "m",
			in: `{"messages":[{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"e","is_error":true,"content":"boom"},
				{"type":"tool_result","tool_use_id":"i","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"UE5H"}}]},
				{"type":"tool_result","tool_use_id":"r","content":[{"type":"tool_reference","tool_name":"mcp__x__y"}]},
				{"type":"tool_result","tool_use_id":"n","content":42},
				{"type":"tool_result","tool_use_id":"z"}]}]}`,
			want: `{"model":"m","messages":[
				{"role":"tool","tool_call_id":"e","content":"Error: boom"},
				{"role":"tool","tool_call_id":"i","content":"(image attached in the next message)"},
				{"role":"tool","tool_call_id":"r","content":"Tools loaded: mcp__x__y"},
				{"role":"tool","tool_call_id":"n","content":"42"},
				{"role":"tool","tool_call_id":"z","content":""},
				{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,UE5H"}}]}]}`,
		},
		{
			name:  "assistant: text, tool calls, unsigned thinking passed back, signed and redacted dropped",
			model: "deepseek-chat",
			echo:  true,
			in: `{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan","signature":""},
				{"type":"thinking","thinking":"claude thought","signature":"EqQB"},
				{"type":"redacted_thinking","data":"xyz"},
				{"type":"text","text":" doing it "},
				{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/a"}},
				{"type":"tool_use","id":"t2","name":"Raw","input":"{\"k\":1}"},
				{"type":"tool_use","id":"t3","name":"NoInput"},
				{"type":"tool_use","name":"NoID","input":{}},
				{"type":"server_tool_use","id":"s1","name":"web_search","input":{}}]}]}`,
			want: `{"model":"deepseek-chat","messages":[{"role":"user","content":"go"},{"role":"assistant","content":" doing it ","reasoning_content":"plan","tool_calls":[
				{"id":"t1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/a\"}"}},
				{"id":"t2","type":"function","function":{"name":"Raw","arguments":"{\"k\":1}"}},
				{"id":"t3","type":"function","function":{"name":"NoInput","arguments":"{}"}}]}]}`,
		},
		{
			name:  "reasoning is not sent back to a target that did not ask for it",
			model: "openai/gpt-oss-120b",
			in: `{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan","signature":""},{"type":"text","text":"ok"}]}]}`,
			want: `{"model":"openai/gpt-oss-120b","messages":[{"role":"user","content":"go"},{"role":"assistant","content":"ok"}]}`,
		},
		{
			name:  "assistant tool calls without text get empty content; thinking-only turn skipped",
			model: "m",
			in: `{"messages":[{"role":"user","content":"a"},
				{"role":"assistant","content":[{"type":"thinking","thinking":"only thought","signature":""}]},
				{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]}]}`,
			want: `{"model":"m","messages":[{"role":"user","content":"a"},
				{"role":"assistant","content":"","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		},
		{
			name:  "tools: custom kept, missing schema defaulted, typed and server tools dropped",
			model: "m",
			in: `{"messages":[{"role":"user","content":"a"}],"tools":[
				{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"p":{"type":"string"}}},"cache_control":{"type":"ephemeral"},"defer_loading":true,"strict":true},
				{"type":"custom","name":"NoSchema"},
				{"type":"web_search_20250305","name":"web_search","max_uses":5},
				{"type":"bash_20250124","name":"bash"},
				{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"}],
				"tool_choice":{"type":"any","disable_parallel_tool_use":true}}`,
			want: `{"model":"m","messages":[{"role":"user","content":"a"}],"tools":[
				{"type":"function","function":{"name":"Read","description":"read a file","parameters":{"type":"object","properties":{"p":{"type":"string"}}}}},
				{"type":"function","function":{"name":"NoSchema","parameters":{"type":"object","properties":{}}}}],
				"tool_choice":"required","parallel_tool_calls":false}`,
		},
		{
			name:  "tool_choice omitted when every tool was dropped",
			model: "m",
			in:    `{"messages":[{"role":"user","content":"a"}],"tools":[{"type":"web_search_20250305","name":"web_search"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`,
			want:  `{"model":"m","messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "reasoning model: max_completion_tokens, effort clamped, default sampling omitted",
			model: "gpt-5",
			in:    `{"messages":[{"role":"user","content":"a"}],"max_tokens":32000,"temperature":1,"top_p":1,"top_k":5,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"metadata":{"user_id":"u_session_1"}}`,
			want:  `{"model":"gpt-5","messages":[{"role":"user","content":"a"}],"max_completion_tokens":32000,"reasoning_effort":"high"}`,
		},
		{
			name:  "o-series behind a vendor prefix",
			model: "openai/o3-mini",
			in:    `{"messages":[{"role":"user","content":"a"}],"max_tokens":10,"output_config":{"effort":"low"}}`,
			want:  `{"model":"openai/o3-mini","messages":[{"role":"user","content":"a"}],"max_completion_tokens":10,"reasoning_effort":"low"}`,
		},
		{
			name:  "other models: max_tokens and sampling kept, effort and thinking dropped",
			model: "grok-4",
			in:    `{"messages":[{"role":"user","content":"a"}],"max_tokens":10,"temperature":0,"top_p":0.5,"stop_sequences":["END"],"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"high"}}`,
			want:  `{"model":"grok-4","messages":[{"role":"user","content":"a"}],"max_tokens":10,"temperature":0,"top_p":0.5,"stop":["END"]}`,
		},
		{
			name:   "stream asks for usage",
			model:  "m",
			in:     `{"messages":[{"role":"user","content":"a"}],"stream":true}`,
			want:   `{"model":"m","messages":[{"role":"user","content":"a"}],"stream":true,"stream_options":{"include_usage":true}}`,
			stream: true,
		},
		{
			name:  "realistic Claude Code turn",
			model: "kimi-k2",
			in: `{"model":"claude-sonnet-4-5","max_tokens":32000,"stream":true,"metadata":{"user_id":"user_x_account_y_session_z"},
				"thinking":{"type":"adaptive"},"context_management":{"edits":[]},"output_config":{"effort":"medium"},
				"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.0;"},{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
				"tools":[{"name":"Bash","description":"Run a command","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
				"messages":[
					{"role":"user","content":[{"type":"text","text":"list files"}]},
					{"role":"assistant","content":[{"type":"text","text":"Listing."},{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"ls"}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"a.go\nb.go"}]},{"type":"text","text":"<system-reminder>ok</system-reminder>","cache_control":{"type":"ephemeral"}}]}]}`,
			want: `{"model":"kimi-k2","max_tokens":32000,"stream":true,"stream_options":{"include_usage":true},
				"messages":[
					{"role":"system","content":"You are Claude Code."},
					{"role":"user","content":"list files"},
					{"role":"assistant","content":"Listing.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]},
					{"role":"tool","tool_call_id":"call_1","content":"a.go\nb.go"},
					{"role":"user","content":"<system-reminder>ok</system-reminder>"}],
				"tools":[{"type":"function","function":{"name":"Bash","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]}`,
			stream: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, stream, err := anthropicToChat([]byte(c.in), c.model, c.echo)
			if err != nil {
				t.Fatalf("anthropicToChat: %v", err)
			}
			if stream != c.stream {
				t.Errorf("stream = %v, want %v", stream, c.stream)
			}
			jsonEqual(t, out, c.want)
		})
	}
}

func TestTranslateToolChoice(t *testing.T) {
	cases := map[string]string{
		`{"type":"auto"}`:               `"auto"`,
		`{"type":"any"}`:                `"required"`,
		`{"type":"none"}`:               `"none"`,
		`{"type":"tool","name":"Read"}`: `{"type":"function","function":{"name":"Read"}}`,
		`{"type":"mystery"}`:            ``,
	}
	for in, want := range cases {
		body := `{"messages":[{"role":"user","content":"a"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}],"tool_choice":` + in + `}`
		out, _, err := anthropicToChat([]byte(body), "m", false)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		_ = json.Unmarshal(out, &got)
		if want == "" {
			if got.ToolChoice != nil {
				t.Errorf("%s: tool_choice = %s, want omitted", in, got.ToolChoice)
			}
			continue
		}
		jsonEqual(t, got.ToolChoice, want)
	}
}

// Kimi and GLM get their earlier reasoning back by model id, on a host and
// label that do not name them (Nebius, Together): BuildRequest passes the
// target's model on.
func TestTranslateEchoesReasoningByModel(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[
		{"type":"thinking","thinking":"plan","signature":""},
		{"type":"tool_use","id":"t1","name":"Read","input":{}}]}]}`)
	for model, want := range map[string]bool{"moonshotai/Kimi-K3": true, "zai-org/GLM-5.3": true, "gpt-4o": false} {
		out, _, err := anthropicToChat(in, model, false)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if got := bytes.Contains(out, []byte(`"reasoning_content":"plan"`)); got != want {
			t.Errorf("%s: reasoning_content sent = %v, want %v: %s", model, got, want, out)
		}
	}
}

func TestTranslateAnthropicToChatErrors(t *testing.T) {
	// OpenAI reasoning models take only the default sampling: another value is
	// refused, never silently changed.
	if _, _, err := anthropicToChat([]byte(`{"messages":[{"role":"user","content":"a"}],"top_p":0.5}`), "gpt-5", false); !errors.As(err, new(*llm.ErrUnsupported)) {
		t.Errorf("top_p on gpt-5: err = %v, want ErrUnsupported", err)
	}
	for _, in := range []string{
		`not json`,
		`{"messages":[]}`,
		`{"system":"  ","messages":[]}`,
		`{"messages":[{"role":"user","content":7}]}`,
		`{"system":5,"messages":[{"role":"user","content":"a"}]}`,
	} {
		if _, _, err := anthropicToChat([]byte(in), "m", false); err == nil {
			t.Errorf("%s: want error", in)
		}
	}
}

func TestTranslateChatToAnthropic(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "text with cached usage",
			in:   `{"id":"chatcmpl-1","model":"gpt-5","choices":[{"message":{"role":"assistant","content":"  hello  "},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}`,
			want: `{"id":"msg_chatcmpl-1","type":"message","role":"assistant","model":"client-model","content":[{"type":"text","text":"  hello  "}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":60,"output_tokens":7,"cache_read_input_tokens":40}}`,
		},
		{
			name: "tool calls: invalid and non-object args preserved under the marker key, empty is {}; nameless skipped; stop with tools is tool_use",
			in: `{"id":"msg_x","choices":[{"message":{"content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"p\":\"/a\"}"}},
				{"id":"call_2","type":"function","function":{"name":"Bad","arguments":"{not json"}},
				{"id":"call_3","type":"function","function":{"name":"Empty","arguments":""}},
				{"id":"call_4","type":"function","function":{"name":"Arr","arguments":"[1]"}},
				{"id":"call_5","type":"function","function":{"name":"Obj","arguments":{"k":1}}},
				{"id":"call_6","type":"function","function":{"arguments":"{}"}}]},"finish_reason":"stop"}]}`,
			want: `{"id":"msg_x","type":"message","role":"assistant","model":"client-model","content":[
				{"type":"tool_use","id":"call_1","name":"Read","input":{"p":"/a"}},
				{"type":"tool_use","id":"call_2","name":"Bad","input":{"_gateway_invalid_arguments":"{not json"}},
				{"type":"tool_use","id":"call_3","name":"Empty","input":{}},
				{"type":"tool_use","id":"call_4","name":"Arr","input":{"_gateway_invalid_arguments":"[1]"}},
				{"type":"tool_use","id":"call_5","name":"Obj","input":{"k":1}}],
				"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}`,
		},
		{
			name: "empty output keeps the real stop reason",
			in:   `{"id":"r","choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":100}}`,
			want: `{"id":"msg_r","type":"message","role":"assistant","model":"client-model","content":[{"type":"text","text":""}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":100,"cache_read_input_tokens":0}}`,
		},
		{
			name: "reasoning_content becomes a leading unsigned thinking block",
			in:   `{"id":"r","choices":[{"message":{"content":"answer","reasoning_content":"think hard"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_cache_hit_tokens":4}}`,
			want: `{"id":"msg_r","type":"message","role":"assistant","model":"client-model","content":[{"type":"thinking","thinking":"think hard","signature":""},{"type":"text","text":"answer"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":6,"output_tokens":3,"cache_read_input_tokens":4}}`,
		},
		{
			name: "reasoning field and part-list content",
			in: `{"id":"r","choices":[{"message":{"reasoning":"a ","content":[{"type":"thinking","thinking":[{"type":"text","text":"b"}]},{"type":"text","text":"out"}]},"finish_reason":"content_filter"}],
				"usage":{"prompt_tokens":3,"completion_tokens":1,"cached_tokens":1}}`,
			want: `{"id":"msg_r","type":"message","role":"assistant","model":"client-model","content":[{"type":"thinking","thinking":"a b","signature":""},{"type":"text","text":"out"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":1}}`,
		},
		{
			name: "tool_calls finish without any usable call is end_turn",
			in:   `{"id":"r","choices":[{"message":{"content":"hi"},"finish_reason":"tool_calls"}]}`,
			want: `{"id":"msg_r","type":"message","role":"assistant","model":"client-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := chatToAnthropic([]byte(c.in), "client-model")
			if err != nil {
				t.Fatalf("chatToAnthropic: %v", err)
			}
			jsonEqual(t, out, c.want)
		})
	}

	t.Run("missing ids are generated", func(t *testing.T) {
		out, err := chatToAnthropic([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}}]}`), "m")
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			ID      string `json:"id"`
			Content []struct {
				ID string `json:"id"`
			} `json:"content"`
		}
		_ = json.Unmarshal(out, &r)
		if !strings.HasPrefix(r.ID, "msg_") || len(r.ID) < 10 || !strings.HasPrefix(r.Content[0].ID, "toolu_") {
			t.Errorf("ids = %q / %q", r.ID, r.Content[0].ID)
		}
	})
	t.Run("invalid JSON is an error", func(t *testing.T) {
		if _, err := chatToAnthropic([]byte(`<html>`), "m"); err == nil {
			t.Error("want error")
		}
	})
}

func TestTranslateChatErrorToAnthropic(t *testing.T) {
	long := strings.Repeat("x", 3000)
	cases := []struct {
		status          int
		body, typ, want string
	}{
		{401, `{"error":{"message":"Incorrect API key","type":"invalid_request_error","code":"invalid_api_key"}}`, "authentication_error", "Incorrect API key"},
		{429, `{"error":{"message":"slow down","code":"rate_limit_exceeded"}}`, "rate_limit_error", "slow down"},
		{400, `{"error":{"message":"Unsupported parameter: 'max_tokens'"}}`, "invalid_request_error", "Unsupported parameter: 'max_tokens'"},
		{400, `{"error":{"message":"This model's maximum context length is 128000 tokens.","code":"context_length_exceeded"}}`, "invalid_request_error", "prompt is too long: This model's maximum context length is 128000 tokens."},
		{422, `{"error":"bad param"}`, "invalid_request_error", "bad param"},
		{409, `{"message":"conflict"}`, "invalid_request_error", "conflict"},
		{402, `{"error":{"code":402,"message":"insufficient credits"}}`, "billing_error", "insufficient credits"},
		{403, `{}`, "permission_error", "{}"},
		{404, ``, "not_found_error", "Not Found"},
		{500, ` upstream exploded `, "api_error", "upstream exploded"},
		{503, `{"error":{"message":"busy"}}`, "overloaded_error", "busy"},
		{504, `{"error":{"message":"timeout"}}`, "timeout_error", "timeout"},
		{529, `{"error":{"message":"overloaded"}}`, "overloaded_error", "overloaded"},
		{502, long, "api_error", long[:1024]},
	}
	for _, c := range cases {
		want, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": c.typ, "message": c.want}})
		jsonEqual(t, chatErrorToAnthropic(c.status, []byte(c.body)), string(want))
	}
}

// --- streaming ---

type recorder struct{ writes [][]byte }

func (r *recorder) Write(p []byte) (int, error) {
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

var reFrame = regexp.MustCompile(`^event: ([a-z_]+)\ndata: (.*)\n\n$`)

// summarize checks every Write is exactly one well-formed event and renders the
// sequence compactly for golden comparison.
func summarize(t *testing.T, writes [][]byte) []string {
	t.Helper()
	var out []string
	for _, w := range writes {
		m := reFrame.FindSubmatch(w)
		if m == nil {
			t.Fatalf("write is not one SSE event: %q", w)
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type, ID, Name string
			} `json:"content_block"`
			Delta struct {
				Type        string  `json:"type"`
				Text        string  `json:"text"`
				Thinking    string  `json:"thinking"`
				PartialJSON string  `json:"partial_json"`
				StopReason  *string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens          int64 `json:"input_tokens"`
				OutputTokens         int64 `json:"output_tokens"`
				CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
			Error struct {
				Type, Message string
			} `json:"error"`
		}
		if err := json.Unmarshal(m[2], &ev); err != nil {
			t.Fatalf("event data %s: %v", m[2], err)
		}
		if ev.Type != string(m[1]) {
			t.Fatalf("event %s carries type %s", m[1], ev.Type)
		}
		var s string
		switch ev.Type {
		case "content_block_start":
			s = fmt.Sprintf("start %d %s", ev.Index, ev.ContentBlock.Type)
			if ev.ContentBlock.Type == "tool_use" {
				s += " " + ev.ContentBlock.Name
				if !strings.HasPrefix(ev.ContentBlock.ID, "toolu_") {
					s += " " + ev.ContentBlock.ID
				}
			}
		case "content_block_delta":
			s = fmt.Sprintf("delta %d %s%s%s", ev.Index, ev.Delta.Text, ev.Delta.Thinking, ev.Delta.PartialJSON)
			if ev.Delta.Type == "thinking_delta" {
				s = "think" + s
			}
		case "content_block_stop":
			s = fmt.Sprintf("stop %d", ev.Index)
		case "message_delta":
			s = fmt.Sprintf("message_delta %s in=%d out=%d cache=%d", *ev.Delta.StopReason, ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CacheReadInputTokens)
		case "error":
			s = "error " + ev.Error.Type + ": " + ev.Error.Message
		default:
			s = ev.Type
		}
		out = append(out, s)
	}
	return out
}

func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

func TestTranslateStreamChatToAnthropic(t *testing.T) {
	head := []string{"message_start", "ping"}
	longArgs := `{"content":"` + strings.Repeat("a", 100<<10) + `"}`
	cases := []struct {
		name, in string
		want     []string
		err      string // returned error, "" for a clean end
	}{
		{
			name: "text only with usage-only chunk",
			in: sse(`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`,
				`{"choices":[{"delta":{"content":"Hel"}}],"usage":null}`,
				`{"choices":[{"delta":{"content":"lo "}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":40}}}`,
				`[DONE]`),
			want: []string{"start 0 text", "delta 0 Hel", "delta 0 lo ", "stop 0", "message_delta end_turn in=60 out=5 cache=40", "message_stop"},
		},
		{
			name: "thinking then text",
			in: sse(`{"choices":[{"delta":{"reasoning_content":"let me "}}]}`,
				`{"choices":[{"delta":{"reasoning":"think"}}]}`,
				`{"choices":[{"delta":{"content":"done"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`),
			want: []string{"start 0 thinking", "thinkdelta 0 let me ", "thinkdelta 0 think", "stop 0", "start 1 text", "delta 1 done", "stop 1", "message_delta end_turn in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "text then two parallel tool calls with chunked args",
			in: sse(`{"choices":[{"delta":{"content":"On it."}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Read","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/a\"}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"Grep","arguments":"{\"q\""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":":1}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":9}}`, `[DONE]`),
			// Tool calls are buffered per call and emitted whole at the end.
			want: []string{"start 0 text", "delta 0 On it.", "stop 0",
				"start 1 tool_use Read call_a", `delta 1 {"p":"/a"}`, "stop 1",
				"start 2 tool_use Grep call_b", `delta 2 {"q":1}`, "stop 2",
				"message_delta tool_use in=20 out=9 cache=0", "message_stop"},
		},
		{
			name: "usage on the finish chunk ends the stream; trailing input ignored",
			in: sse(`{"choices":[{"delta":{"content":"x"}}]}`,
				`{"choices":[{"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_cache_hit_tokens":8}}`,
				`{"choices":[{"delta":{"content":"ignored"}}]}`),
			want: []string{"start 0 text", "delta 0 x", "stop 0", "message_delta end_turn in=2 out=1 cache=8", "message_stop"},
		},
		{
			name: "EOF without [DONE] still completes the message",
			in:   sse(`{"choices":[{"delta":{"content":"partial"}}]}`) + `data: {"choices":[{"delta":{"content":"!"},"finish_reason":"length"}]}`,
			want: []string{"start 0 text", "delta 0 partial", "delta 0 !", "stop 0", "message_delta max_tokens in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "empty stream is an error",
			in:   ``,
			want: []string{"error api_error: upstream stream ended early"},
			err:  "unexpected EOF",
		},
		{
			name: "EOF before a finish_reason fails instead of finishing a half tool call",
			in:   sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Bash","arguments":"{\"comm"}}]}}]}`),
			want: []string{"error api_error: upstream stream ended early"},
			err:  "unexpected EOF",
		},
		{
			name: "error frame mid-stream ends with an error event",
			in: sse(`{"choices":[{"delta":{"content":"a"}}]}`,
				`{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`,
				`{"choices":[{"delta":{"content":"never"}}]}`),
			want: []string{"start 0 text", "delta 0 a", "error rate_limit_error: Rate limit reached"},
			err:  "upstream stream error: Rate limit reached",
		},
		{
			name: "numeric error code maps by status",
			in:   sse(`{"error":{"code":502,"message":"provider down"},"choices":[{"delta":{},"finish_reason":"error"}]}`),
			want: []string{"error api_error: provider down"},
			err:  "upstream stream error: provider down",
		},
		{
			name: "CRLF framing, comments, event lines and bad JSON are tolerated",
			in:   ": keep-alive\r\n\r\nevent: chunk\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\r\n\r\ndata: {broken\r\n\r\ndata: [DONE]\r\n\r\n",
			want: []string{"start 0 text", "delta 0 ok", "stop 0", "message_delta end_turn in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "args before the name are held; stop with a tool call is tool_use",
			in: sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_z","function":{"name":"F","arguments":"1}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`),
			want: []string{"start 0 tool_use F call_z", `delta 0 {"a":1}`, "stop 0", "message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "nameless call is discarded; a fragment after other output rejoins its call",
			in: sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"t"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"G","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"u"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"x"}}]}}]}`, `[DONE]`),
			want: []string{"start 0 text", "delta 0 t", "delta 0 u", "stop 0", "start 1 tool_use G c1", `delta 1 {"_gateway_invalid_arguments":"{}x"}`, "stop 1",
				"message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "calls without index key by position; object args",
			in:   sse(`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"A","arguments":{"x":1}}},{"id":"b","function":{"name":"B","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`),
			want: []string{"start 0 tool_use A a", `delta 0 {"x":1}`, "stop 0", "start 1 tool_use B b", "delta 1 {}", "stop 1", "message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "calls without index in separate chunks stay separate",
			in: sse(`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"A","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"B","arguments":"{\"q\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"2}"}}]}}]}`, `[DONE]`),
			want: []string{"start 0 tool_use A a", "delta 0 {}", "stop 0", "start 1 tool_use B b", `delta 1 {"q":2}`, "stop 1", "message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "index 0 reused with a new id is a new call",
			in: sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_b","function":{"name":"Grep","arguments":"{\"q\":2}"}}]}}]}`, `[DONE]`),
			want: []string{"start 0 tool_use Read call_a", `delta 0 {"p":1}`, "stop 0", "start 1 tool_use Grep call_b", `delta 1 {"q":2}`, "stop 1", "message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "tool args longer than 64 KiB on one line",
			in:   sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"w","function":{"name":"Write","arguments":`+mustJSON(longArgs)+`}}]}}]}`, `[DONE]`),
			want: []string{"start 0 tool_use Write w", "delta 0 " + longArgs, "stop 0", "message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "parallel calls interleaved by index keep every fragment",
			in: sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"A","arguments":"{\"x\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"B","arguments":"{\"y\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `[DONE]`),
			want: []string{"start 0 tool_use A a", `delta 0 {"x":1}`, "stop 0", "start 1 tool_use B b", `delta 1 {"y":2}`, "stop 1", "message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "invalid streamed arguments are preserved, not replaced by {}",
			in:   sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"A","arguments":"{oops"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`),
			want: []string{"start 0 tool_use A a", `delta 0 {"_gateway_invalid_arguments":"{oops"}`, "stop 0", "message_delta tool_use in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "a stop string the vendor names is a stop_sequence",
			in:   sse(`{"choices":[{"delta":{"content":"x"},"finish_reason":"stop","stop_reason":"END"}]}`, `[DONE]`),
			want: []string{"start 0 text", "delta 0 x", "stop 0", "message_delta stop_sequence in=0 out=0 cache=0", "message_stop"},
		},
		{
			name: "part-list content streams thinking then text",
			in:   sse(`{"choices":[{"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"hm"}]},{"type":"text","text":"yes"}]}}]}`, `[DONE]`),
			want: []string{"start 0 thinking", "thinkdelta 0 hm", "stop 0", "start 1 text", "delta 1 yes", "stop 1", "message_delta end_turn in=0 out=0 cache=0", "message_stop"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var rec recorder
			err := streamChatToAnthropic(&rec, strings.NewReader(c.in), "client-model")
			if (err == nil) != (c.err == "") || (err != nil && err.Error() != c.err) {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
			got := summarize(t, rec.writes)
			if want := append(append([]string(nil), head...), c.want...); !reflect.DeepEqual(got, want) {
				t.Errorf("events\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestTranslateStreamMessageStartAndUsageParse(t *testing.T) {
	var rec recorder
	in := sse(`{"id":"c","choices":[{"delta":{"content":"hi"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":40}}}`, `[DONE]`)
	if err := streamChatToAnthropic(&rec, strings.NewReader(in), "gpt-5"); err != nil {
		t.Fatal(err)
	}
	var start struct {
		Message struct {
			ID, Model string
		}
	}
	_ = json.Unmarshal(reFrame.FindSubmatch(rec.writes[0])[2], &start)
	if !strings.HasPrefix(start.Message.ID, "msg_") || start.Message.Model != "gpt-5" {
		t.Errorf("message_start = %+v", start.Message)
	}
}

func TestTranslateStreamReadError(t *testing.T) {
	boom := errors.New("connection reset")
	src := io.MultiReader(strings.NewReader(sse(`{"choices":[{"delta":{"content":"a"}}]}`)), iotest.ErrReader(boom))
	var rec recorder
	if err := streamChatToAnthropic(&rec, src, "m"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	got := summarize(t, rec.writes)
	// The cause goes to capture (the returned error), not to the client.
	want := []string{"message_start", "ping", "start 0 text", "delta 0 a", "error api_error: upstream stream interrupted"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events\n got: %q\nwant: %q", got, want)
	}
}

// failWriter models a client that went away: the first write fails.
type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, io.ErrClosedPipe
}

func TestTranslateStreamStopsOnWriteError(t *testing.T) {
	// An endless upstream must not keep the translator reading once dst is gone.
	src := strings.NewReader(strings.Repeat(sse(`{"choices":[{"delta":{"content":"a"}}]}`), 1000))
	var fw failWriter
	if err := streamChatToAnthropic(&fw, src, "m"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("err = %v", err)
	}
	if fw.n != 1 {
		t.Errorf("writes after failure: %d", fw.n)
	}
}

// An upstream that sends nothing (a reasoning model thinking) still gets
// periodic pings to the client, so its idle watchdog does not abort.
func TestTranslateStreamPingsWhileSilent(t *testing.T) {
	defer func(d time.Duration) { PingEvery = d }(PingEvery)
	PingEvery = 5 * time.Millisecond
	pr, pw := io.Pipe()
	go func() {
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(pw, sse(`{"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}`, `[DONE]`))
		pw.Close()
	}()
	var rec lockedRecorder
	if err := streamChatToAnthropic(&rec, pr, "m"); err != nil {
		t.Fatal(err)
	}
	got := summarize(t, rec.writes)
	pings := 0
	for _, e := range got[2:] {
		if e == "ping" {
			pings++
		}
	}
	if pings == 0 || got[len(got)-1] != "message_stop" {
		t.Errorf("events %q: want pings while silent and a complete message", got)
	}
}

type lockedRecorder struct {
	mu sync.Mutex
	recorder
}

func (r *lockedRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorder.Write(p)
}

// A frame that never ends is cut off at the cap instead of growing the heap.
func TestTranslateStreamFrameCap(t *testing.T) {
	src := io.MultiReader(strings.NewReader("data: "), io.LimitReader(neverEnding('x'), llm.MaxFrameBytes+1<<20))
	var rec recorder
	if err := streamChatToAnthropic(&rec, src, "m"); !errors.Is(err, llm.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want %v", err, llm.ErrFrameTooLarge)
	}
	got := summarize(t, rec.writes)
	if got[len(got)-1] != "error api_error: upstream stream interrupted" {
		t.Errorf("events %q", got)
	}
}

type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}
