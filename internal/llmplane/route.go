package llmplane

import (
	"net/http"
	"strings"
)

// What a route does (RouteInfo.Op).
const (
	routeGenerate = "generate" // one model call
	routeBatch    = "batch"    // many model calls submitted at once
	routeCount    = "count"    // token counting: no generation
	routeUtility  = "utility"  // listing, retrieval, management: no generation
)

// Reader names a route declares (pkg/llm registry names): an llm.Reader's
// on a generate route, an llm.BatchReader's on a batch route. Every one is
// registered by the reader packages cmd/gateway blank-imports.
const (
	readerAnthropic         = "anthropic"
	readerAnthropicComplete = "anthropic_complete"
	readerOpenAIChat        = "openai_chat"
	readerOpenAICompletions = "openai_completions"
	readerAnthropicBatch    = "anthropic_batch"
	readerOpenAIResponses   = "openai_responses"
	readerGemini            = "gemini"
	readerGeminiBatch       = "gemini_batch"
	readerBedrockInvoke     = "bedrock_invoke"
	readerBedrockConverse   = "bedrock_converse"
)

// RouteInfo describes one endpoint of a Provider. Op is one of the op*
// constants, "" for an endpoint the Provider does not know. Reader is the
// pkg/llm reader name of the wire format the client speaks there (its
// request, and the response it gets back, translated or not): the format to
// read the call in, and the one to render a gateway error in. "" when the
// format has no name yet.
type RouteInfo struct {
	Op     string
	Reader string
}

// routeDescriber is implemented by a Provider that can say what each of its
// endpoints does. A Provider without it describes nothing (RouteInfo{}).
type routeDescriber interface {
	Route(method, upstreamPath string) RouteInfo
}

// routeOf is p's description of method upstreamPath. A query string and a
// trailing slash are ignored; any method but POST runs no generation.
func routeOf(p Provider, method, upstreamPath string) RouteInfo {
	rd, ok := p.(routeDescriber)
	if !ok {
		return RouteInfo{}
	}
	if method != http.MethodPost {
		return RouteInfo{Op: routeUtility}
	}
	if i := strings.IndexByte(upstreamPath, '?'); i >= 0 {
		upstreamPath = upstreamPath[:i]
	}
	if len(upstreamPath) > 1 {
		upstreamPath = strings.TrimRight(upstreamPath, "/")
	}
	return rd.Route(method, upstreamPath)
}

// Route describes the Anthropic API. Only POST /v1/messages/batches creates
// a batch; everything else under it manages one.
func (anthropicProvider) Route(_, path string) RouteInfo {
	switch {
	case path == "/v1/messages":
		return RouteInfo{Op: routeGenerate, Reader: readerAnthropic}
	case path == "/v1/complete":
		return RouteInfo{Op: routeGenerate, Reader: readerAnthropicComplete}
	case path == "/v1/messages/count_tokens":
		return RouteInfo{Op: routeCount}
	case path == "/v1/messages/batches":
		return RouteInfo{Op: routeBatch, Reader: readerAnthropicBatch}
	case strings.HasPrefix(path, "/v1/messages/batches/"), path == "/v1/models", strings.HasPrefix(path, "/v1/models/"):
		return RouteInfo{Op: routeUtility}
	}
	return RouteInfo{}
}

// Route describes the OpenAI API by path suffix, so a base URL with or
// without /v1 matches the same way.
func (openaiProvider) Route(_, path string) RouteInfo {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return RouteInfo{Op: routeGenerate, Reader: readerOpenAIChat}
	case strings.HasSuffix(path, "/completions"):
		return RouteInfo{Op: routeGenerate, Reader: readerOpenAICompletions}
	case strings.HasSuffix(path, "/responses"):
		return RouteInfo{Op: routeGenerate, Reader: readerOpenAIResponses}
	case strings.HasSuffix(path, "/responses/input_tokens"):
		return RouteInfo{Op: routeCount}
	case strings.Contains(path, "/responses/"), strings.HasSuffix(path, "/models"), strings.Contains(path, "/models/"),
		strings.HasSuffix(path, "/embeddings"), strings.HasSuffix(path, "/moderations"):
		return RouteInfo{Op: routeUtility}
	}
	return RouteInfo{}
}

// Route describes the Gemini API by its method suffix
// (.../models/{model}:generateContent).
func (geminiProvider) Route(_, path string) RouteInfo {
	switch {
	case strings.HasSuffix(path, ":generateContent"), strings.HasSuffix(path, ":streamGenerateContent"):
		return RouteInfo{Op: routeGenerate, Reader: readerGemini}
	case strings.HasSuffix(path, ":batchGenerateContent"):
		return RouteInfo{Op: routeBatch, Reader: readerGeminiBatch}
	case strings.HasSuffix(path, ":countTokens"):
		return RouteInfo{Op: routeCount}
	case strings.HasSuffix(path, ":embedContent"), strings.HasSuffix(path, ":batchEmbedContents"):
		return RouteInfo{Op: routeUtility}
	}
	return RouteInfo{}
}

// Route describes the Bedrock runtime (/model/{id}/{action}). InvokeModel
// takes the model's own format, which for an Anthropic model is the
// Messages body (its stream is the Messages events in eventstream frames).
func (*bedrockProvider) Route(_, path string) RouteInfo {
	if !strings.HasPrefix(path, "/model/") {
		return RouteInfo{}
	}
	switch action := path[strings.LastIndexByte(path, '/'):]; action {
	case "/invoke", "/invoke-with-response-stream":
		if isAnthropicBedrockModel(parseModelID(path)) {
			return RouteInfo{Op: routeGenerate, Reader: readerAnthropic}
		}
		return RouteInfo{Op: routeGenerate, Reader: readerBedrockInvoke}
	case "/converse", "/converse-stream":
		return RouteInfo{Op: routeGenerate, Reader: readerBedrockConverse}
	case "/count-tokens":
		return RouteInfo{Op: routeCount}
	}
	return RouteInfo{}
}
