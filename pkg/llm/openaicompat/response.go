package openaicompat

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// InvalidArgumentsKey is the one key of the tool_use input this Provider
// returns when a vendor's tool-call arguments are not a JSON object: its
// value is the vendor's raw arguments text. The call still reaches the
// client (so the model sees its own mistake when the tool rejects the
// input) and nothing is replaced by an empty object.
const InvalidArgumentsKey = "_gateway_invalid_arguments"

// chatDelta is both a non-stream choices[].message and a stream choices[].delta.
// Reasoning and content stay raw: vendors disagree on their shapes.
type chatDelta struct {
	Content          json.RawMessage  `json:"content"`
	ReasoningContent json.RawMessage  `json:"reasoning_content"` // xAI, DeepSeek, GLM, Kimi
	Reasoning        json.RawMessage  `json:"reasoning"`         // Groq, Ollama
	ToolCalls        []chatToolCallIn `json:"tool_calls"`
}

type chatToolCallIn struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // a string, or an object on some vendors
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"` // DeepSeek
	CachedTokens         int64 `json:"cached_tokens"`           // Moonshot
}

// neutral converts usage: Chat prompt_tokens include the cached prefix, the
// neutral InputTokens exclude it.
func (u *chatUsage) neutral() llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	cached := u.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = u.PromptCacheHitTokens
	}
	if cached == 0 {
		cached = u.CachedTokens
	}
	return llm.Usage{InputTokens: max(0, u.PromptTokens-cached), OutputTokens: u.CompletionTokens, CacheReadTokens: cached}
}

type piece struct {
	kind llm.BlockType // text or thinking
	text string
}

// reasoningText reads the reasoning string whichever field the vendor uses.
func (d chatDelta) reasoningText() string {
	return firstNonEmpty(jsonString(d.ReasoningContent), jsonString(d.Reasoning))
}

// pieces reads content as a string, or as Mistral's part list where reasoning
// arrives as {"type":"thinking","thinking":[{"type":"text","text":…}]}.
func (d chatDelta) pieces() []piece {
	if s := jsonString(d.Content); s != "" {
		return []piece{{llm.BlockText, s}}
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking json.RawMessage `json:"thinking"`
	}
	if json.Unmarshal(d.Content, &parts) != nil {
		return nil
	}
	var out []piece
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, piece{llm.BlockText, p.Text})
		case "thinking":
			if s := jsonString(p.Thinking); s != "" {
				out = append(out, piece{llm.BlockThinking, s})
				continue
			}
			var inner []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(p.Thinking, &inner)
			for _, t := range inner {
				out = append(out, piece{llm.BlockThinking, t.Text})
			}
		}
	}
	return out
}

func jsonString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// argString returns a tool call's arguments fragment: a JSON string's value, or
// an object's own JSON.
func argString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// toolInput turns a vendor's complete arguments text into a tool_use input:
// the object itself, "{}" for no arguments, and anything else (invalid JSON,
// an array, a scalar) wrapped under InvalidArgumentsKey with the raw text
// preserved.
func toolInput(args string) json.RawMessage {
	switch {
	case strings.TrimSpace(args) == "":
		return json.RawMessage(`{}`)
	case isJSONObject(json.RawMessage(args)):
		return json.RawMessage(args)
	}
	b, _ := json.Marshal(map[string]string{InvalidArgumentsKey: args})
	return b
}

// stopReason maps a Chat finish_reason. Several vendors report "stop" alongside
// tool calls, and Claude Code only continues its tool loop on tool_use.
// content_filter stays end_turn: Claude Code treats refusal as an error. A
// matched stop sequence is reported only when the vendor names it (vLLM's
// choices[].stop_reason); OpenAI does not.
func stopReason(finish string, sawTool bool, stopSeq string) string {
	switch {
	case finish == "length":
		return "max_tokens"
	case sawTool:
		return "tool_use"
	case finish == "stop" && stopSeq != "":
		return "stop_sequence"
	}
	return "end_turn"
}

// matchedStop reads vLLM's choices[].stop_reason: the stop string that ended
// generation (a number is a stop token id, not a client stop sequence).
func matchedStop(raw json.RawMessage) string { return jsonString(raw) }

func newID(prefix string) string { return prefix + rand.Text() }

// ParseResponse converts a non-stream Chat Completions response.
func (Provider) ParseResponse(resp *http.Response) (*llm.Response, error) {
	out, err := parseResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("upstream response: %v", err)
	}
	return out, nil
}

// parseResponse converts a non-stream Chat Completions body; the error is
// the decoder's.
func parseResponse(body io.Reader) (*llm.Response, error) {
	var in struct {
		ID      string `json:"id"`
		Choices []struct {
			Message      chatDelta       `json:"message"`
			FinishReason string          `json:"finish_reason"`
			StopReason   json.RawMessage `json:"stop_reason"`
		} `json:"choices"`
		Usage *chatUsage `json:"usage"`
	}
	if err := json.NewDecoder(body).Decode(&in); err != nil {
		return nil, err
	}
	out := &llm.Response{Role: "assistant", Content: []llm.Block{}, Usage: in.Usage.neutral()}
	finish, stopSeq, sawTool := "", "", false
	if len(in.Choices) > 0 {
		ch := in.Choices[0]
		finish, stopSeq = ch.FinishReason, matchedStop(ch.StopReason)
		thinking := []string{ch.Message.reasoningText()}
		var texts []string
		for _, p := range ch.Message.pieces() {
			if p.kind == llm.BlockThinking {
				thinking = append(thinking, p.text)
			} else {
				texts = append(texts, p.text)
			}
		}
		if t := strings.Join(thinking, ""); t != "" {
			out.Content = append(out.Content, llm.Block{Type: llm.BlockThinking, Thinking: t})
		}
		if t := strings.Join(texts, ""); t != "" {
			out.Content = append(out.Content, llm.Block{Type: llm.BlockText, Text: t})
		}
		for _, tc := range ch.Message.ToolCalls {
			if tc.Function.Name == "" {
				continue // cannot be executed
			}
			out.Content = append(out.Content, llm.Block{Type: llm.BlockToolUse, ID: firstNonEmpty(tc.ID, newID("toolu_")),
				Name: tc.Function.Name, Input: toolInput(argString(tc.Function.Arguments))})
			sawTool = true
		}
	}
	// Reasoning models can spend every token on hidden reasoning and return no
	// content; an empty text block keeps the real stop_reason visible.
	if len(out.Content) == 0 {
		out.Content = append(out.Content, llm.Block{Type: llm.BlockText})
	}
	switch out.ID = in.ID; {
	case out.ID == "":
		out.ID = newID("msg_")
	case !strings.HasPrefix(out.ID, "msg_"):
		out.ID = "msg_" + out.ID
	}
	out.StopReason = stopReason(finish, sawTool, stopSeq)
	if out.StopReason == "stop_sequence" {
		out.StopSequence = stopSeq
	}
	return out, nil
}

// ParseError maps a vendor error body, keeping the vendor's message: Claude
// Code matches on its wording to retry with a feature turned off.
func (Provider) ParseError(status int, body []byte) *llm.Error {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	var obj struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(e.Error, &obj)
	msg := firstNonEmpty(firstNonEmpty(obj.Message, jsonString(e.Error)), e.Message)
	if msg == "" {
		if msg = strings.TrimSpace(string(body)); len(msg) > 1024 {
			msg = msg[:1024]
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	out := &llm.Error{Status: status, Type: llm.ErrorTypeForStatus(status), Message: msg}
	if jsonString(obj.Code) == "context_length_exceeded" {
		out.Code = llm.CodeContextLength
	}
	return out
}

// streamErrorType maps an error object inside a Chat stream.
func streamErrorType(code string, raw json.RawMessage) string {
	var status int
	switch {
	case code == "rate_limit_exceeded" || code == "insufficient_quota":
		return llm.ErrorTypeRateLimit
	case strings.Contains(code, "overload"):
		return llm.ErrorTypeOverloaded
	case json.Unmarshal(raw, &status) == nil && status >= 400:
		return llm.ErrorTypeForStatus(status) // numeric codes are HTTP statuses
	}
	return llm.ErrorTypeAPI
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
