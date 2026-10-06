// Package llmtest is the conformance suite a pkg/llm adapter must pass.
//
// RunProvider drives a Provider against a REAL vendor endpoint -- plain
// text, streamed text, a tool-use round trip (streamed and not), and thinking
// when the target model reasons -- and checks the neutral results and the
// stream event order (ValidateStream). Adapters run it from an env-gated
// test that skips when no endpoint is configured; this repository does not
// fake vendors.
//
// RunDialect checks a Dialect against recorded wire samples of its format
// (testdata/<dialect name>/): request parsing, response, error and stream
// rendering.
//
// RunReader checks a Reader against goldens of its format
// (testdata/readers/<reader name>/): request, response and stream reading,
// including the strict rejections every Reader must make.
package llmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// Option adjusts RunProvider.
type Option func(*config)

type config struct {
	thinking bool
	client   *http.Client
	timeout  time.Duration
}

// WithThinking declares that the target model reasons, so the thinking
// subtest expects thinking blocks (it is skipped otherwise).
func WithThinking() Option { return func(c *config) { c.thinking = true } }

// WithClient sets the HTTP client (default: redirects not followed).
func WithClient(hc *http.Client) Option { return func(c *config) { c.client = hc } }

// WithTimeout bounds each vendor call (default 3 minutes).
func WithTimeout(d time.Duration) Option { return func(c *config) { c.timeout = d } }

// RunProvider runs the provider conformance suite against target, a real
// endpoint. A subtest the model cannot exercise (a small model that never
// calls the tool) is skipped with the reason, not failed.
func RunProvider(t *testing.T, p llm.Provider, target llm.Target, opts ...Option) {
	cfg := config{
		client:  &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		timeout: 3 * time.Minute,
	}
	for _, o := range opts {
		o(&cfg)
	}
	r := runner{p: p, target: target, cfg: cfg}
	// A reasoning model spends tokens thinking before it answers.
	budget := int64(128)
	if cfg.thinking {
		budget = 2048
	}
	textRequest := func(stream bool) *llm.Request { return textRequest(stream, budget) }
	toolRequest := func(stream bool) *llm.Request { return toolRequest(stream, 2*budget) }

	t.Run("text", func(t *testing.T) {
		resp := r.call(t, textRequest(false))
		if resp.ID == "" {
			t.Error("response has no id")
		}
		if strings.TrimSpace(text(resp.Content)) == "" {
			t.Errorf("no text in %+v", resp.Content)
		}
		switch resp.StopReason {
		case "end_turn", "max_tokens", "stop_sequence":
		default:
			t.Errorf("stop_reason = %q", resp.StopReason)
		}
		checkUsage(t, resp.Usage)
	})

	t.Run("stream_text", func(t *testing.T) {
		evs := r.stream(t, textRequest(true))
		if err := ValidateStream(evs); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, ev := range evs {
			if ev.Type == llm.EventContentBlockDelta && ev.Delta.Type == llm.DeltaText {
				b.WriteString(ev.Delta.Text)
			}
		}
		if strings.TrimSpace(b.String()) == "" {
			t.Error("stream carried no text")
		}
		checkUsage(t, finalUsage(evs))
	})

	t.Run("tool_round_trip", func(t *testing.T) {
		req := toolRequest(false)
		first := r.call(t, req)
		call := toolUse(first.Content)
		if call == nil {
			t.Skipf("model %s did not call the tool (stop_reason %q, content %q)", target.Model, first.StopReason, text(first.Content))
		}
		checkToolCall(t, call)
		if first.StopReason != "tool_use" {
			t.Errorf("stop_reason = %q with a tool call, want tool_use", first.StopReason)
		}
		req.Messages = append(req.Messages,
			llm.Message{Role: "assistant", Content: first.Content},
			llm.Message{Role: "user", Content: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: call.ID,
				Content: []llm.Block{{Type: llm.BlockText, Text: "18 degrees Celsius and sunny"}}}}},
		)
		req.ToolChoice = nil
		final := r.call(t, req)
		if strings.TrimSpace(text(final.Content)) == "" && toolUse(final.Content) == nil {
			t.Errorf("no answer after the tool result: %+v", final.Content)
		}
		checkUsage(t, final.Usage)
	})

	t.Run("stream_tool_use", func(t *testing.T) {
		evs := r.stream(t, toolRequest(true))
		if err := ValidateStream(evs); err != nil {
			t.Fatal(err)
		}
		var call *llm.Block
		var args strings.Builder
		for _, ev := range evs {
			switch {
			case ev.Type == llm.EventContentBlockStart && ev.Block.Type == llm.BlockToolUse && call == nil:
				b := *ev.Block
				call = &b
			case ev.Type == llm.EventContentBlockDelta && ev.Delta.Type == llm.DeltaInputJSON && call != nil:
				args.WriteString(ev.Delta.PartialJSON)
			}
		}
		if call == nil {
			t.Skipf("model %s did not call the tool in a stream", target.Model)
		}
		call.Input = json.RawMessage(args.String())
		if args.Len() == 0 {
			call.Input = json.RawMessage(`{}`)
		}
		checkToolCall(t, call)
	})

	t.Run("thinking", func(t *testing.T) {
		if !cfg.thinking || !p.Capabilities().Thinking {
			t.Skip("target not declared as a reasoning model (WithThinking)")
		}
		req := textRequest(false)
		req.Thinking = &llm.Thinking{Type: "enabled", BudgetTokens: 1024}
		req.MaxTokens = ptr(2 * budget)
		resp := r.call(t, req)
		found := false
		for _, b := range resp.Content {
			found = found || (b.Type == llm.BlockThinking && strings.TrimSpace(b.Thinking) != "")
		}
		if !found {
			t.Errorf("no thinking block in %+v", resp.Content)
		}
	})
}

type runner struct {
	p      llm.Provider
	target llm.Target
	cfg    config
}

func (r runner) send(t *testing.T, req *llm.Request) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.timeout)
	t.Cleanup(cancel)
	up, err := r.p.BuildRequest(ctx, req, r.target)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	resp, err := r.cfg.client.Do(up)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, llm.MaxBodyBytes))
		e := r.p.ParseError(resp.StatusCode, body)
		t.Fatalf("upstream %d: %s (%s)", resp.StatusCode, e.Message, e.Type)
	}
	return resp
}

func (r runner) call(t *testing.T, req *llm.Request) *llm.Response {
	t.Helper()
	resp, err := r.p.ParseResponse(r.send(t, req))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	return resp
}

func (r runner) stream(t *testing.T, req *llm.Request) []llm.Event {
	t.Helper()
	dec := r.p.NewStreamDecoder(r.send(t, req).Body)
	var evs []llm.Event
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			return evs
		}
		if err != nil {
			t.Fatalf("stream after %d events: %v", len(evs), err)
		}
		evs = append(evs, ev)
	}
}

// ValidateStream checks the event-order invariants every StreamDecoder must
// keep: message_start first; content blocks strictly one at a time, indexed
// 0, 1, 2, ...; every delta inside its open block; every start stopped; then
// exactly one message_delta and message_stop last; no error event. ping may
// appear anywhere after message_start.
func ValidateStream(evs []llm.Event) error {
	if len(evs) == 0 || evs[0].Type != llm.EventMessageStart || evs[0].Message == nil {
		return errors.New("stream does not begin with message_start")
	}
	open, next, deltas := -1, 0, 0
	for i, ev := range evs[1:] {
		pos := i + 1
		fail := func(format string, a ...any) error {
			return fmt.Errorf("event %d (%s): %s", pos, ev.Type, fmt.Sprintf(format, a...))
		}
		if deltas > 0 && ev.Type != llm.EventMessageStop && ev.Type != llm.EventPing {
			return fail("after message_delta")
		}
		switch ev.Type {
		case llm.EventPing:
		case llm.EventMessageStart:
			return fail("second message_start")
		case llm.EventContentBlockStart:
			switch {
			case open >= 0:
				return fail("block %d starts while block %d is open", ev.Index, open)
			case ev.Index != next:
				return fail("index %d, want %d", ev.Index, next)
			case ev.Block == nil:
				return fail("no block")
			}
			open = ev.Index
			next++
		case llm.EventContentBlockDelta:
			if ev.Index != open || open < 0 {
				return fail("delta for block %d, open block %d", ev.Index, open)
			}
			if ev.Delta == nil {
				return fail("no delta")
			}
		case llm.EventContentBlockStop:
			if ev.Index != open || open < 0 {
				return fail("stop for block %d, open block %d", ev.Index, open)
			}
			open = -1
		case llm.EventMessageDelta:
			if open >= 0 {
				return fail("block %d still open", open)
			}
			if ev.StopReason == "" {
				return fail("no stop_reason")
			}
			deltas++
		case llm.EventMessageStop:
			if deltas != 1 {
				return fail("message_stop without one message_delta")
			}
			if pos != len(evs)-1 {
				return fail("events after message_stop")
			}
		case llm.EventError:
			return fail("error event: %+v", ev.Error)
		default:
			return fail("unknown event type")
		}
	}
	if evs[len(evs)-1].Type != llm.EventMessageStop {
		return errors.New("stream does not end with message_stop")
	}
	return nil
}

func textRequest(stream bool, maxTokens int64) *llm.Request {
	return &llm.Request{
		Model:     "llmtest",
		System:    []llm.Block{{Type: llm.BlockText, Text: "You are a terse assistant."}},
		Messages:  []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.BlockText, Text: "Reply with one short sentence about the sea."}}}},
		MaxTokens: ptr(maxTokens),
		Stream:    stream,
	}
}

func toolRequest(stream bool, maxTokens int64) *llm.Request {
	return &llm.Request{
		Model:  "llmtest",
		System: []llm.Block{{Type: llm.BlockText, Text: "Answer weather questions only by calling the get_weather tool."}},
		Messages: []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.BlockText,
			Text: "What is the weather in Paris right now? Use the get_weather tool."}}}},
		Tools: []llm.Tool{{
			Name:        "get_weather",
			Description: "Get the current weather for a city.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"City name"}},"required":["city"]}`),
		}},
		ToolChoice: &llm.ToolChoice{Type: "auto"},
		MaxTokens:  ptr(maxTokens),
		Stream:     stream,
	}
}

func checkToolCall(t *testing.T, b *llm.Block) {
	t.Helper()
	if b.ID == "" || b.Name != "get_weather" {
		t.Errorf("tool_use id=%q name=%q", b.ID, b.Name)
	}
	var in map[string]any
	if err := json.Unmarshal(b.Input, &in); err != nil || in == nil {
		t.Errorf("tool_use input %s is not a JSON object: %v", b.Input, err)
	}
}

func checkUsage(t *testing.T, u llm.Usage) {
	t.Helper()
	if u.InputTokens+u.CacheReadTokens <= 0 || u.OutputTokens <= 0 {
		t.Errorf("usage = %+v, want input and output tokens", u)
	}
}

func finalUsage(evs []llm.Event) llm.Usage {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == llm.EventMessageDelta && evs[i].Usage != nil {
			u := *evs[i].Usage
			if s := evs[0].Message; s != nil {
				u.InputTokens = max(u.InputTokens, s.Usage.InputTokens)
			}
			return u
		}
	}
	return llm.Usage{}
}

func text(bs []llm.Block) string {
	var b strings.Builder
	for _, x := range bs {
		if x.Type == llm.BlockText {
			b.WriteString(x.Text)
		}
	}
	return b.String()
}

func toolUse(bs []llm.Block) *llm.Block {
	for i := range bs {
		if bs[i].Type == llm.BlockToolUse {
			return &bs[i]
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
