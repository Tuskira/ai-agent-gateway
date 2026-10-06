package turn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"regexp"
	"strings"
)

// Stage is when a rule runs.
type Stage string

const (
	StageRequest  Stage = "request"
	StageResponse Stage = "response"
)

// State is what a stage's questions are asked over. Only the NEW content of
// a turn is included -- an agent resends its whole history on every call, so
// judging history again would re-flag the same content on every turn. Field
// names are what rule instructions reference in backticks.
type State struct {
	UserText string `json:"user_text,omitempty"`
	UserGoal string `json:"user_goal,omitempty"`
	// HarnessText is the turn's text that is not the user's words: the
	// <system-reminder> sections cut out of user_text and the turn's
	// system/developer messages. Not the goal and not judged as the user's
	// text, but not hidden either: a payload can be wrapped in the tag.
	HarnessText       string       `json:"harness_text,omitempty"`
	ToolResults       []ToolResult `json:"tool_results,omitempty"`
	PriorToolCalls    []ToolCall   `json:"prior_tool_calls,omitempty"`
	ResponseText      string       `json:"response_text,omitempty"`
	ResponseToolCalls []ToolCall   `json:"response_tool_calls,omitempty"`
}

// ToolResult is one tool output returned into the turn. Untrusted.
type ToolResult struct {
	Tool    string `json:"tool,omitempty"`
	Content string `json:"content"`
	// Not sent to the judge model; kept so a decision can point at the event.
	CallID    string `json:"-"` // tool_use id / tool_call_id
	CallInput string `json:"-"` // the input of the call that produced this
}

// ToolCall is one tool invocation the assistant made or proposed.
type ToolCall struct {
	Name  string `json:"name"`
	Input string `json:"input,omitempty"`
	ID    string `json:"-"` // tool_use id, not sent to the judge model
}

// Per-field caps keep a stage's state sized to fit the judge model's context
// (with room for the longest question) whatever the agent sends.
const (
	capText       = 8000
	capToolResult = 6000
	capToolInput  = 2000
	maxToolItems  = 8
	// capName bounds a tool name or call id: short in every real turn, but
	// a string the agent sends all the same.
	capName = 256
)

// harnessRe matches text an agent harness injects into the user turn
// (Claude Code's <system-reminder> sections): not typed by the user, so
// neither judged as the user's words nor treated as the user's goal. It is
// carried as harness_text instead.
var harnessRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// typed is what the human actually typed in a text block.
func typed(t string) string { return strings.TrimSpace(harnessRe.ReplaceAllString(t, "")) }

// inTurn reports whether a message of role is part of a new turn: the
// user's, a tool's result ("function" is the legacy OpenAI tool role), or
// a system/developer message interleaved with them.
func inTurn(role string) bool {
	return role == "user" || role == "tool" || role == "function" || role == "system" || role == "developer"
}

// message is the union of the Anthropic Messages and OpenAI Chat shapes.
type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`   // OpenAI assistant
	ToolCallID string          `json:"tool_call_id"` // OpenAI tool
	Name       string          `json:"name"`
	// FunctionCall is the legacy OpenAI assistant call (one per message, no
	// id), answered by a "function" message.
	FunctionCall *oaiFunction `json:"function_call"`
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   content         `json:"content"`
}

// RequestState extracts the new turn from a request body: the final
// message's user text and tool results, the tool calls that produced them,
// and the latest thing the user actually typed (the goal that authorizes
// actions), each clipped to its cap. ok is false when the body is not a
// chat request.
func RequestState(body []byte) (State, bool) {
	s, ok := extractRequest(newReader(context.Background()), body)
	s.clip()
	return s, ok
}

// extractRequest is RequestState unclipped: the secret scan must see a
// secret whole before clipping can cut it (see scrub). It reads within
// r's bounds: once r has a reason, s is not to be sent.
func extractRequest(r *reader, body []byte) (s State, ok bool) {
	var req struct {
		Messages []message `json:"messages"`
	}
	// Strict decoding (exact names, a repeated name is an error): the v1
	// decoder matches names case-insensitively, so a body carrying both
	// "messages" and "Messages" (or "content" and "Content") would be judged
	// on one and sent to the model with the other.
	if jsonv2.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return s, false
	}
	msgs := req.Messages

	// Each message's content, decoded once (nested content included).
	cs := make([][]block, len(msgs))
	for i, m := range msgs {
		if cs[i] = r.content(m.Content); !r.ok(len(m.ToolCalls)) {
			return s, true
		}
	}

	// Tool names by call id, so each result can say which tool produced it.
	names, inputs := map[string]string{}, map[string]string{}
	for i, m := range msgs {
		for _, b := range cs[i] {
			if b.Type == "tool_use" {
				names[b.ID], inputs[b.ID] = b.Name, string(b.Input)
			}
		}
		for _, tc := range m.ToolCalls {
			names[tc.ID], inputs[tc.ID] = tc.Function.Name, tc.Function.Arguments
		}
	}

	// A body may end in assistant messages: a prefill the model is to
	// continue ("Sure,"). The new turn is the user/tool run before them;
	// stopping at the prefill would leave nothing to judge, and a prefill is
	// exactly where an attack hides. Only a trailing run is skipped, so a
	// body with no user turn after the last reply (a continuation of a cut
	// reply) judges the user turn before that reply again: once per such
	// call, the price of never judging an empty turn.
	end := len(msgs)
	for end > 0 && msgs[end-1].Role == "assistant" {
		end--
	}

	// The new turn: the trailing run of user/tool messages (OpenAI puts each
	// tool result in its own "tool" message). System/developer messages are
	// transparent: agents interleave them (Claude Code appends a "system"
	// message of environment reminders after the user's).
	start := end
	for start > 0 && inTurn(msgs[start-1].Role) {
		start--
	}
	var userText, harness []string
	for i := start; i < end && r.ok(0); i++ {
		m := msgs[i]
		if m.Role == "system" || m.Role == "developer" {
			if t := strings.TrimSpace(r.textOf(cs[i])); t != "" {
				harness = append(harness, t)
			}
			continue
		}
		if m.Role == "tool" {
			s.addResult(ToolResult{Tool: firstNonEmpty(names[m.ToolCallID], m.Name), Content: r.textOf(cs[i]),
				CallID: m.ToolCallID, CallInput: inputs[m.ToolCallID]})
			continue
		}
		if m.Role == "function" { // legacy: no call id, the function's name
			s.addResult(ToolResult{Tool: m.Name, Content: r.textOf(cs[i])})
			continue
		}
		for _, b := range cs[i] {
			switch b.Type {
			case "text":
				if t := typed(b.Text); t != "" {
					userText = append(userText, t)
				}
				harness = append(harness, harnessRe.FindAllString(b.Text, -1)...)
			case "tool_result":
				s.addResult(ToolResult{Tool: names[b.ToolUseID], Content: r.textOf(b.Content),
					CallID: b.ToolUseID, CallInput: inputs[b.ToolUseID]})
			}
		}
	}
	s.HarnessText = strings.Join(harness, "\n\n")
	s.UserText = strings.Join(userText, "\n\n")

	// The assistant message right before the turn made the calls whose
	// results are in it.
	if start > 0 && msgs[start-1].Role == "assistant" {
		s.PriorToolCalls = toolCalls(msgs[start-1], cs[start-1])
	}

	// The goal: the latest human-typed text anywhere in the conversation.
	s.UserGoal = s.UserText
	for i := start - 1; s.UserGoal == "" && i >= 0 && r.ok(0); i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var goal []string
		for _, b := range cs[i] {
			if t := typed(b.Text); b.Type == "text" && t != "" {
				goal = append(goal, t)
			}
		}
		s.UserGoal = strings.Join(goal, "\n\n")
	}
	return s, true
}

func (s *State) addResult(r ToolResult) {
	r.Content = strings.TrimSpace(r.Content)
	if r.Content == "" || len(s.ToolResults) >= maxToolItems {
		return
	}
	s.ToolResults = append(s.ToolResults, r)
}

// ResponseState extracts the assistant's text and proposed tool calls from
// a response body: an Anthropic or OpenAI SSE stream, or a plain JSON body,
// each clipped to its cap.
func ResponseState(body []byte) State {
	s := extractResponse(newReader(context.Background()), body)
	s.clip()
	return s
}

// extractResponse is ResponseState unclipped, read within r's bounds.
func extractResponse(r *reader, body []byte) State {
	var s State
	text, calls := parseSSE(r, body)
	if text == "" && len(calls) == 0 && r.ok(0) {
		text, calls = parseJSONResponse(r, body)
	}
	s.ResponseText = strings.TrimSpace(text)
	if len(calls) > maxToolItems {
		calls = calls[:maxToolItems]
	}
	s.ResponseToolCalls = calls
	return s
}

func parseSSE(r *reader, body []byte) (string, []ToolCall) {
	var text strings.Builder
	var calls []ToolCall
	var inputs []*strings.Builder
	byIndex := map[int]int{} // content block / tool call index → calls index
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for n := 0; sc.Scan(); n++ {
		if n%256 == 0 && !r.ok(0) {
			return "", nil
		}
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock block  `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Type { // Anthropic
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				byIndex[ev.Index] = len(calls)
				calls = append(calls, ToolCall{Name: ev.ContentBlock.Name, ID: ev.ContentBlock.ID})
				inputs = append(inputs, &strings.Builder{})
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
			case "input_json_delta":
				if i, ok := byIndex[ev.Index]; ok {
					inputs[i].WriteString(ev.Delta.PartialJSON)
				}
			}
		}
		for _, c := range ev.Choices { // OpenAI
			text.WriteString(c.Delta.Content)
			for _, tc := range c.Delta.ToolCalls {
				i, ok := byIndex[tc.Index]
				if !ok {
					i = len(calls)
					byIndex[tc.Index] = i
					calls = append(calls, ToolCall{})
					inputs = append(inputs, &strings.Builder{})
				}
				if tc.Function.Name != "" {
					calls[i].Name = tc.Function.Name
				}
				inputs[i].WriteString(tc.Function.Arguments)
			}
		}
	}
	for i := range calls {
		calls[i].Input = inputs[i].String()
	}
	return text.String(), calls
}

func parseJSONResponse(rd *reader, body []byte) (string, []ToolCall) {
	var r struct {
		Content json.RawMessage `json:"content"` // Anthropic
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"` // OpenAI
	}
	if json.Unmarshal(body, &r) != nil {
		return "", nil
	}
	m := message{Role: "assistant", Content: r.Content}
	if len(r.Choices) > 0 {
		m = r.Choices[0].Message
	}
	bs := rd.content(m.Content)
	return rd.textOf(bs), toolCalls(m, bs)
}

// toolCalls is the tool calls of m, whose content decoded is bs.
func toolCalls(m message, bs []block) []ToolCall {
	var out []ToolCall
	for _, b := range bs {
		if b.Type == "tool_use" {
			out = append(out, ToolCall{Name: b.Name, Input: string(b.Input), ID: b.ID})
		}
	}
	for _, tc := range m.ToolCalls {
		out = append(out, ToolCall{Name: tc.Function.Name, Input: tc.Function.Arguments, ID: tc.ID})
	}
	if f := m.FunctionCall; f != nil {
		out = append(out, ToolCall{Name: f.Name, Input: f.Arguments})
	}
	if len(out) > maxToolItems {
		out = out[:maxToolItems]
	}
	return out
}

// Clip bounds s to n bytes on a rune boundary, keeping the head and tail
// (instructions hide at either end of a long document).
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head, t := clipBounds(s, n)
	return s[:head] + clipMark + s[t:]
}

// clipMark replaces what Clip cuts out.
const clipMark = "\n…[truncated]…\n"

// clipBounds is where Clip cuts s (longer than n): s[:head] and s[tail:]
// are kept.
func clipBounds(s string, n int) (head, tail int) {
	head, tail = n*2/3, len(s)-n/3
	for head > 0 && !UTF8Start(s[head]) {
		head--
	}
	for tail < len(s) && !UTF8Start(s[tail]) {
		tail++
	}
	return head, tail
}

func UTF8Start(b byte) bool { return b&0xC0 != 0x80 }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// clip bounds every field to its cap, as RequestState / ResponseState
// return them (no secret scan: see scrub for what leaves the host).
func (s *State) clip() {
	s.UserText, s.UserGoal, s.ResponseText = Clip(s.UserText, capText), Clip(s.UserGoal, capText), Clip(s.ResponseText, capText)
	s.HarnessText = Clip(s.HarnessText, capText)
	for i := range s.ToolResults {
		r := &s.ToolResults[i]
		r.Content, r.CallInput = Clip(r.Content, capToolResult), Clip(r.CallInput, capToolInput)
	}
	for i := range s.PriorToolCalls {
		s.PriorToolCalls[i].Input = Clip(s.PriorToolCalls[i].Input, capToolInput)
	}
	for i := range s.ResponseToolCalls {
		s.ResponseToolCalls[i].Input = Clip(s.ResponseToolCalls[i].Input, capToolInput)
	}
}
