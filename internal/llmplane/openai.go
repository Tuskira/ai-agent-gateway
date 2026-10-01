package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// openaiProvider forwards /openai/* to the OpenAI API byte-for-byte (native
// passthrough). With streamUsage on (opt-in, off by default), a Chat
// Completions stream without stream_options also gets include_usage added
// (InjectStreamUsage). The client's own Authorization: Bearer key rides along
// (BYOK).
type openaiProvider struct {
	baseURL     string // e.g. https://api.openai.com
	streamUsage bool   // opt-in: inject stream_options.include_usage (InjectStreamUsage)
}

func newOpenAIProvider(baseURL string, streamUsage bool) openaiProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	return openaiProvider{baseURL: strings.TrimRight(baseURL, "/"), streamUsage: streamUsage}
}

// streamUsageOption is spliced in as the first member of the request object.
// Only the leading bytes change; messages/tools stay byte-identical.
var streamUsageOption = []byte(`"stream_options":{"include_usage":true},`)

// InjectStreamUsage makes a Chat Completions stream report its usage: without
// stream_options.include_usage OpenAI sends no token counts on a stream, so the
// call's cost would be unknown. It applies only to a streaming
// /v1/chat/completions request that sets no stream_options at all (an explicit
// client choice is left alone), and returns (body, false) otherwise. The router
// strips the resulting usage-only chunk from the client's stream; the other
// chunks keep the "usage":null member OpenAI then adds to each.
func (p openaiProvider) InjectStreamUsage(body []byte, upstreamPath string) ([]byte, bool) {
	if !p.streamUsage || !strings.HasSuffix(upstreamPath, "/chat/completions") {
		return body, false
	}
	var req struct {
		Stream        bool            `json:"stream"`
		StreamOptions json.RawMessage `json:"stream_options"`
	}
	if json.Unmarshal(body, &req) != nil || !req.Stream || req.StreamOptions != nil {
		return body, false
	}
	i := bytes.IndexByte(body, '{')
	if i < 0 {
		return body, false
	}
	out := make([]byte, 0, len(body)+len(streamUsageOption))
	out = append(append(append(out, body[:i+1]...), streamUsageOption...), body[i+1:]...)
	if !json.Valid(out) {
		return body, false
	}
	return out, true
}

func (openaiProvider) Name() string                       { return "openai" }
func (openaiProvider) ID() string                         { return "openai" }
func (openaiProvider) Model(body []byte, _ string) string { return modelFromBody(body) }

func (p openaiProvider) BuildUpstream(ctx context.Context, r *http.Request, body []byte, upstreamPath string) (*http.Request, error) {
	url := p.baseURL + upstreamPath
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(up.Header, r.Header) // Authorization: Bearer rides along (BYOK)
	return up, nil
}

// openaiUsage covers both dialects that pass through /openai/*: Chat Completions
// (prompt_tokens/completion_tokens) and the Responses API
// (input_tokens/output_tokens). The accessors prefer the Chat fields and fall
// back to the Responses ones, so either schema is captured.
type openaiUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u openaiUsage) input() int64 {
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}
	return u.InputTokens
}
func (u openaiUsage) output() int64 {
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}
	return u.OutputTokens
}
func (u openaiUsage) cached() int64 {
	if u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	return u.InputTokensDetails.CachedTokens
}
func (u openaiUsage) empty() bool { return u.input() == 0 && u.output() == 0 }

// ParseUsage reads OpenAI usage from a non-stream response, or the final SSE
// chunk. A Chat Completions stream carries usage only with
// stream_options.include_usage, which InjectStreamUsage adds when the operator
// opts in; without it the tokens are unknown and the cost is stored as NULL.
func (openaiProvider) ParseUsage(body []byte) Usage {
	if u, ok := parseOpenAIObject(body); ok {
		return u
	}
	var out Usage
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Usage       *openaiUsage `json:"usage"` // Chat Completions final chunk
			ServiceTier string       `json:"service_tier"`
			Choices     []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Response *struct { // Responses API response.completed event
				Usage       *openaiUsage `json:"usage"`
				ServiceTier string       `json:"service_tier"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		u := ev.Usage
		if ev.ServiceTier != "" {
			out.ServiceTier = ev.ServiceTier
		}
		if u == nil && ev.Response != nil {
			u = ev.Response.Usage
			out.ServiceTier = firstNonEmpty(ev.Response.ServiceTier, out.ServiceTier)
		}
		if u != nil && !u.empty() {
			out.InputTokens = u.input()
			out.OutputTokens = u.output()
			out.CacheReadTokens = u.cached()
		}
		if len(ev.Choices) > 0 && ev.Choices[0].FinishReason != "" {
			out.StopReason = ev.Choices[0].FinishReason
		}
	}
	if out.InputTokens > 0 || out.OutputTokens > 0 {
		return out
	}
	// Fallback: a non-stream body larger than the usage scan window fails the
	// whole-body Unmarshal, but its trailing usage object is intact.
	if obj := lastJSONObjectForKey(body, "usage"); obj != nil {
		var uu openaiUsage
		if json.Unmarshal(obj, &uu) == nil && !uu.empty() {
			out.InputTokens, out.OutputTokens, out.CacheReadTokens = uu.input(), uu.output(), uu.cached()
		}
	}
	return out
}

func parseOpenAIObject(body []byte) (Usage, bool) {
	var full struct {
		Usage       openaiUsage `json:"usage"`
		ServiceTier string      `json:"service_tier"` // the tier actually served (default, priority, flex, ...)
		Choices     []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &full) == nil && !full.Usage.empty() {
		u := Usage{
			InputTokens:     full.Usage.input(),
			OutputTokens:    full.Usage.output(),
			CacheReadTokens: full.Usage.cached(),
			ServiceTier:     full.ServiceTier,
		}
		if len(full.Choices) > 0 {
			u.StopReason = full.Choices[0].FinishReason
		}
		return u, true
	}
	return Usage{}, false
}
