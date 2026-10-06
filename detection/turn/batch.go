package turn

import (
	"context"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
)

// ItemUnreadReason is the not-judged detail of a batch request whose
// canonical conversation has nothing to read (no messages, or a version
// newer than this package).
const ItemUnreadReason = "not judged: the batch request has no conversation the agent can read"

// capItemID bounds a batch request's id as it leaves the host (ItemID).
const capItemID = 128

// Batch is the raw body of a batch creation call as the agent prepares each
// of its requests. Every string of the whole body is searched for values
// to remove once, by the first request prepared, and every value found is
// removed from each request's strings: a secret in one request's prompt
// leaves with none of them. A Batch is used by one goroutine at a time.
type Batch struct {
	body   []byte
	shared []secretMatch
	done   bool
}

// NewBatch is the Batch of a raw batch creation body.
func NewBatch(body []byte) *Batch { return &Batch{body: body} }

// values is every value of minSharedSecret bytes or more found in the
// body's strings, searched once. Past ctx it stops with ctx's error and the
// next call searches again.
func (b *Batch) values(ctx context.Context) ([]secretMatch, error) {
	if b.done {
		return b.shared, nil
	}
	cache := scanCacheOn.Load()
	var out []secretMatch
	for _, x := range bodyTexts(b.body) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ms, err := scanText(ctx, validUTF8(x), cache)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if len(m.secret) >= minSharedSecret {
				out = append(out, m)
			}
		}
	}
	b.shared, b.done = out, true
	return out, nil
}

// PrepareItem is PrepareCallRequest for one request of the batch, read from
// its canonical conversation: the request stage, with every value found in
// the batch's raw body (and in this conversation) removed. A conversation
// it cannot read is a NotJudgedTurn (ItemUnreadReason); one cut short by
// ctx, a NotJudgedTurn (DeadlineReason).
func (b *Batch) PrepareItem(ctx context.Context, c *conv.Conversation) PreparedTurn {
	r := newReader(ctx)
	s, ok := extractConversation(r, c)
	if r.reason != "" {
		return NotJudgedTurn(StageRequest, r.reason)
	}
	if !ok {
		return NotJudgedTurn(StageRequest, ItemUnreadReason)
	}
	pre, err := b.values(ctx)
	if err != nil {
		return NotJudgedTurn(StageRequest, DeadlineReason)
	}
	hits, err := s.scrubWith(ctx, StageRequest, conversationTexts(c, nil), pre, scanCacheOn.Load())
	if err != nil {
		return NotJudgedTurn(StageRequest, DeadlineReason)
	}
	return newTurn(StageRequest, &s, hits)
}

// ItemID is a batch request's id as it may leave the host: valid UTF-8,
// with any secret in it and any value found in the batch's body so far
// replaced, clipped to capItemID bytes.
func (b *Batch) ItemID(id string) string {
	id = validUTF8(id)
	ms := append(scanSecrets(id), b.shared...)
	return clipRedacted(id, capItemID, byLength(withEncodings(ms)))
}
