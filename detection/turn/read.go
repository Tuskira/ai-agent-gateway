package turn

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
)

// Bounds of reading the content blocks of a body or a conversation. A
// tool_result inside a tool_result is one level; no agent nests them more
// than once or twice, and every level is a pass over what it holds, so the
// depth is capped. The block count bounds the work of every loop over them.
const (
	maxBlockDepth = 8
	maxBlocks     = 1 << 16
)

// The not-judged details of a body past a bound of reading it.
const (
	nestingReason = "not judged: content blocks nested deeper than 8 levels"
	blocksReason  = "not judged: more than 65536 content blocks"
)

// errNesting stops decoding content nested deeper than maxBlockDepth.
var errNesting = errors.New("turn: content nested too deep")

// reader reads the content of one call within its bounds: ctx (checked in
// every loop), maxBlockDepth levels of nested content and maxBlocks blocks
// in all. The first bound it hits is its reason; from then on it reads
// nothing, and the stage is sent as not judged with that reason.
type reader struct {
	ctx    context.Context
	blocks int
	reason string
}

func newReader(ctx context.Context) *reader { return &reader{ctx: ctx} }

// ok reports whether reading goes on, counting n more blocks read.
func (r *reader) ok(n int) bool {
	switch {
	case r.reason != "":
	case r.ctx.Err() != nil:
		r.reason = DeadlineReason
	case r.blocks+n > maxBlocks:
		r.reason = blocksReason
	default:
		r.blocks += n
	}
	return r.reason == ""
}

// stop records reason when no bound was hit before.
func (r *reader) stop(reason string) {
	if r.reason == "" {
		r.reason = reason
	}
}

// content is a raw block's content decoded once, nested content included:
// a bare string is one text block. Decoding stops with errNesting past
// maxBlockDepth levels (each level is an array and an object deep).
type content []block

func (c *content) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch dec.PeekKind() {
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		*c = content{{Type: "text", Text: tok.String()}}
		return nil
	case '[':
		if dec.StackDepth() > 2*maxBlockDepth {
			return errNesting
		}
		var bs []block
		err := jsonv2.UnmarshalDecode(dec, &bs)
		*c = bs
		return err
	}
	return dec.SkipValue() // null, or not content: no blocks
}

// content decodes raw (a message's content) and counts its blocks, nested
// ones included. Content that does not decode has no blocks (or those
// before the error), as a reader that cannot read it would see it.
func (r *reader) content(raw json.RawMessage) []block {
	if len(raw) == 0 || !r.ok(0) {
		return nil
	}
	var c content
	if err := jsonv2.Unmarshal(raw, &c); errors.Is(err, errNesting) {
		r.stop(nestingReason)
		return nil
	}
	if !r.ok(countBlocks(c)) {
		return nil
	}
	return c
}

func countBlocks(bs []block) int {
	n := len(bs)
	for _, b := range bs {
		n += countBlocks(b.Content)
	}
	return n
}

// textOf is the text of content, one part per line: text blocks, and the
// content of a nested tool_result in place. Each part is collected once
// and joined once, however deep it sits.
func (r *reader) textOf(bs []block) string {
	var parts []string
	r.flatten(bs, &parts)
	return strings.Join(parts, "\n")
}

func (r *reader) flatten(bs []block, parts *[]string) {
	for _, b := range bs {
		if !r.ok(0) {
			return
		}
		switch {
		case b.Type == "text" && b.Text != "":
			*parts = append(*parts, b.Text)
		case b.Type == "tool_result":
			r.flatten(b.Content, parts)
		}
	}
}

// checkBlocks counts the canonical blocks of bs, nested ones included, and
// stops past maxBlockDepth levels below level, as content does for a raw
// body.
func (r *reader) checkBlocks(bs []conv.ContentBlock, level int) bool {
	if level > maxBlockDepth {
		r.stop(nestingReason)
		return false
	}
	if !r.ok(len(bs)) {
		return false
	}
	for _, b := range bs {
		if len(b.Content) > 0 && !r.checkBlocks(b.Content, level+1) {
			return false
		}
	}
	return true
}

// contentText is the text of canonical blocks, one part per line: text
// blocks, the text of a text document, an opaque block's wire form, and
// the content of a nested tool_result in place, each collected once.
func (r *reader) contentText(bs []conv.ContentBlock) string {
	var parts []string
	r.flattenCanonical(bs, &parts)
	return strings.Join(parts, "\n")
}

func (r *reader) flattenCanonical(bs []conv.ContentBlock, parts *[]string) {
	for _, b := range bs {
		if !r.ok(0) {
			return
		}
		if b.Type == conv.ContentToolResult {
			r.flattenCanonical(b.Content, parts)
			continue
		}
		if t := blockText(b); t != "" {
			*parts = append(*parts, t)
		}
	}
}
