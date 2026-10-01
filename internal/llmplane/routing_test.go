package llmplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func testProviders() ([]Provider, map[string]Provider) {
	ps := buildProviders(Config{
		BedrockEnabled: true,
		OpenAIEnabled:  true,
		GeminiEnabled:  true,
	}, nil)
	byID := make(map[string]Provider, len(ps))
	for _, p := range ps {
		byID[p.ID()] = p
	}
	return ps, byID
}

func TestPickProvider_Routing(t *testing.T) {
	_, byID := testProviders()
	cases := []struct {
		path         string
		wantProvider string // "" = 404
		wantUpstream string
	}{
		// prefix routing (explicit, unambiguous — required for openai/gemini)
		{"/openai/v1/chat/completions", "openai", "/v1/chat/completions"},
		{"/gemini/v1beta/models/gemini-2.5-flash:generateContent", "gemini", "/v1beta/models/gemini-2.5-flash:generateContent"},
		{"/anthropic/v1/messages", "anthropic", "/v1/messages"},
		{"/anthropic/v1/messages/count_tokens", "anthropic", "/v1/messages/count_tokens"},
		{"/bedrock/model/us.anthropic.claude/converse", "bedrock", "/model/us.anthropic.claude/converse"},
		{"/openai", "openai", "/"}, // prefix with no subpath still selects the provider
		// bare native paths for SDK clients that append to a base URL (upstream
		// path = full path), scoped to each provider's OWN endpoints.
		{"/v1/messages", "anthropic", "/v1/messages"},
		{"/v1/messages/count_tokens", "anthropic", "/v1/messages/count_tokens"},
		{"/model/us.anthropic.claude/converse", "bedrock", "/model/us.anthropic.claude/converse"},
		// bare /v1 is NOT a broad Anthropic catch-all: OpenAI's own /v1 path must
		// NOT be swept into Anthropic — it requires the /openai prefix → 404 bare.
		{"/v1/chat/completions", "", ""},
		{"/v1/embeddings", "", ""},
		// unknown → 404
		{"/nope", "", ""},
		{"/", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, c.path, nil)
		p, up := pickProvider(byID, r)
		if c.wantProvider == "" {
			if p != nil {
				t.Errorf("%s: got provider %q, want 404", c.path, p.Name())
			}
			continue
		}
		if p == nil {
			t.Errorf("%s: got 404, want %s", c.path, c.wantProvider)
			continue
		}
		if p.Name() != c.wantProvider || up != c.wantUpstream {
			t.Errorf("%s: got (%s, %q), want (%s, %q)", c.path, p.Name(), up, c.wantProvider, c.wantUpstream)
		}
	}
}

func TestProvider_ModelExtraction(t *testing.T) {
	_, byID := testProviders()
	// body-model providers
	if m := byID["openai"].Model([]byte(`{"model":"gpt-4o-mini"}`), "/v1/chat/completions"); m != "gpt-4o-mini" {
		t.Errorf("openai model = %q", m)
	}
	if m := byID["anthropic"].Model([]byte(`{"model":"claude-haiku-4-5"}`), "/v1/messages"); m != "claude-haiku-4-5" {
		t.Errorf("anthropic model = %q", m)
	}
	// path-model providers
	if m := byID["bedrock"].Model(nil, "/model/us.anthropic.claude-haiku-4-5-v1:0/converse"); m != "us.anthropic.claude-haiku-4-5-v1:0" {
		t.Errorf("bedrock model = %q", m)
	}
	if m := byID["gemini"].Model(nil, "/v1beta/models/gemini-2.5-flash:generateContent"); m != "gemini-2.5-flash" {
		t.Errorf("gemini model = %q", m)
	}
}

func TestParseUsage_OpenAIAndGemini(t *testing.T) {
	_, byID := testProviders()
	oa := byID["openai"].ParseUsage([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":6}}}`))
	if oa.InputTokens != 11 || oa.OutputTokens != 7 || oa.CacheReadTokens != 6 || oa.StopReason != "stop" {
		t.Errorf("openai usage = %+v", oa)
	}
	gm := byID["gemini"].ParseUsage([]byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"cachedContentTokenCount":3}}`))
	if gm.InputTokens != 12 || gm.OutputTokens != 8 || gm.CacheReadTokens != 3 || gm.StopReason != "STOP" {
		t.Errorf("gemini usage = %+v", gm)
	}
	// Thinking model: thoughtsTokenCount is billed as output but reported
	// separately from candidatesTokenCount, so output = candidates + thoughts.
	gt := byID["gemini"].ParseUsage([]byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":13,"candidatesTokenCount":8,"thoughtsTokenCount":62,"totalTokenCount":83}}`))
	if gt.InputTokens != 13 || gt.OutputTokens != 70 {
		t.Errorf("gemini thinking usage = %+v (want in=13 out=70)", gt)
	}
}

// OpenAI /v1/responses uses input_tokens/output_tokens (not prompt/completion),
// non-stream and in the streamed response.completed event. Both must be captured.
func TestParseUsage_OpenAIResponses(t *testing.T) {
	_, byID := testProviders()
	obj := byID["openai"].ParseUsage([]byte(`{"usage":{"input_tokens":20,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}}}`))
	if obj.InputTokens != 20 || obj.OutputTokens != 5 || obj.CacheReadTokens != 4 {
		t.Errorf("responses non-stream usage = %+v", obj)
	}
	sse := byID["openai"].ParseUsage([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":5}}}\n"))
	if sse.InputTokens != 20 || sse.OutputTokens != 5 {
		t.Errorf("responses stream usage = %+v", sse)
	}
}

// :streamGenerateContent without alt=sse returns a JSON array; the last chunk's
// cumulative usageMetadata wins.
func TestParseUsage_GeminiArrayMode(t *testing.T) {
	_, byID := testProviders()
	u := byID["gemini"].ParseUsage([]byte(`[{"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":1}},{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"cachedContentTokenCount":3}}]`))
	if u.InputTokens != 12 || u.OutputTokens != 8 || u.CacheReadTokens != 3 || u.StopReason != "STOP" {
		t.Errorf("gemini array usage = %+v", u)
	}
}

// Anthropic server-tool-use grows the cumulative input+cache in the final
// message_delta; the delta value must win over message_start's initial value.
func TestParseUsage_AnthropicStreamCumulativeInput(t *testing.T) {
	_, byID := testProviders()
	sse := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":2679,\"output_tokens\":1}}}\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":10682,\"output_tokens\":50},\"delta\":{\"stop_reason\":\"end_turn\"}}\n"
	u := byID["anthropic"].ParseUsage([]byte(sse))
	if u.InputTokens != 10682 || u.OutputTokens != 50 || u.StopReason != "end_turn" || u.ProviderRequestID != "msg_1" {
		t.Errorf("anthropic stream usage = %+v", u)
	}
}

// Signed Bedrock modes must forward native X-Amzn-Bedrock-* feature headers, but
// nothing else (a blanket copy would poison the SigV4 canonical request).
func TestCopyBedrockNativeHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("X-Amzn-Bedrock-GuardrailIdentifier", "gr-1")
	src.Set("X-Amzn-Bedrock-Trace", "ENABLED")
	src.Set("User-Agent", "foo")
	src.Set("Authorization", "AWS4-HMAC-SHA256 x")
	dst := http.Header{}
	copyBedrockNativeHeaders(dst, src)
	if dst.Get("X-Amzn-Bedrock-GuardrailIdentifier") != "gr-1" || dst.Get("X-Amzn-Bedrock-Trace") != "ENABLED" {
		t.Errorf("native bedrock headers not forwarded: %v", dst)
	}
	if dst.Get("User-Agent") != "" || dst.Get("Authorization") != "" {
		t.Errorf("non-native header leaked into signed request: %v", dst)
	}
}
