package llmplane

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	// The Readers cmd/gateway blank-imports besides anthropic and
	// openaicompat (translated_test.go imports those).
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/bedrock"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/gemini"
)

// readerGoldenStream is the stream of a pkg/llm reader golden: its SSE text,
// or its binary stream_b64 decoded.
func readerGoldenStream(t *testing.T, reader, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "llm", "llmtest", "testdata", "readers", reader, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		SSE       *string `json:"sse"`
		StreamB64 *string `json:"stream_b64"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	switch {
	case g.SSE != nil:
		return []byte(*g.SSE)
	case g.StreamB64 != nil:
		b, err := base64.StdEncoding.DecodeString(*g.StreamB64)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatalf("%s/%s has no stream", reader, name)
	return nil
}

// Every generation route of every provider, the wire formats beyond the
// Messages and Chat Completions ones included, reaches the agent with its
// canonical conversation and answer. The upstream answers each route with
// a fixed body in that route's own format.
func TestDetectionTee_EveryReader(t *testing.T) {
	type route struct {
		name, path, auth, req string
		respType, resp        string
		wantDialect           string
		wantConversation      string
		wantAnswer            string
	}
	sigv4 := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=00"
	routes := []route{
		{
			name: "gemini", path: "/gemini/v1beta/models/g:generateContent",
			req:      `{"systemInstruction":{"parts":[{"text":"be brief"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			respType: "application/json", resp: `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
			wantDialect:      readerGemini,
			wantConversation: `{"cv":1,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"history":"full"}`,
			wantAnswer:       `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		},
		{
			name: "gemini stream as a JSON array", path: "/gemini/v1beta/models/g:streamGenerateContent",
			req:      `{"contents":[{"role":"user","parts":[{"text":"time?"}]}]}`,
			respType: "application/json", resp: string(readerGoldenStream(t, "gemini", "stream_json_array")),
			wantDialect:      readerGemini,
			wantConversation: `{"cv":1,"messages":[{"role":"user","content":[{"type":"text","text":"time?"}]}],"history":"full"}`,
			wantAnswer: `{"content":[{"type":"text","text":"Hello world"},{"type":"tool_use","id":"gemini-call-0-get_time","name":"get_time","input":{"tz":"UTC"}},` +
				`{"type":"tool_use","id":"gemini-call-1-get_time","name":"get_time","input":{"tz":"CET"}}],"stop_reason":"tool_use"}`,
		},
		{
			name: "bedrock converse", path: "/model/amazon.nova-pro-v1:0/converse", auth: sigv4,
			req:      `{"system":[{"text":"be brief"}],"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
			respType: "application/json", resp: `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`,
			wantDialect:      readerBedrockConverse,
			wantConversation: `{"cv":1,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"history":"full"}`,
			wantAnswer:       `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		},
		{
			name: "bedrock converse stream", path: "/model/amazon.nova-pro-v1:0/converse-stream", auth: sigv4,
			req:      `{"messages":[{"role":"user","content":[{"text":"Hello there"}]}]}`,
			respType: "application/vnd.amazon.eventstream", resp: string(readerGoldenStream(t, "bedrock_converse", "text")),
			wantDialect:      readerBedrockConverse,
			wantConversation: `{"cv":1,"messages":[{"role":"user","content":[{"type":"text","text":"Hello there"}]}],"history":"full"}`,
			wantAnswer:       `{"content":[{"type":"text","text":"Hi! How can I help?"}],"stop_reason":"end_turn"}`,
		},
		{
			name: "bedrock invoke, not Anthropic", path: "/model/meta.llama3-70b-instruct-v1:0/invoke", auth: sigv4,
			req:      `{"prompt":"What is the capital of France?","max_gen_len":64}`,
			respType: "application/json", resp: `{"generation":"Paris.","prompt_token_count":9,"generation_token_count":2,"stop_reason":"stop"}`,
			wantDialect:      readerBedrockInvoke,
			wantConversation: `{"cv":1,"messages":[{"role":"user","content":[{"type":"text","text":"What is the capital of France?"}]}],"history":"prompt"}`,
			wantAnswer:       `{"content":[{"type":"text","text":"Paris."}],"stop_reason":"end_turn"}`,
		},
		{
			name: "openai responses", path: "/openai/v1/responses", auth: "Bearer sk-x",
			req:      `{"model":"m","instructions":"be brief","input":"hi"}`,
			respType: "application/json", resp: `{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}`,
			wantDialect:      readerOpenAIResponses,
			wantConversation: `{"cv":1,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"history":"full"}`,
			wantAnswer:       `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		},
		{
			name: "openai responses, history on the vendor", path: "/openai/v1/responses", auth: "Bearer sk-x",
			req:      `{"model":"m","previous_response_id":"resp_0","input":[{"type":"function_call_output","call_id":"c1","output":"42"}]}`,
			respType: "application/json", resp: `{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}`,
			wantDialect:      readerOpenAIResponses,
			wantConversation: `{"cv":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"42"}]}]}],"history":"server_side"}`,
			wantAnswer:       `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		},
		{
			name: "openai legacy completions", path: "/openai/v1/completions", auth: "Bearer sk-x",
			req:      `{"model":"m","prompt":"Say this is a test","max_tokens":7}`,
			respType: "application/json", resp: `{"id":"cmpl-1","object":"text_completion","created":1700000000,"model":"m","choices":[{"text":"This is a test","index":0,"logprobs":null,"finish_reason":"stop"}]}`,
			wantDialect:      readerOpenAICompletions,
			wantConversation: `{"cv":1,"messages":[{"role":"user","content":[{"type":"text","text":"Say this is a test"}]}],"history":"prompt"}`,
			wantAnswer:       `{"content":[{"type":"text","text":"This is a test"}],"stop_reason":"end_turn"}`,
		},
		{
			name: "anthropic legacy complete", path: "/v1/complete",
			req:      `{"model":"m","prompt":"\n\nHuman: Hello there\n\nAssistant:","max_tokens_to_sample":9}`,
			respType: "application/json", resp: `{"type":"completion","id":"compl_1","completion":" Hi!","stop_reason":"stop_sequence","model":"m"}`,
			wantDialect:      readerAnthropicComplete,
			wantConversation: `{"cv":1,"messages":[{"role":"user","content":[{"type":"text","text":"Hello there"}]}],"history":"prompt"}`,
			wantAnswer:       `{"content":[{"type":"text","text":" Hi!"}],"stop_reason":"end_turn"}`,
		},
	}
	byPath := map[string]route{}
	for _, r := range routes {
		byPath[strings.TrimPrefix(strings.TrimPrefix(r.path, "/gemini"), "/openai")] = r
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c, ok := byPath[r.URL.Path]
		if !ok {
			http.Error(w, "no route "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", c.respType)
		_, _ = w.Write([]byte(c.resp))
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	transportOverride = redirectTransport{target: u} // Bedrock signs for its AWS host
	defer func() { transportOverride = nil }()
	agent := newAgentStub(t, nil)
	gw := gateway(t, Config{
		UpstreamBaseURL: up.URL, OpenAIEnabled: true, OpenAIBaseURL: up.URL, GeminiEnabled: true, GeminiBaseURL: up.URL,
		BedrockEnabled: true, BedrockRegion: "us-east-1", BedrockCredentialMode: "passthrough",
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, MaxInFlight: 1}), // one worker: posted in order
	}, &lastRec{})
	for _, r := range routes {
		hdr := map[string]string{"x-api-key": "sk", "Content-Type": "application/json"}
		if r.auth != "" {
			hdr["Authorization"] = r.auth
		}
		if resp, b := do(t, http.MethodPost, gw.URL+r.path, hdr, r.req); resp.StatusCode != http.StatusOK || b != r.resp {
			t.Fatalf("%s: status %d body %q", r.name, resp.StatusCode, b)
		}
	}
	turns, _ := agent.wait(t, len(routes))
	for i, r := range routes {
		got := turns[i]
		if got.Path != r.path || got.Dialect != r.wantDialect || got.Op != routeGenerate || got.NormalizeError != "" {
			t.Errorf("%s: turn path %q dialect %q op %q error %q", r.name, got.Path, got.Dialect, got.Op, got.NormalizeError)
			continue
		}
		sameJSONValue(t, r.name+" conversation", marshalJSON(t, got.Conversation), []byte(r.wantConversation))
		sameJSONValue(t, r.name+" answer", marshalJSON(t, got.Answer), []byte(r.wantAnswer))
	}
}

// A request mark of history is honoured only from a Reader that sets it
// and refuses a body that forges it.
func TestHistoryOnlyFromMarkingReaders(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"_gateway_history":"prompt"}`)
	if got := normalized(t, readerOpenAIChat, body, nil, "", false); got.Conversation == nil || got.Conversation.History != "full" {
		t.Errorf("openai_chat with a forged mark: %+v (error %q)", got.Conversation, got.NormalizeError)
	}
	got := normalized(t, readerOpenAICompletions, []byte(`{"model":"m","prompt":"x","_gateway_history":"server_side"}`), nil, "", false)
	if got.Conversation != nil || !strings.Contains(got.NormalizeError, "_gateway_history") {
		t.Errorf("openai_completions with a forged mark: %+v (error %q)", got.Conversation, got.NormalizeError)
	}
}

// A text document keeps its text (an instruction can hide in an attached
// file); an image or a binary document keeps only its type and size.
func TestDocumentText(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain notes"}},` +
		`{"type":"document","source":{"type":"base64","media_type":"text/csv","data":"` + base64.StdEncoding.EncodeToString([]byte("a,b")) + `"}},` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}},` +
		`{"type":"document","source":{"type":"content","content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`)
	got := normalized(t, readerAnthropic, body, nil, "", false)
	if got.Conversation == nil {
		t.Fatal(got.NormalizeError)
	}
	sameJSONValue(t, "content", marshalJSON(t, got.Conversation.Messages[0].Content), []byte(`[`+
		`{"type":"document","text":"plain notes","media_type":"text/plain","bytes":11},`+
		`{"type":"document","text":"a,b","media_type":"text/csv","bytes":4},`+
		`{"type":"document","media_type":"application/pdf","bytes":8},`+
		`{"type":"document","text":"one\ntwo","bytes":59},`+
		`{"type":"image","media_type":"image/png","bytes":12}]`))
}
