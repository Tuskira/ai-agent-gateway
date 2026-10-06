package turn

import (
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
// past the prepare deadline: extraction is bounded by the deadline and the
// nesting cap, on the raw body and on the canonical conversation.
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
			if pt.NotJudged == "" || len(pt.State.ToolResults) != 0 {
				t.Errorf("%s, depth %d: judged (%d tool results); want not judged", name, depth, len(pt.State.ToolResults))
			}
		}
	}
}

// Nesting up to the cap is read whole, each level's text in order, the
// same way from the raw body and from the canonical conversation.
func TestNestedToolResultsFlattened(t *testing.T) {
	want := strings.TrimSuffix(strings.Repeat("level\n", maxBlockDepth-1), "\n")
	raw := PrepareRequest(nestedBody(maxBlockDepth, 0))
	canonical := PrepareCallRequest(context.Background(), Call{Request: userBody("read it"), Conversation: nestedConversation(maxBlockDepth, 0)})
	for name, pt := range map[string]PreparedTurn{"raw": raw, "canonical": canonical} {
		if pt.NotJudged != "" || len(pt.State.ToolResults) != 1 {
			t.Fatalf("%s: not judged %q, %d tool results", name, pt.NotJudged, len(pt.State.ToolResults))
		}
		if got := pt.State.ToolResults[0].Content; got != want {
			t.Errorf("%s: content = %q", name, got)
		}
	}
	for name, pt := range map[string]PreparedTurn{
		"raw":       PrepareRequest(nestedBody(maxBlockDepth+1, 0)),
		"canonical": PrepareCallRequest(context.Background(), Call{Request: userBody("read it"), Conversation: nestedConversation(maxBlockDepth+1, 0)}),
	} {
		if pt.NotJudged != nestingReason {
			t.Errorf("%s, one level past the cap: not judged %q", name, pt.NotJudged)
		}
	}
}

// A body of more content blocks than maxBlocks is not judged: no text.
func TestTooManyBlocksNotJudged(t *testing.T) {
	bs := make([]map[string]any, maxBlocks+1)
	for i := range bs {
		bs[i] = map[string]any{"type": "text", "text": "x"}
	}
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": bs}}})
	if pt := PrepareRequest(body); pt.NotJudged != blocksReason {
		t.Errorf("raw: not judged %q", pt.NotJudged)
	}
	blocks := make([]conv.ContentBlock, maxBlocks+1)
	for i := range blocks {
		blocks[i] = text("x")
	}
	c := conversation(conv.HistoryFull, msg("user", blocks...))
	if pt := PrepareCallRequest(context.Background(), Call{Request: userBody("x"), Conversation: c}); pt.NotJudged != blocksReason {
		t.Errorf("canonical: not judged %q", pt.NotJudged)
	}
}
