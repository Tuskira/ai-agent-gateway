package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// geminiProvider forwards /gemini/* to the Google AI Studio (Generative
// Language) API byte-for-byte. The client's own x-goog-api-key rides along
// (BYOK). The model is in the path (/v1beta/models/{model}:generateContent).
// Reached via the /gemini/ prefix.
type geminiProvider struct {
	baseURL string // e.g. https://generativelanguage.googleapis.com
}

func newGeminiProvider(baseURL string) geminiProvider {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	return geminiProvider{baseURL: strings.TrimRight(baseURL, "/")}
}

func (geminiProvider) Name() string { return "gemini" }
func (geminiProvider) ID() string   { return "gemini" }

var reGeminiModel = regexp.MustCompile(`/models/([^/:]+)`)

// Model parses {model} from .../models/{model}:generateContent.
func (geminiProvider) Model(_ []byte, upstreamPath string) string {
	if m := reGeminiModel.FindStringSubmatch(upstreamPath); len(m) == 2 {
		return m[1]
	}
	return ""
}

func (p geminiProvider) BuildUpstream(ctx context.Context, r *http.Request, body []byte, upstreamPath string) (*http.Request, error) {
	url := p.baseURL + upstreamPath
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(up.Header, r.Header) // x-goog-api-key rides along (BYOK)
	return up, nil
}

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	// ThoughtsTokenCount is the reasoning ("thinking") tokens a thinking model
	// spends. Gemini reports them SEPARATE from candidatesTokenCount but bills
	// them as output, so they must be added to OutputTokens or cost undercounts.
	ThoughtsTokenCount int64 `json:"thoughtsTokenCount"`
	// ToolUsePromptTokenCount is a separate BILLED input bucket for tokens
	// consumed by enabled tools (Search grounding, code execution, function
	// calling); it is not included in PromptTokenCount, so add it to input.
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
	// TrafficType is the tier the call was served on (ON_DEMAND,
	// ON_DEMAND_PRIORITY, FLEX, ...), which prices it.
	TrafficType string `json:"trafficType"`
}

// serviceTier maps Gemini's trafficType onto the rate card's tier rows.
func (u geminiUsage) serviceTier() string {
	switch u.TrafficType {
	case "ON_DEMAND_PRIORITY", "PRIORITY":
		return "priority"
	case "FLEX", "ON_DEMAND_FLEX":
		return "flex"
	}
	return ""
}

// ParseUsage handles a non-stream generateContent response, an SSE (alt=sse)
// stream, and a plain JSON array (:streamGenerateContent without alt=sse). In
// every streamed form the final chunk carries the cumulative usageMetadata.
func (geminiProvider) ParseUsage(body []byte) Usage {
	if u, ok := parseGeminiObject(body); ok {
		return u
	}
	// Array mode: :streamGenerateContent without ?alt=sse returns a JSON array
	// of chunks ([{...},{...}]); the last chunk with usageMetadata wins.
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		var chunks []json.RawMessage
		if json.Unmarshal(trimmed, &chunks) == nil {
			var out Usage
			for _, c := range chunks {
				if u, ok := parseGeminiObject(c); ok {
					out = u
				}
			}
			return out
		}
	}
	var out Usage
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if u, ok := parseGeminiObject([]byte(payload)); ok {
			out = u // last one wins (cumulative)
		}
	}
	if out.InputTokens > 0 || out.OutputTokens > 0 {
		return out
	}
	// Fallback: an array/non-stream body larger than the scan window fails the
	// whole-body/array Unmarshal above, but its trailing usageMetadata object is
	// intact in the tail.
	if obj := lastJSONObjectForKey(body, "usageMetadata"); obj != nil {
		var um geminiUsage
		if json.Unmarshal(obj, &um) == nil && (um.PromptTokenCount > 0 || um.CandidatesTokenCount > 0) {
			return Usage{
				InputTokens:     um.PromptTokenCount + um.ToolUsePromptTokenCount,
				OutputTokens:    um.CandidatesTokenCount + um.ThoughtsTokenCount,
				CacheReadTokens: um.CachedContentTokenCount,
				ServiceTier:     um.serviceTier(),
			}
		}
	}
	return out
}

func parseGeminiObject(body []byte) (Usage, bool) {
	var full struct {
		UsageMetadata geminiUsage `json:"usageMetadata"`
		Candidates    []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if json.Unmarshal(body, &full) != nil {
		return Usage{}, false
	}
	if full.UsageMetadata.PromptTokenCount == 0 && full.UsageMetadata.CandidatesTokenCount == 0 {
		return Usage{}, false
	}
	u := Usage{
		InputTokens:     full.UsageMetadata.PromptTokenCount + full.UsageMetadata.ToolUsePromptTokenCount,
		OutputTokens:    full.UsageMetadata.CandidatesTokenCount + full.UsageMetadata.ThoughtsTokenCount,
		CacheReadTokens: full.UsageMetadata.CachedContentTokenCount,
		ServiceTier:     full.UsageMetadata.serviceTier(),
	}
	if len(full.Candidates) > 0 {
		u.StopReason = full.Candidates[0].FinishReason
	}
	return u, true
}
