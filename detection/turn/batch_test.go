package turn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
)

// Each request of a batch is prepared from its own conversation, with every
// value found anywhere in the raw body removed; a conversation with nothing
// to read is not judged; an item id leaves clipped and without a secret.
func TestBatchPrepareItem(t *testing.T) {
	v := join("Qm7Lp2Vx", "9Rt4Kw8Zn3")
	raw := []byte(`{"requests":[{"custom_id":"a","params":{"messages":[{"role":"user","content":"ship it with ` + v + `"}]}},` +
		`{"custom_id":"b","params":{"messages":[{"role":"user","content":"my api_key = \"` + v + `\""}]}}]}`)
	b := NewBatch(raw)
	first := b.PrepareItem(context.Background(), conversation(conv.HistoryFull, msg("user", text("ship it with "+v))))
	if first.NotJudged != "" || !strings.HasPrefix(first.State.UserText, "ship it with [REDACTED:") {
		t.Errorf("first item: %+v", first)
	}
	if pt := b.PrepareItem(context.Background(), conversation(conv.HistoryFull)); pt.NotJudged != ItemUnreadReason {
		t.Errorf("empty conversation: %+v", pt)
	}
	newer := conversation(conv.HistoryFull, msg("user", text("hi")))
	newer.Version = conv.ConversationVersion + 1
	if pt := b.PrepareItem(context.Background(), newer); pt.NotJudged != ItemUnreadReason {
		t.Errorf("newer conversation: %+v", pt)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if pt := NewBatch(raw).PrepareItem(ctx, conversation(conv.HistoryFull, msg("user", text("x")))); pt.NotJudged != DeadlineReason {
		t.Errorf("past the deadline: %+v", pt)
	}

	key := join("AKIA", "Z3MFKR7QW2LXB5TN")
	for id, want := range map[string]string{
		"plain-id":               "plain-id",
		"id-" + v:                "id-[REDACTED:",
		key:                      "[REDACTED:",
		strings.Repeat("x", 300): strings.Repeat("x", 40),
	} {
		got := b.ItemID(id)
		if !strings.HasPrefix(got, want) || strings.Contains(got, v) || strings.Contains(got, key) || len(got) > capItemID+len(clipMark) {
			t.Errorf("ItemID(%.20q) = %q", id, got)
		}
	}
	if out, _ := json.Marshal(first); strings.Contains(string(out), v) {
		t.Errorf("value left: %s", out)
	}
}
