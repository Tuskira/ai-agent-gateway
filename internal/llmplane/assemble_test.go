package llmplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/anthropic"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// The registered Anthropic reader is what assembles; cmd/gateway imports it
// for the same reason.
var _ = anthropic.Name

// assembleEvents are the events of a short streamed Anthropic reply with one
// text block and one tool call, as an upstream sends them.
var assembleEvents = []string{
	`{"type":"message_start","message":{"id":"msg_test_proxy","type":"message","role":"assistant","model":"claude-test-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":21,"output_tokens":1,"service_tier":"standard"}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"ping"}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Listing "}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"files."}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_test_proxy","name":"Bash","input":{},"caller":{"type":"direct"}}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"ls"}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" -la\"}"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":17}}`,
	`{"type":"message_stop"}`,
}

const assembledProxyMessage = `{"id":"msg_test_proxy","type":"message","role":"assistant","model":"claude-test-1","content":[` +
	`{"type":"text","text":"Listing files."},` +
	`{"type":"tool_use","id":"toolu_test_proxy","name":"Bash","input":{"command":"ls -la"},"caller":{"type":"direct"}}` +
	`],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":21,"output_tokens":17,"service_tier":"standard"}}`

func sseStream(events []string) string {
	var b strings.Builder
	for _, e := range events {
		var t struct{ Type string }
		_ = json.Unmarshal([]byte(e), &t)
		b.WriteString("event: " + t.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

// A streamed call is stored as the message it describes while the client
// still gets the upstream's bytes untouched; the switch off, a stream that
// cannot be assembled, and a capture cut by the cap all store the raw stream
// (the cut one flagged truncated, the others not).
func TestRouter_StreamedResponseStoredAssembled(t *testing.T) {
	full := sseStream(assembleEvents)
	cut := sseStream(assembleEvents[:len(assembleEvents)-1]) // no message_stop
	failed := sseStream(append(append([]string{}, assembleEvents[:4]...), `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	cases := []struct {
		name      string
		upstream  string
		assemble  bool
		capBytes  int
		reqBody   string
		want      string // the stored response body
		truncated bool
	}{
		{"assembled", full, true, 1 << 20, `{"model":"claude-test-1","stream":true}`, assembledProxyMessage, false},
		{"switch off", full, false, 1 << 20, `{"model":"claude-test-1","stream":true}`, full, false},
		{"capture cut by the cap", full, true, 200, `{"model":"claude-test-1","stream":true}`, full[:200], true},
		{"stream cut short", cut, true, 1 << 20, `{"model":"claude-test-1","stream":true}`, cut, false},
		{"error event", failed, true, 1 << 20, `{"model":"claude-test-1","stream":true}`, failed, false},
		{"not a streamed call", `{"id":"msg_test_plain","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`, true, 1 << 20, `{"model":"claude-test-1"}`,
			`{"id":"msg_test_plain","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if strings.HasPrefix(c.upstream, "event:") {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				for _, ev := range strings.SplitAfter(c.upstream, "\n\n") {
					_, _ = io.WriteString(w, ev)
					w.(http.Flusher).Flush()
				}
			}))
			defer up.Close()
			rec := &lastRec{}
			gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: true, AssembleStreams: c.assemble,
				MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: c.capBytes}, rec)
			resp, got := do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, c.reqBody)
			if resp.StatusCode != http.StatusOK || got != c.upstream {
				t.Errorf("client got status %d, body differs from the upstream's: %q", resp.StatusCode, got)
			}
			call := rec.wait(t)
			if string(call.ResponseBody) != c.want || call.Truncated != c.truncated {
				t.Errorf("stored (truncated=%v) %q\nwant (truncated=%v) %q", call.Truncated, call.ResponseBody, c.truncated, c.want)
			}
			// Usage comes from the relayed bytes either way.
			if strings.HasPrefix(c.upstream, "event:") && c.name == "assembled" && (call.InputTokens != 21 || call.OutputTokens != 17 || call.StopReason != "tool_use") {
				t.Errorf("usage %d/%d stop %q", call.InputTokens, call.OutputTokens, call.StopReason)
			}
		})
	}
}

// An error status from a streaming request is a JSON body: stored as it is.
func TestRouter_StreamedCallErrorBodyStaysRaw(t *testing.T) {
	const errBody = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, errBody)
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: true, AssembleStreams: true, MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: 1 << 20}, rec)
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-test-1","stream":true}`)
	if c := rec.wait(t); string(c.ResponseBody) != errBody {
		t.Errorf("stored %q", c.ResponseBody)
	}
}

// A stream other readers speak (OpenAI here) has no assembler yet and is
// stored raw.
func TestRouter_StreamOfOtherDialectStaysRaw(t *testing.T) {
	const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{OpenAIEnabled: true, OpenAIBaseURL: up.URL, StoreBodies: true, AssembleStreams: true,
		MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: 1 << 20}, rec)
	do(t, "POST", gw.URL+"/openai/v1/chat/completions", nil, `{"model":"gpt-4o","stream":true}`)
	if c := rec.wait(t); string(c.ResponseBody) != stream {
		t.Errorf("stored %q", c.ResponseBody)
	}
}

// bedrockStream is events as Bedrock's InvokeModelWithResponseStream sends
// them: binary event-stream frames whose payload carries each event base64.
func bedrockStream(events []string) []byte {
	var out []byte
	for _, ev := range events {
		payload, _ := json.Marshal(map[string]string{"bytes": base64Std(ev)})
		out = append(out, encodeEventStreamFrame(map[string]string{":message-type": "event", ":event-type": "chunk", ":content-type": "application/json"}, payload)...)
	}
	return out
}

// An Anthropic model called natively on Bedrock's invoke route is stored
// assembled too: what the client gets (and the capture sees) is binary
// event-stream frames, which are decoded first. The client's bytes are the
// upstream's.
func TestRouter_NativeBedrockInvokeStreamStoredAssembled(t *testing.T) {
	frames := bedrockStream(assembleEvents)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(frames)
	}))
	defer up.Close()
	for _, assemble := range []bool{true, false} {
		rec := &lastRec{}
		fb := fakeBedrock{newBedrockProvider("us-east-1", "", ""), up.URL}
		rt := &router{byID: map[string]Provider{"bedrock": fb}, recorder: rec, client: &http.Client{}, pricing: pricing.Default,
			storeBodies: true, maxCaptureReq: 1 << 20, maxCaptureResp: 1 << 20, assembleStream: assemble}
		gw := httptest.NewServer(withTags(rt))
		resp, got := do(t, "POST", gw.URL+"/bedrock/model/anthropic.claude-haiku-4-5-20251001-v1:0/invoke-with-response-stream", nil, `{}`)
		gw.Close()
		if resp.StatusCode != http.StatusOK || got != string(frames) {
			t.Fatalf("assemble=%v: client got status %d, %d bytes, want the upstream's %d", assemble, resp.StatusCode, len(got), len(frames))
		}
		want := string(frames)
		if assemble {
			want = assembledProxyMessage
		}
		if c := rec.wait(t); string(c.ResponseBody) != want || c.Truncated {
			t.Errorf("assemble=%v: stored (truncated=%v) %q, want %q", assemble, c.Truncated, c.ResponseBody, want)
		}
	}
}

// An Anthropic client routed to Bedrock through the model registry gets SSE
// re-framed from event-stream frames; the capture sits after the re-framing
// and stores the assembled message.
func TestRegistry_AnthropicOnBedrockStreamStoredAssembled(t *testing.T) {
	f := newRegistryFixture(t)
	frames := bedrockStream(assembleEvents)
	bedrock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(frames)
	}))
	t.Cleanup(bedrock.Close)
	f.credential(t, "aws-bedrock", map[string]string{"access_key_id": "AKIAREGISTRY", "secret_access_key": "secret"})
	f.model(t, "sonnet-br", nil, store.ModelTarget{Vendor: "bedrock", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", Region: "us-west-2", Credential: "aws-bedrock"})
	to, _ := url.Parse(bedrock.URL)
	rt := &router{
		byID:     map[string]Provider{"anthropic": newAnthropicProvider("https://api.anthropic.com")},
		recorder: f.rec,
		client:   &http.Client{Transport: hostRewriteTransport{to: to}},
		pricing:  pricing.Default,
		registry: f.registry,
		bedrock:  newBedrockProvider("us-east-1", "", ""),

		storeBodies: true, maxCaptureReq: 1 << 20, maxCaptureResp: 1 << 20, assembleStream: true,
	}
	gw := httptest.NewServer(withPrincipal(withTags(rt), &pkgauth.Principal{TenantID: f.tenant, Roles: []string{"agent"}}))
	t.Cleanup(gw.Close)

	resp, got := do(t, "POST", gw.URL+"/v1/messages", nil, `{"model":"sonnet-br","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || got != sseStream(assembleEvents) {
		t.Fatalf("client got status %d, body %q", resp.StatusCode, got)
	}
	if c := f.rec.wait(t); string(c.ResponseBody) != assembledProxyMessage {
		t.Errorf("stored %q\nwant %q", c.ResponseBody, assembledProxyMessage)
	}
}
