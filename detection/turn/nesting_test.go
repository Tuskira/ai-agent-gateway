package turn

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
)

// nestedBody is a request whose new turn is one tool_result holding a
// tool_result holding ... depth levels down, each level's content a short
// text block and the next level, the innermost level size bytes of text.
func nestedBody(depth, size int) []byte {
	var level any = []map[string]any{{"type": "text", "text": prose(size)}}
	for range depth {
		level = []map[string]any{
			{"type": "text", "text": "level"},
			{"type": "tool_result", "tool_use_id": "t1", "content": level},
		}
	}
	b, _ := json.Marshal(map[string]any{"messages": []map[string]any{
		{"role": "user", "content": "read it"},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"p": "x"}}}},
		{"role": "user", "content": level},
	}})
	return b
}

// nestedConversation is nestedBody in the canonical shape.
func nestedConversation(depth, size int) *conv.Conversation {
	level := []conv.ContentBlock{text(prose(size))}
	for range depth {
		level = []conv.ContentBlock{text("level"), {Type: conv.ContentToolResult, ToolUseID: "t1", Content: level}}
	}
	return conversation(conv.HistoryFull,
		msg("user", text("read it")),
		msg("assistant", conv.ContentBlock{Type: conv.ContentToolUse, ID: "t1", Name: "Read", Input: json.RawMessage(`{"p":"x"}`)}),
		msg("user", level...))
}

// A request nested thousands of levels deep (a 2.6 MB body) ties up no CPU
// past the prepare deadline: extraction reads the levels past the cap as
// one JSON text, once, on the raw body and on the canonical conversation.
// It is judged, or, when the scan of its strings runs out of time, not
// judged with the deadline's reason: never for its depth.
func TestDeeplyNestedBodyGivesUpWithinDeadline(t *testing.T) {
	secretDetector()
	for _, depth := range []int{10, 3000} {
		for name, prep := range map[string]func(context.Context) PreparedTurn{
			"raw": func(ctx context.Context) PreparedTurn { return PrepareRequestContext(ctx, nestedBody(depth, 2600<<10)) },
			"canonical": func(ctx context.Context) PreparedTurn {
				return PrepareCallRequest(ctx, Call{Request: userBody("read it"), Conversation: nestedConversation(depth, 2600<<10)})
			},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			start := time.Now()
			pt := prep(ctx)
			d := time.Since(start)
			cancel()
			if d > time.Second+slowdown*500*time.Millisecond {
				t.Errorf("%s, depth %d: prepared in %v, past the 1s deadline", name, depth, d)
			}
			switch {
			case pt.NotJudged == "":
				if !strings.Contains(pt.State.HarnessText, deepNote) || len(pt.State.ToolResults) != 1 {
					t.Errorf("%s, depth %d: judged without the depth note (%d tool results)", name, depth, len(pt.State.ToolResults))
				}
			case pt.NotJudged != DeadlineReason:
				t.Errorf("%s, depth %d: not judged %q; want judged, or the deadline", name, depth, pt.NotJudged)
			}
		}
	}
}

// prepareBoth is a request of the new turn's content blocks prepared from
// the raw body and from the canonical conversation.
func prepareBoth(t *testing.T, body []byte, c *conv.Conversation) map[string]PreparedTurn {
	t.Helper()
	return map[string]PreparedTurn{
		"raw":       prepareRequest(body),
		"canonical": PrepareCallRequest(context.Background(), Call{Request: body, Conversation: c}),
	}
}

// Nesting up to the cap is read whole, each level's text in order, the
// same way from the raw body and from the canonical conversation (one
// definition of the depth, tooDeep). One level past it, the call is still
// judged: the levels up to the cap are read, and the rest is harness_text,
// as JSON under the depth note.
func TestNestedToolResultsFlattened(t *testing.T) {
	want := strings.TrimSuffix(strings.Repeat("level\n", maxBlockDepth-1), "\n")
	for name, pt := range prepareBoth(t, nestedBody(maxBlockDepth, 0), nestedConversation(maxBlockDepth, 0)) {
		if pt.NotJudged != "" || len(pt.State.ToolResults) != 1 {
			t.Fatalf("%s: not judged %q, %d tool results", name, pt.NotJudged, len(pt.State.ToolResults))
		}
		if got := pt.State.ToolResults[0].Content; got != want {
			t.Errorf("%s: content = %q", name, got)
		}
		if strings.Contains(pt.State.HarnessText, deepNote) {
			t.Errorf("%s, at the cap: harness_text has the depth note", name)
		}
	}
	for name, pt := range prepareBoth(t, nestedBody(maxBlockDepth+1, 0), nestedConversation(maxBlockDepth+1, 0)) {
		if pt.NotJudged != "" || len(pt.State.ToolResults) != 1 {
			t.Fatalf("%s, one level past the cap: not judged %q, %d tool results", name, pt.NotJudged, len(pt.State.ToolResults))
		}
		if got := pt.State.ToolResults[0].Content; got != want+"\nlevel" {
			t.Errorf("%s, one level past the cap: content = %q", name, got)
		}
		if h := pt.State.HarnessText; !strings.HasPrefix(h, deepNote+"\n[") {
			t.Errorf("%s, one level past the cap: harness_text = %q", name, h)
		}
	}
}

// Depth 20: the levels past 8 are one JSON text in harness_text, the
// innermost text in it, and none of it in the tool result.
func TestDepthPastCapIsHarnessText(t *testing.T) {
	inner := "innermost words"
	body := bytes.Replace(nestedBody(20, 0), []byte(`"text":""`), []byte(`"text":"`+inner+`"`), 1)
	c := nestedConversation(20, 0)
	deepest := c.Messages[2].Content
	for len(deepest) == 2 {
		deepest = deepest[1].Content
	}
	deepest[0].Text = inner
	for name, pt := range prepareBoth(t, body, c) {
		if pt.NotJudged != "" || len(pt.State.ToolResults) != 1 {
			t.Fatalf("%s: not judged %q", name, pt.NotJudged)
		}
		if got := strings.Count(pt.State.ToolResults[0].Content, "level"); got != maxBlockDepth {
			t.Errorf("%s: %d levels read as blocks, want %d", name, got, maxBlockDepth)
		}
		h := pt.State.HarnessText
		if !strings.HasPrefix(h, deepNote) || !strings.Contains(h, inner) || strings.Contains(pt.State.ToolResults[0].Content, inner) {
			t.Errorf("%s: harness_text %.200q", name, h)
		}
	}
}

// manyBlocks is a new turn of n text blocks, "x" each but the last, last.
func manyBlocks(n int, last string) ([]byte, *conv.Conversation) {
	bs := make([]map[string]any, n)
	blocks := make([]conv.ContentBlock, n)
	for i := range bs {
		t := "x"
		if i == n-1 {
			t = last
		}
		bs[i] = map[string]any{"type": "text", "text": t}
		blocks[i] = text(t)
	}
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": bs}}})
	return body, conversation(conv.HistoryFull, msg("user", blocks...))
}

// A body of 65,537 content blocks is judged on the first 65,536, and
// harness_text says one block was left out.
func TestTooManyBlocksJudgedOnWhatFits(t *testing.T) {
	body, c := manyBlocks(maxBlocks+1, "the last block")
	r := newReader(context.Background())
	s, ok := extractRequest(r, body)
	if !ok || r.reason != "" || strings.Count(s.UserText, "x") != maxBlocks || strings.Contains(s.UserText, "last") {
		t.Errorf("raw: read %d blocks (ok %v, reason %q)", strings.Count(s.UserText, "x"), ok, r.reason)
	}
	r = newReader(context.Background())
	s, ok = extractConversation(r, c)
	if !ok || r.reason != "" || strings.Count(s.UserText, "x") != maxBlocks || strings.Contains(s.UserText, "last") {
		t.Errorf("canonical: read %d blocks (ok %v, reason %q)", strings.Count(s.UserText, "x"), ok, r.reason)
	}
	if len(c.Messages[0].Content) != maxBlocks+1 {
		t.Error("canonical: the conversation was changed")
	}
	for name, pt := range prepareBoth(t, body, c) {
		if pt.NotJudged != "" || pt.State.HarnessText != blocksNote(1) {
			t.Errorf("%s: not judged %q, harness_text %q", name, pt.NotJudged, pt.State.HarnessText)
		}
	}
}

// Past the cap, the oldest history is what is left out, not the new turn:
// it is read first.
func TestTooManyBlocksKeepsNewTurn(t *testing.T) {
	bs := make([]map[string]any, maxBlocks)
	for i := range bs {
		bs[i] = map[string]any{"type": "text", "text": "old"}
	}
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{
		{"role": "user", "content": bs},
		{"role": "assistant", "content": "ok"},
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "the new turn"}}},
	}})
	pt := prepareRequest(body)
	// 65,536 old blocks, "ok" and the new turn's block: the 2 oldest are out.
	if pt.NotJudged != "" || pt.State.UserText != "the new turn" || pt.State.HarnessText != blocksNote(2) {
		t.Errorf("not judged %q, user_text %q, harness_text %q", pt.NotJudged, pt.State.UserText, pt.State.HarnessText)
	}
}

// What is left out is still searched for secrets: a value whose rule needs
// context found only in block 70,000, or only at depth 12, is removed from
// every string, where it sits bare too.
func TestSecretPastCapsRemoved(t *testing.T) {
	v := join("wJalrXUtnFEMI/K7MDENG", "/bPxRfiCYzq8Tq3Rk2Z")
	ctxText := "aws_secret_access_key = " + v
	n := 70000
	bs := make([]map[string]any, n)
	blocks := make([]conv.ContentBlock, n)
	for i := range bs {
		t := "x"
		switch i {
		case 0:
			t = "the value is " + v
		case n - 1:
			t = ctxText
		}
		bs[i] = map[string]any{"type": "text", "text": t}
		blocks[i] = text(t)
	}
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": bs}}})
	for name, pt := range prepareBoth(t, body, conversation(conv.HistoryFull, msg("user", blocks...))) {
		if pt.NotJudged != "" || !strings.Contains(pt.State.UserText, "[REDACTED:generic-api-key]") {
			t.Errorf("block %d, %s: not judged %q, user_text %.80q", n, name, pt.NotJudged, pt.State.UserText)
		}
		if l := leak(t, pt, v); l != "" {
			t.Errorf("block %d, %s: leaked %s", n, name, l)
		}
	}

	var level any = []map[string]any{{"type": "text", "text": ctxText}}
	for range 12 {
		level = []map[string]any{{"type": "text", "text": "level"}, {"type": "tool_result", "tool_use_id": "t1", "content": level}}
	}
	deep, _ := json.Marshal(map[string]any{"messages": []map[string]any{
		{"role": "user", "content": "read it"},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"p": "x"}}}},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "the value is " + v}, level.([]map[string]any)[0], level.([]map[string]any)[1]}},
	}})
	c := nestedConversation(12, 0)
	deepest := c.Messages[2].Content
	for len(deepest) == 2 {
		deepest = deepest[1].Content
	}
	deepest[0].Text = ctxText
	c.Messages[2].Content = append([]conv.ContentBlock{text("the value is " + v)}, c.Messages[2].Content...)
	for name, pt := range prepareBoth(t, deep, c) {
		if pt.NotJudged != "" || !strings.Contains(pt.State.HarnessText, deepNote) {
			t.Errorf("depth 12, %s: not judged %q, harness_text %.80q", name, pt.NotJudged, pt.State.HarnessText)
		}
		if l := leak(t, pt, v); l != "" {
			t.Errorf("depth 12, %s: leaked %s", name, l)
		}
	}
}

// The depth is counted the same wherever content sits: a streamed
// tool_use start carrying nested content (which no reader reads) is not
// dropped. Before, its content was decoded with the depth counted from
// the event, so 8 levels there failed the event and hid the call.
func TestStreamedToolUseWithNestedContent(t *testing.T) {
	for _, d := range []int{maxBlockDepth - 1, maxBlockDepth, 20} {
		var level any = []map[string]any{{"type": "text", "text": "x"}}
		for range d {
			level = []map[string]any{{"type": "tool_result", "content": level}}
		}
		start, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{}, "content": level}})
		delta := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`
		body := "event: content_block_start\ndata: " + string(start) + "\n\nevent: content_block_delta\ndata: " + delta + "\n\n"
		pt, ok := prepareResponse(userBody("hi"), []byte(body))
		if !ok || len(pt.State.ResponseToolCalls) != 1 || pt.State.ResponseToolCalls[0].Name != "Bash" {
			t.Errorf("depth %d: ok %v, calls %+v", d, ok, pt.State.ResponseToolCalls)
		}
	}
}
