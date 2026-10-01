package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// anthropicProvider forwards /v1/* to the Anthropic API byte-for-byte. The body
// is never re-marshaled, so cache_control blocks stay byte-identical and prompt
// caching survives. The client's own x-api-key/Authorization rides along (BYOK).
type anthropicProvider struct {
	baseURL string // e.g. https://api.anthropic.com
}

func newAnthropicProvider(baseURL string) anthropicProvider {
	return anthropicProvider{baseURL: strings.TrimRight(baseURL, "/")}
}

func (anthropicProvider) Name() string { return "anthropic" }
func (anthropicProvider) ID() string   { return "anthropic" }

// Model reads the model from the request JSON body (stored verbatim).
func (anthropicProvider) Model(body []byte, _ string) string { return modelFromBody(body) }

func (p anthropicProvider) BuildUpstream(ctx context.Context, r *http.Request, body []byte, upstreamPath string) (*http.Request, error) {
	url := p.baseURL + upstreamPath
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(up.Header, r.Header)
	return up, nil
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	// CacheCreation splits the aggregate above into TTL buckets; the 1-hour
	// slice bills at 2x base input vs 1.25x for the 5-minute default.
	CacheCreation struct {
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	// ServerToolUse carries per-request server-tool counts (web search is billed
	// $10/1,000 requests on top of tokens).
	ServerToolUse struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
	// Pricing modifiers: fast mode and US-only inference.
	Speed        string `json:"speed"`
	InferenceGeo string `json:"inference_geo"`
}

func (u anthropicUsage) into(id, stop string) Usage {
	return Usage{
		InputTokens:           u.InputTokens,
		OutputTokens:          u.OutputTokens,
		CacheCreationTokens:   u.CacheCreationInputTokens,
		CacheCreation1hTokens: u.CacheCreation.Ephemeral1h,
		CacheReadTokens:       u.CacheReadInputTokens,
		WebSearchRequests:     u.ServerToolUse.WebSearchRequests,
		Speed:                 u.Speed,
		InferenceGeo:          u.InferenceGeo,
		StopReason:            stop,
		ProviderRequestID:     id,
	}
}

// ParseUsage reads token accounting from either a non-streaming JSON response or
// an SSE stream (message_start carries the initial input+cache, message_delta
// carries the cumulative output_tokens + stop_reason, and — with server-tool-use
// / web_search — a grown cumulative input+cache too).
func (anthropicProvider) ParseUsage(body []byte) Usage {
	var full struct {
		ID         string         `json:"id"`
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &full); err == nil && (full.Usage.InputTokens > 0 || full.Usage.OutputTokens > 0) {
		return full.Usage.into(full.ID, full.StopReason)
	}
	// count_tokens returns usage fields at the TOP level: {"input_tokens": N}.
	var top anthropicUsage
	if err := json.Unmarshal(body, &top); err == nil && (top.InputTokens > 0 || top.OutputTokens > 0) {
		return top.into("", "")
	}
	if u := parseAnthropicSSE(body); u.InputTokens > 0 || u.OutputTokens > 0 {
		return u
	}
	// Fallback: a non-stream body larger than the usage scan window fails the
	// whole-body Unmarshal above, but its trailing usage object is intact.
	if obj := lastJSONObjectForKey(body, "usage"); obj != nil {
		var uu anthropicUsage
		if json.Unmarshal(obj, &uu) == nil && (uu.InputTokens > 0 || uu.OutputTokens > 0) {
			stop := ""
			if m := reAnthropicStop.FindAllSubmatch(body, -1); len(m) > 0 {
				stop = string(m[len(m)-1][1]) // stop_reason sits just before usage
			}
			return uu.into("", stop)
		}
	}
	return Usage{}
}

var reAnthropicStop = regexp.MustCompile(`"stop_reason"\s*:\s*"([a-z_]+)"`)

func parseAnthropicSSE(body []byte) Usage {
	var u Usage
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
			Type    string `json:"type"`
			Message struct {
				ID    string         `json:"id"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage anthropicUsage `json:"usage"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			u.Speed, u.InferenceGeo = ev.Message.Usage.Speed, ev.Message.Usage.InferenceGeo
			u.InputTokens = ev.Message.Usage.InputTokens
			u.CacheCreationTokens = ev.Message.Usage.CacheCreationInputTokens
			u.CacheCreation1hTokens = ev.Message.Usage.CacheCreation.Ephemeral1h
			u.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
			u.WebSearchRequests = ev.Message.Usage.ServerToolUse.WebSearchRequests
			u.ProviderRequestID = ev.Message.ID
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				u.OutputTokens = ev.Usage.OutputTokens
			}
			// Server-tool-use / web_search grow the cumulative input+cache and
			// re-report them here; overwrite only when present so a plain delta
			// (output only) doesn't zero the message_start values.
			if ev.Usage.InputTokens > 0 {
				u.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				u.CacheCreationTokens = ev.Usage.CacheCreationInputTokens
			}
			if ev.Usage.CacheCreation.Ephemeral1h > 0 {
				u.CacheCreation1hTokens = ev.Usage.CacheCreation.Ephemeral1h
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				u.CacheReadTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.ServerToolUse.WebSearchRequests > 0 {
				u.WebSearchRequests = ev.Usage.ServerToolUse.WebSearchRequests
			}
			if ev.Delta.StopReason != "" {
				u.StopReason = ev.Delta.StopReason
			}
		}
	}
	return u
}
