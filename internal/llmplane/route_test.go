package llmplane

import (
	"net/http"
	"testing"
)

// Every endpoint each provider relays is described: generation routes with
// the reader of their wire format, and the rest as count or utility. A
// query string and a trailing slash do not change the answer; any method
// but POST is a utility.
func TestRouteOf(t *testing.T) {
	anthropic, openai, gemini := newAnthropicProvider(""), newOpenAIProvider("", false), newGeminiProvider("")
	bedrock := newBedrockProvider("us-east-1", "", "")
	post, get := http.MethodPost, http.MethodGet
	for _, c := range []struct {
		p            Provider
		method, path string
		want         RouteInfo
	}{
		{anthropic, post, "/v1/messages", RouteInfo{routeGenerate, readerAnthropic}},
		{anthropic, post, "/v1/messages/", RouteInfo{routeGenerate, readerAnthropic}},
		{anthropic, post, "/v1/messages?beta=true", RouteInfo{routeGenerate, readerAnthropic}},
		{anthropic, get, "/v1/messages", RouteInfo{Op: routeUtility}},
		{anthropic, post, "/v1/complete", RouteInfo{routeGenerate, readerAnthropicComplete}},
		{anthropic, post, "/v1/messages/count_tokens", RouteInfo{Op: routeCount}},
		{anthropic, post, "/v1/messages/batches", RouteInfo{routeBatch, readerAnthropicBatch}},
		{anthropic, post, "/v1/messages/batches/", RouteInfo{routeBatch, readerAnthropicBatch}},
		{anthropic, post, "/v1/messages/batches/msgbatch_1/cancel", RouteInfo{Op: routeUtility}},
		{anthropic, get, "/v1/messages/batches/msgbatch_1/results", RouteInfo{Op: routeUtility}},
		{anthropic, post, "/v1/messages/batches-export", RouteInfo{}},
		{anthropic, get, "/v1/models", RouteInfo{Op: routeUtility}},
		{anthropic, post, "/v1/files", RouteInfo{}},

		{openai, post, "/v1/chat/completions", RouteInfo{routeGenerate, readerOpenAIChat}},
		{openai, post, "/chat/completions/", RouteInfo{routeGenerate, readerOpenAIChat}},
		{openai, post, "/v1/chat/completions?api-version=1", RouteInfo{routeGenerate, readerOpenAIChat}},
		{openai, post, "/v1/completions", RouteInfo{routeGenerate, readerOpenAICompletions}},
		{openai, post, "/v1/responses", RouteInfo{routeGenerate, readerOpenAIResponses}},
		{openai, post, "/v1/responses/input_tokens", RouteInfo{Op: routeCount}},
		{openai, post, "/v1/responses/resp_1/cancel", RouteInfo{Op: routeUtility}},
		{openai, get, "/v1/responses/resp_1", RouteInfo{Op: routeUtility}},
		{openai, get, "/v1/models", RouteInfo{Op: routeUtility}},
		{openai, post, "/v1/embeddings", RouteInfo{Op: routeUtility}},
		{openai, post, "/v1/moderations", RouteInfo{Op: routeUtility}},
		{openai, post, "/v1/images/generations", RouteInfo{}},

		{gemini, post, "/v1beta/models/gemini-2.5-pro:generateContent", RouteInfo{routeGenerate, readerGemini}},
		{gemini, post, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", RouteInfo{routeGenerate, readerGemini}},
		{gemini, post, "/v1beta/models/gemini-2.5-pro:countTokens", RouteInfo{Op: routeCount}},
		{gemini, post, "/v1beta/models/gemini-2.5-pro:batchGenerateContent", RouteInfo{Op: routeBatch}},
		{gemini, post, "/v1beta/models/text-embedding-004:embedContent", RouteInfo{Op: routeUtility}},
		{gemini, get, "/v1beta/models", RouteInfo{Op: routeUtility}},
		{gemini, post, "/v1beta/cachedContents", RouteInfo{}},

		{bedrock, post, "/model/anthropic.claude-3-5-sonnet-20240620-v1:0/invoke", RouteInfo{routeGenerate, readerAnthropic}},
		{bedrock, post, "/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke-with-response-stream", RouteInfo{routeGenerate, readerAnthropic}},
		{bedrock, post, "/model/global.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke/", RouteInfo{routeGenerate, readerAnthropic}},
		{bedrock, post, "/model/meta.llama3-70b-instruct-v1:0/invoke", RouteInfo{routeGenerate, readerBedrockInvoke}},
		{bedrock, post, "/model/amazon.nova-pro-v1:0/invoke-with-response-stream", RouteInfo{routeGenerate, readerBedrockInvoke}},
		{bedrock, post, "/model/not-anthropic.claude/invoke", RouteInfo{routeGenerate, readerBedrockInvoke}},
		{bedrock, post, "/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/converse", RouteInfo{routeGenerate, readerBedrockConverse}},
		{bedrock, post, "/model/amazon.nova-pro-v1:0/converse-stream", RouteInfo{routeGenerate, readerBedrockConverse}},
		{bedrock, post, "/model/anthropic.claude-3-haiku-20240307-v1:0/count-tokens", RouteInfo{Op: routeCount}},
		{bedrock, post, "/model/anthropic.claude-3-haiku-20240307-v1:0/unknown", RouteInfo{}},
		{bedrock, post, "/async-invoke", RouteInfo{}},
	} {
		if got := routeOf(c.p, c.method, c.path); got != c.want {
			t.Errorf("%s %s %s = %+v, want %+v", c.p.Name(), c.method, c.path, got, c.want)
		}
	}
}
