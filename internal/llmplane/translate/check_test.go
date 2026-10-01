package translate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

func parse(t *testing.T, body string) *llm.Request {
	t.Helper()
	req, err := dialect.ParseRequest(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return req
}

func TestCheck(t *testing.T) {
	const user = `"messages":[{"role":"user","content":"a"}]`
	msg := func(content string) string { return `{"messages":[{"role":"user","content":[` + content + `]}]}` }
	cases := []struct {
		name, body, want string // want "" = carried (or on the drop list)
	}{
		{"plain text", `{` + user + `}`, ""},
		{"drop list: metadata, top_k, cache_control, context_management", `{` + user + `,"metadata":{"user_id":"u"},"top_k":5,"cache_control":{"type":"ephemeral"},"context_management":{"edits":[]}}`, ""},
		{"drop list: block and tool cache_control, tool hints", `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}],
			"tools":[{"name":"f","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"},"strict":true,"defer_loading":false,"eager_input_streaming":true}]}`, ""},
		{"earlier thinking, redacted thinking and citations are dropped", `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":[
			{"type":"thinking","thinking":"t","signature":"sig"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"b","citations":[]}]}]}`, ""},
		{"images: base64 and url", msg(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}},{"type":"image","source":{"type":"url","url":"https://e.x/a.png"}}`), ""},
		{"text document", msg(`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"d"}}`), ""},
		{"tool result with image and tool_reference", `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"x"},{"type":"tool_reference","tool_name":"g"}]}]}]}`, ""},
		{"thinking config and tool_choice none", `{` + user + `,"thinking":{"type":"enabled","budget_tokens":1024},"tools":[{"name":"f"}],"tool_choice":{"type":"none"}}`, ""},

		{"unknown top-level field", `{` + user + `,"container":"c"}`, "container"},
		{"structured output", `{` + user + `,"output_config":{"effort":"high","format":{"type":"json_schema"}}}`, "output_config.format"},
		{"server tool", `{` + user + `,"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, "tools[0] (web_search_20250305)"},
		{"unknown tool field", `{` + user + `,"tools":[{"name":"f","input_examples":[{}]}]}`, "tools[0].input_examples"},
		{"unknown tool_choice", `{` + user + `,"tools":[{"name":"f"}],"tool_choice":{"type":"mystery"}}`, "tool_choice.type=mystery"},
		{"pdf document", msg(`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA"}}`), "messages[0].content[0] (document source base64)"},
		{"document citations", msg(`{"type":"document","source":{"type":"text","data":"d"},"citations":{"enabled":true}}`), "messages[0].content[0].citations"},
		{"image by file id", msg(`{"type":"image","source":{"type":"file","file_id":"f"}}`), "messages[0].content[0] (image source file)"},
		{"non-image media in an image block", msg(`{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"AA"}}`), "messages[0].content[0] (image media_type application/pdf)"},
		{"unknown block", msg(`{"type":"search_result","content":[]}`), "messages[0].content[0] (search_result)"},
		{"server tool history", `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{}}]}]}`, "messages[1].content[0] (server_tool_use)"},
		{"document inside a tool result", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"document","source":{"type":"text","data":"d"}}]}]}]}`, "messages[0].content[0].content[0] (document)"},
		{"thinking in a user turn", msg(`{"type":"thinking","thinking":"t"}`), "messages[0].content[0] (thinking outside an assistant turn)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Check(parse(t, c.body), provider.Capabilities())
			var unsup *llm.ErrUnsupported
			switch {
			case c.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.want != "" && (!errors.As(err, &unsup) || unsup.Field != c.want):
				t.Errorf("err = %v, want unsupported_by_route: %s", err, c.want)
			}
		})
	}

	t.Run("passthrough carries everything", func(t *testing.T) {
		if err := Check(parse(t, msg(`{"type":"search_result","content":[]}`)), llm.Capabilities{Passthrough: true}); err != nil {
			t.Error(err)
		}
	})
	t.Run("capability flags gate their feature", func(t *testing.T) {
		body := `{` + user + `,"tools":[{"name":"f"}],"tool_choice":{"type":"none"},"stop_sequences":["x"]}`
		for caps, want := range map[llm.Capabilities]string{
			{StopSequences: true}:  "tool_choice.none",
			{ToolChoiceNone: true}: "stop_sequences",
		} {
			var unsup *llm.ErrUnsupported
			if err := Check(parse(t, body), caps); !errors.As(err, &unsup) || unsup.Field != want {
				t.Errorf("caps %+v: err = %v, want %s", caps, err, want)
			}
		}
	})
}

// A refused request never reaches BuildRequest, and the refusal is a 400 the
// router relays in the client's envelope.
func TestPrepareRefusesBeforeBuild(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"a"}],"mcp_servers":[]}`))
	_, err := Prepare(context.Background(), r, dialect, panicProvider{provider}, llm.Target{Model: "x"}, Options{})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadRequest || se.Message != "unsupported_by_route: mcp_servers" {
		t.Fatalf("err = %v, want 400 unsupported_by_route: mcp_servers", err)
	}
}

type panicProvider struct{ llm.Provider }

func (panicProvider) BuildRequest(context.Context, *llm.Request, llm.Target) (*http.Request, error) {
	panic("BuildRequest called for a refused request")
}

// Prompt usage a vendor reports on its first chunk is in message_start.
func TestStreamPromptUsageInMessageStart(t *testing.T) {
	var rec recorder
	in := sse(`{"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":0}}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1}}`)
	if err := streamChatToAnthropic(&rec, strings.NewReader(in), "m"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rec.writes[0], []byte(`"input_tokens":12`)) {
		t.Errorf("message_start = %s, want input_tokens 12", rec.writes[0])
	}
}

// Vendor errors carry the gateway request id; only allowlisted vendor
// headers survive; a redirect becomes 502.
func TestResponseErrorsAndHeaders(t *testing.T) {
	call := func(managed bool) *Call {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"a"}]}`))
		c, err := Prepare(context.Background(), r, dialect, provider, llm.Target{BaseURL: "https://x.test/v1", Model: "up"}, Options{RequestID: "req-1", ManagedKey: managed})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	respond := func(c *Call, status int, hdr http.Header, body string) (*http.Response, string) {
		resp := &http.Response{StatusCode: status, Header: hdr, Body: io.NopCloser(strings.NewReader(body))}
		c.Response(resp)
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, out := respond(call(false), 429, http.Header{"Retry-After": {"3"}, "Openai-Organization": {"org"}, "Location": {"https://e.x"}}, `{"error":{"message":"slow"}}`)
	if resp.StatusCode != 429 || !strings.Contains(out, `"request_id":"req-1"`) || !strings.Contains(out, `"rate_limit_error"`) {
		t.Errorf("status %d body %s", resp.StatusCode, out)
	}
	if resp.Header.Get("Retry-After") != "3" || resp.Header.Get("Openai-Organization") != "" || resp.Header.Get("Location") != "" {
		t.Errorf("headers = %v", resp.Header)
	}
	if resp, out := respond(call(false), 307, http.Header{"Location": {"https://e.x"}}, ``); resp.StatusCode != 502 || !strings.Contains(out, "upstream redirected") || !strings.Contains(out, "req-1") {
		t.Errorf("redirect: status %d body %s", resp.StatusCode, out)
	}
	if _, out := respond(call(true), 401, http.Header{}, `{"error":{"message":"bad key sk-...abcd"}}`); strings.Contains(out, "sk-") || !strings.Contains(out, "upstream credential rejected for model alias") {
		t.Errorf("managed-key 401 leaked the vendor message: %s", out)
	}
}
