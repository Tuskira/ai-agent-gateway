package turn

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
)

// Call is one call as the agent received it: the raw bodies and, when the
// gateway read them, their canonical form (the tee's conversation and
// answer). A nil Conversation or Answer means the raw body is read instead.
type Call struct {
	Request      []byte
	Response     []byte
	Conversation *conv.Conversation
	Answer       *conv.Answer
}

// RequestStateFromConversation is RequestState over a canonical
// conversation: the same fields, by the same rules, whatever wire format
// the gateway read. ok is false when there is nothing to read (no
// messages) or the conversation's version is newer than this package.
//
// Besides what RequestState reads, the text of a text document and the
// wire form of an opaque block (a kind the canonical shape has no slot
// for) in the new turn's user or system messages are harness_text, and in
// a tool result its content. When the request does not carry the whole
// conversation (history server_side or prompt), user_goal is only the
// turn's own user_text: the earlier turns are not all there to search,
// and an empty goal is unknown, not absent.
func RequestStateFromConversation(c *conv.Conversation) (State, bool) {
	s, ok := extractConversation(newReader(context.Background()), c)
	s.clip()
	return s, ok
}

// ResponseStateFromAnswer is ResponseState over a canonical answer.
func ResponseStateFromAnswer(a *conv.Answer) State {
	s := extractAnswer(newReader(context.Background()), a)
	s.clip()
	return s
}

// extractConversation is RequestStateFromConversation unclipped, read
// within r's bounds (as extractRequest reads a raw body).
func extractConversation(r *reader, c *conv.Conversation) (s State, ok bool) {
	if c == nil || c.Version > conv.ConversationVersion || len(c.Messages) == 0 {
		return s, false
	}
	msgs := c.Messages
	for _, m := range msgs {
		if !r.checkBlocks(m.Content, 0) {
			return s, true
		}
	}

	// Tool names and inputs by call id, so each result can say which tool
	// produced it and with what.
	names, inputs := map[string]string{}, map[string]string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == conv.ContentToolUse {
				names[b.ID], inputs[b.ID] = b.Name, inputText(b.Input)
			}
		}
	}

	// The new turn, as extractRequest finds it: the trailing run of
	// user/system messages before any trailing assistant prefill.
	end := len(msgs)
	for end > 0 && msgs[end-1].Role == "assistant" {
		end--
	}
	start := end
	for start > 0 && inTurn(msgs[start-1].Role) {
		start--
	}
	var userText, harness []string
	for _, m := range msgs[start:end] {
		if !r.ok(0) {
			break
		}
		if m.Role == "system" || m.Role == "developer" {
			if t := strings.TrimSpace(r.contentText(m.Content)); t != "" {
				harness = append(harness, t)
			}
			continue
		}
		for _, b := range m.Content {
			switch b.Type {
			case conv.ContentText:
				if t := typed(b.Text); t != "" {
					userText = append(userText, t)
				}
				harness = append(harness, harnessRe.FindAllString(b.Text, -1)...)
			case conv.ContentToolResult:
				s.addResult(ToolResult{Tool: names[b.ToolUseID], Content: r.contentText(b.Content),
					CallID: b.ToolUseID, CallInput: inputs[b.ToolUseID]})
			case conv.ContentDocument, conv.ContentOpaque:
				if t := strings.TrimSpace(blockText(b)); t != "" {
					harness = append(harness, t)
				}
			}
		}
	}
	s.HarnessText = strings.Join(harness, "\n\n")
	s.UserText = strings.Join(userText, "\n\n")

	if start > 0 && msgs[start-1].Role == "assistant" {
		s.PriorToolCalls = blockCalls(msgs[start-1].Content)
	}

	// The goal: the latest human-typed text anywhere in the conversation,
	// when the conversation is all there.
	s.UserGoal = s.UserText
	if strings.ToLower(c.History) != conv.HistoryFull { // history is case-insensitive
		return s, true
	}
	for i := start - 1; s.UserGoal == "" && i >= 0 && r.ok(0); i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var goal []string
		for _, b := range msgs[i].Content {
			if t := typed(b.Text); b.Type == conv.ContentText && t != "" {
				goal = append(goal, t)
			}
		}
		s.UserGoal = strings.Join(goal, "\n\n")
	}
	return s, true
}

// extractAnswer is ResponseStateFromAnswer unclipped, read within r's
// bounds.
func extractAnswer(r *reader, a *conv.Answer) State {
	var s State
	if a == nil || !r.checkBlocks(a.Content, 0) {
		return s
	}
	var text []string
	for _, b := range a.Content {
		if b.Type == conv.ContentText && b.Text != "" {
			text = append(text, b.Text)
		}
	}
	s.ResponseText = strings.TrimSpace(strings.Join(text, "\n"))
	s.ResponseToolCalls = blockCalls(a.Content)
	return s
}

// blockCalls is the tool calls among bs, at most maxToolItems.
func blockCalls(bs []conv.ContentBlock) []ToolCall {
	var out []ToolCall
	for _, b := range bs {
		if b.Type == conv.ContentToolUse && len(out) < maxToolItems {
			out = append(out, ToolCall{Name: b.Name, Input: inputText(b.Input), ID: b.ID})
		}
	}
	return out
}

// inputText is a tool call's input as the raw parsers read it: the JSON
// arguments, or, when they are a JSON string (free-form input, or the
// incomplete arguments of a cut stream), that string.
func inputText(raw json.RawMessage) string {
	var str string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &str) == nil {
		return str
	}
	return string(raw)
}

// blockText is the text a block carries: a text block's or a text
// document's text, or an opaque block's wire form, compact (a JSON string,
// when the gateway clipped it, is that string).
func blockText(b conv.ContentBlock) string {
	switch b.Type {
	case conv.ContentText, conv.ContentDocument:
		return b.Text
	case conv.ContentOpaque:
		if t := inputText(b.Raw); len(b.Raw) == 0 || b.Raw[0] == '"' {
			return t
		}
		var c bytes.Buffer
		if json.Compact(&c, b.Raw) != nil {
			return string(b.Raw)
		}
		return c.String()
	}
	return ""
}

// conversationTexts is every string of the canonical forms, as bodyTexts
// lists a raw body's: what the gateway decoded (a text document sent as
// base64, a tool call's arguments) can hold a value the raw strings show
// only encoded.
func conversationTexts(c *conv.Conversation, a *conv.Answer) []string {
	var out []string
	for _, v := range []any{c, a} {
		if b, err := json.Marshal(v); err == nil {
			out = append(out, bodyTexts(b)...)
		}
	}
	return out
}

// PrepareCallRequest is PrepareRequestContext for a call the gateway may
// have read: the state comes from the canonical conversation when there is
// one it can read, else from the raw body. Either way every string of the
// raw body (and of the conversation) is searched for values to remove, so
// no secret found anywhere in the raw body leaves.
func PrepareCallRequest(ctx context.Context, c Call) PreparedTurn {
	r := newReader(ctx)
	s, ok := extractConversation(r, c.Conversation)
	if r.reason != "" {
		return NotJudgedTurn(StageRequest, r.reason)
	}
	if !ok {
		return PrepareRequestContext(ctx, c.Request)
	}
	extra := append(bodyTexts(c.Request), conversationTexts(c.Conversation, nil)...)
	hits, err := s.scrub(ctx, StageRequest, extra, scanCacheOn.Load())
	if err != nil {
		return NotJudgedTurn(StageRequest, DeadlineReason)
	}
	return newTurn(StageRequest, &s, hits)
}

// PrepareCallResponse is PrepareResponseContext for a call the gateway may
// have read: the reply comes from the canonical answer, and the goal and
// tool results from the canonical conversation, when both are there; else
// both come from the raw bodies. Values are removed as PrepareCallRequest
// removes them, from both raw bodies.
func PrepareCallResponse(ctx context.Context, c Call) (PreparedTurn, bool) {
	r := newReader(ctx)
	req, ok := extractConversation(r, c.Conversation)
	if r.reason == "" && (!ok || c.Answer == nil) {
		return PrepareResponseContext(ctx, c.Request, c.Response)
	}
	s := extractAnswer(r, c.Answer)
	if r.reason != "" {
		return NotJudgedTurn(StageResponse, r.reason), true
	}
	if s.ResponseText == "" && len(s.ResponseToolCalls) == 0 {
		return PreparedTurn{}, false
	}
	s.UserGoal, s.ToolResults = req.UserGoal, req.ToolResults
	extra := append(append(bodyTexts(c.Request), bodyTexts(c.Response)...), conversationTexts(c.Conversation, c.Answer)...)
	hits, err := s.scrub(ctx, StageResponse, extra, scanCacheOn.Load())
	if err != nil {
		return NotJudgedTurn(StageResponse, DeadlineReason), true
	}
	return newTurn(StageResponse, &s, hits), true
}
