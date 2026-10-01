package llmplane

import "testing"

// A response larger than the 64 KiB usage window arrives at ParseUsage as a
// truncated tail fragment: the whole-body Unmarshal fails but the trailing usage
// object is intact and must still be recovered (else tokens/cost = 0).
func TestParseUsage_LargeBodyTailFallback(t *testing.T) {
	_, byID := testProviders()

	// OpenAI-shaped fragment (leading junk makes whole-body Unmarshal fail).
	oaFrag := []byte(`...truncated prefix junk... "usage":{"prompt_tokens":1200,"completion_tokens":800,"prompt_tokens_details":{"cached_tokens":300}}}`)
	oa := byID["openai"].ParseUsage(oaFrag)
	if oa.InputTokens != 1200 || oa.OutputTokens != 800 || oa.CacheReadTokens != 300 {
		t.Errorf("openai tail fallback = %+v", oa)
	}

	// Anthropic-shaped fragment.
	anFrag := []byte(`junk...}] "usage":{"input_tokens":50,"output_tokens":9000,"cache_read_input_tokens":10}}`)
	an := byID["anthropic"].ParseUsage(anFrag)
	if an.InputTokens != 50 || an.OutputTokens != 9000 || an.CacheReadTokens != 10 {
		t.Errorf("anthropic tail fallback = %+v", an)
	}

	// Gemini-shaped array fragment (usageMetadata).
	gmFrag := []byte(`,{"candidates":[...]} "usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":12,"thoughtsTokenCount":30}}]`)
	gm := byID["gemini"].ParseUsage(gmFrag)
	if gm.InputTokens != 40 || gm.OutputTokens != 42 { // 12 candidates + 30 thoughts
		t.Errorf("gemini tail fallback = %+v", gm)
	}
}

// Bedrock InvokeModel native bodies for non-Claude/Nova models must yield tokens.
func TestParseUsage_BedrockInvokeShapes(t *testing.T) {
	_, byID := testProviders()
	br := byID["bedrock"]

	llama := br.ParseUsage([]byte(`{"generation":"hi","prompt_token_count":123,"generation_token_count":45}`))
	if llama.InputTokens != 123 || llama.OutputTokens != 45 {
		t.Errorf("llama = %+v", llama)
	}
	titan := br.ParseUsage([]byte(`{"inputTextTokenCount":77,"results":[{"totalOutputTextTokenCount":33}]}`))
	if titan.InputTokens != 77 || titan.OutputTokens != 33 {
		t.Errorf("titan = %+v", titan)
	}
	mistral := br.ParseUsage([]byte(`{"choices":[{"message":{}}],"usage":{"prompt_tokens":88,"completion_tokens":22}}`))
	if mistral.InputTokens != 88 || mistral.OutputTokens != 22 {
		t.Errorf("mistral = %+v", mistral)
	}
}

// Gemini tool/grounding tokens are a separate billed input bucket.
func TestParseUsage_GeminiToolTokens(t *testing.T) {
	_, byID := testProviders()
	u := byID["gemini"].ParseUsage([]byte(`{"usageMetadata":{"promptTokenCount":100,"toolUsePromptTokenCount":40,"candidatesTokenCount":10}}`))
	if u.InputTokens != 140 || u.OutputTokens != 10 {
		t.Errorf("gemini tool tokens = %+v (want in=140 out=10)", u)
	}
}

// Anthropic 1-hour cache split and web-search request count must be captured.
func TestParseUsage_Anthropic1hAndWebSearch(t *testing.T) {
	_, byID := testProviders()
	u := byID["anthropic"].ParseUsage([]byte(`{"id":"x","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_1h_input_tokens":400},"server_tool_use":{"web_search_requests":3}}}`))
	if u.CacheCreationTokens != 1000 || u.CacheCreation1hTokens != 400 || u.WebSearchRequests != 3 {
		t.Errorf("anthropic 1h/web = %+v", u)
	}
}
