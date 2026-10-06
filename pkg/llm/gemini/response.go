package gemini

import (
	"encoding/json"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// chunk is one GenerateContentResponse: a whole non-stream answer, or one
// frame of a stream.
type chunk struct {
	id, model   string
	parts       []llm.Block // of the first candidate
	finish      string      // finishReason of the first candidate
	blockReason string      // promptFeedback.blockReason
	usage       *llm.Usage
	err         *llm.Error // an error frame
}

func (c *chunk) sawTool() bool {
	for _, b := range c.parts {
		if b.Type == llm.BlockToolUse {
			return true
		}
	}
	return false
}

// stopReason maps the chunk's finishReason (or a blocked prompt).
func (c *chunk) stopReason(sawTool bool) string {
	if c.blockReason != "" {
		return "refusal"
	}
	return stopReason(c.finish, sawTool)
}

// stopReason maps a finishReason. Gemini reports STOP alongside function
// calls and when a stop sequence matched (without naming it), so STOP is
// tool_use after a call and end_turn otherwise. The content-policy reasons
// are refusal; the rest (OTHER, MALFORMED_FUNCTION_CALL, ...) end_turn.
func stopReason(finish string, sawTool bool) string {
	switch finish {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return "refusal"
	}
	if sawTool {
		return "tool_use"
	}
	return "end_turn"
}

// readChunk reads one GenerateContentResponse; calls numbers its function
// calls (shared across the frames of a stream).
func readChunk(raw []byte, ids *calls) (*chunk, error) {
	top, err := object(raw)
	if err != nil {
		return nil, err
	}
	c := &chunk{}
	if e, ok := top["error"]; ok && !isNull(e) {
		c.err = readError(e)
		return c, nil
	}
	var candidates []json.RawMessage
	for _, f := range []struct {
		key string
		dst any
	}{
		{"responseId", &c.id}, {"modelVersion", &c.model}, {"candidates", &candidates},
	} {
		if err := take(top, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	if len(candidates) > 0 {
		cand, err := object(candidates[0])
		if err != nil {
			return nil, fmt.Errorf("candidates.0: %v", err)
		}
		if err := take(cand, "finishReason", &c.finish); err != nil {
			return nil, fmt.Errorf("candidates.0.%v", err)
		}
		content, err := takeObject(cand, "content")
		if err != nil {
			return nil, fmt.Errorf("candidates.0.%v", err)
		}
		var role string
		var parts []json.RawMessage
		if err := take(content, "role", &role); err != nil {
			return nil, fmt.Errorf("candidates.0.content.%v", err)
		}
		if role != "" && role != "model" {
			return nil, fmt.Errorf("candidates.0.content.role: unknown role %q", role)
		}
		if err := take(content, "parts", &parts); err != nil {
			return nil, fmt.Errorf("candidates.0.content.%v", err)
		}
		for j, p := range parts {
			b, err := ids.part(p)
			if err != nil {
				return nil, fmt.Errorf("candidates.0.content.parts.%d: %v", j, err)
			}
			c.parts = append(c.parts, b)
		}
	}
	feedback, err := takeObject(top, "promptFeedback")
	if err != nil {
		return nil, err
	}
	if err := take(feedback, "blockReason", &c.blockReason); err != nil {
		return nil, fmt.Errorf("promptFeedback.%v", err)
	}
	if c.blockReason == "BLOCK_REASON_UNSPECIFIED" {
		c.blockReason = ""
	}
	usage, err := takeObject(top, "usageMetadata")
	if err != nil {
		return nil, err
	}
	if usage != nil {
		if c.usage, err = readUsage(usage); err != nil {
			return nil, fmt.Errorf("usageMetadata.%v", err)
		}
	}
	return c, nil
}

// readUsage converts usageMetadata to the neutral convention: Gemini's
// promptTokenCount includes the cached prefix (subtracted) and excludes the
// tool-use prompt (added); its thoughts are billed as output.
func readUsage(m map[string]json.RawMessage) (*llm.Usage, error) {
	var prompt, candidates, cached, thoughts, toolUse int64
	for _, f := range []struct {
		key string
		dst *int64
	}{
		{"promptTokenCount", &prompt}, {"candidatesTokenCount", &candidates}, {"cachedContentTokenCount", &cached},
		{"thoughtsTokenCount", &thoughts}, {"toolUsePromptTokenCount", &toolUse},
	} {
		if err := take(m, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	return &llm.Usage{
		InputTokens:     max(0, prompt-cached) + toolUse,
		OutputTokens:    candidates + thoughts,
		CacheReadTokens: cached,
	}, nil
}

// readError reads a Google API error object ({code, message, status}).
func readError(raw json.RawMessage) *llm.Error {
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(raw, &e) != nil {
		_ = json.Unmarshal(raw, &e.Message)
	}
	typ := llm.ErrorTypeAPI
	switch {
	case e.Status == "RESOURCE_EXHAUSTED":
		typ = llm.ErrorTypeRateLimit
	case e.Status == "UNAVAILABLE":
		typ = llm.ErrorTypeOverloaded
	case e.Code >= 400:
		typ = llm.ErrorTypeForStatus(e.Code)
	}
	if e.Message == "" {
		e.Message = "upstream error"
	}
	return &llm.Error{Type: typ, Message: e.Message}
}
