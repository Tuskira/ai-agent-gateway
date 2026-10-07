package turn

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
)

// Bounds of reading the content blocks of a body or a conversation. A
// tool_result inside a tool_result is one level; no agent nests them more
// than once or twice, and every level is a pass over what it holds, so the
// depth is capped. The block count bounds the work of every loop over them.
//
// Past a bound the call is still judged on what fits (only an unreadable
// body or the deadline leave a stage not judged): content nested deeper
// than maxBlockDepth is not read as blocks but kept whole as its JSON
// text, and the blocks past maxBlocks are left out. Either is said in the
// stage's text (notes) so the engine records it. What is left out is still
// searched for secrets: every string of the raw body is (bodyTexts), so a
// value found there is removed from everything that is sent.
const (
	maxBlockDepth = 8
	maxBlocks     = 1 << 16
)

// tooDeep reports whether content at level is past maxBlockDepth. A
// message's (or an answer's) own content is level 0, the content of a
// block in it level 1, and so on, on the raw body and on the canonical
// conversation alike: the one definition of the depth.
func tooDeep(level int) bool { return level > maxBlockDepth }

// deepType is the type of the block that stands for content nested past
// maxBlockDepth: Text is that content's JSON. It holds a byte no decoded
// JSON string can (invalid UTF-8 is replaced when decoding), so no block a
// sender writes has it.
const deepType = "\xffdeep"

// deepNote heads the JSON of content nested past maxBlockDepth in the
// stage's text.
const deepNote = "[content nested deeper than 8 levels, as JSON:]"

// blocksNote is the note of blocks left out past maxBlocks.
func blocksNote(n int) string { return fmt.Sprintf("[content truncated: %d blocks beyond the cap]", n) }

// reader reads the content of one call within its bounds: ctx (checked in
// every loop, its reason DeadlineReason, the only one that stops it),
// maxBlockDepth levels of nested content and maxBlocks blocks in all.
type reader struct {
	ctx     context.Context
	blocks  int      // blocks kept so far
	dropped int      // blocks left out past maxBlocks, not yet noted
	deep    []string // JSON of content nested too deep, flattened, not yet noted
	reason  string
}

func newReader(ctx context.Context) *reader { return &reader{ctx: ctx} }

// ok reports whether reading goes on: the deadline is not past.
func (r *reader) ok() bool {
	if r.reason == "" && r.ctx.Err() != nil {
		r.reason = DeadlineReason
	}
	return r.reason == ""
}

// take counts one more block and reports whether it is kept (within
// maxBlocks); one that is not is counted as dropped.
func (r *reader) take() bool {
	if r.blocks >= maxBlocks {
		r.dropped++
		return false
	}
	r.blocks++
	return true
}

// notes is what the stage's text says of what was left out since the last
// call: the JSON of each content nested too deep that was flattened, then
// how many blocks were past the cap. Each extraction appends it to its
// stage's text, so the engine records that the content was cut.
func (r *reader) notes() []string {
	var out []string
	for _, d := range r.deep {
		out = append(out, deepNote+"\n"+d)
	}
	if r.dropped > 0 {
		out = append(out, blocksNote(r.dropped))
	}
	r.deep, r.dropped = nil, 0
	return out
}

// content decodes raw (a message's content), one pass, nested content
// included: a bare string is one text block; content nested past
// maxBlockDepth is one deepType block; the blocks past maxBlocks (in
// order, depth first) are dropped. Content that does not decode has the
// blocks before the error, as a reader that cannot read it would see it.
func (r *reader) content(raw json.RawMessage) []block {
	if len(raw) == 0 || !r.ok() {
		return nil
	}
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	bs, _ := r.decodeContent(dec, 0)
	return bs
}

// decodeContent decodes the content value at dec's next token, at level.
func (r *reader) decodeContent(dec *jsontext.Decoder, level int) ([]block, error) {
	switch dec.PeekKind() {
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		if !r.take() {
			return nil, nil
		}
		return []block{{Type: "text", Text: tok.String()}}, nil
	case '[':
		if tooDeep(level) {
			v, err := dec.ReadValue()
			if err != nil || !r.take() {
				return nil, err
			}
			return []block{{Type: deepType, Text: string(v)}}, nil
		}
	default:
		return nil, dec.SkipValue() // null, or not content: no blocks
	}
	if _, err := dec.ReadToken(); err != nil { // [
		return nil, err
	}
	var bs []block
	for dec.PeekKind() != ']' {
		if !r.ok() {
			return bs, r.ctx.Err()
		}
		b, keep, err := r.decodeBlock(dec, level)
		if err != nil {
			return bs, err
		}
		if keep {
			bs = append(bs, b)
		}
	}
	_, err := dec.ReadToken() // ]
	return bs, err
}

// decodeBlock decodes one block of content at level: exact member names, a
// repeated name an error, as the strict decoder reads a body. A value that
// is not an object is no block. A block past maxBlocks is read all the
// same, so that it and every block it holds are counted as dropped, and
// not kept.
func (r *reader) decodeBlock(dec *jsontext.Decoder, level int) (block, bool, error) {
	var b block
	if dec.PeekKind() != '{' {
		return b, false, dec.SkipValue()
	}
	keep := r.take()
	if _, err := dec.ReadToken(); err != nil { // {
		return b, false, err
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return b, keep, err
		}
		var dst *string
		switch name.String() {
		case "type":
			dst = &b.Type
		case "text":
			dst = &b.Text
		case "id":
			dst = &b.ID
		case "name":
			dst = &b.Name
		case "tool_use_id":
			dst = &b.ToolUseID
		case "input":
			v, err := dec.ReadValue()
			if err != nil {
				return b, keep, err
			}
			b.Input = json.RawMessage(v.Clone())
			continue
		case "content":
			if b.Content, err = r.decodeContent(dec, level+1); err != nil {
				return b, keep, err
			}
			continue
		}
		if dst != nil && dec.PeekKind() == '"' {
			tok, err := dec.ReadToken()
			if err != nil {
				return b, keep, err
			}
			*dst = tok.String()
			continue
		}
		if err := dec.SkipValue(); err != nil {
			return b, keep, err
		}
	}
	_, err := dec.ReadToken() // }
	return b, keep, err
}

// textOf is the text of content, one part per line: text blocks, and the
// content of a nested tool_result in place. Each part is collected once
// and joined once, however deep it sits. Content nested too deep goes to
// the reader's notes.
func (r *reader) textOf(bs []block) string {
	var parts []string
	r.flatten(bs, &parts)
	return strings.Join(parts, "\n")
}

func (r *reader) flatten(bs []block, parts *[]string) {
	for _, b := range bs {
		if !r.ok() {
			return
		}
		switch {
		case b.Type == deepType:
			r.deep = append(r.deep, b.Text)
		case b.Type == "text" && b.Text != "":
			*parts = append(*parts, b.Text)
		case b.Type == "tool_result":
			r.flatten(b.Content, parts)
		}
	}
}

// limit is a copy of bs (canonical blocks at level) within the reader's
// bounds, as content reads a raw body: content nested past maxBlockDepth
// is one deepType block holding its JSON, and the blocks past maxBlocks
// are left out (counted as dropped). bs itself is not changed.
func (r *reader) limit(bs []conv.ContentBlock, level int) []conv.ContentBlock {
	if len(bs) == 0 {
		return nil
	}
	if tooDeep(level) {
		if !r.take() {
			return nil
		}
		j, _ := json.Marshal(bs) // canonical blocks always marshal
		return []conv.ContentBlock{{Type: deepType, Text: string(j)}}
	}
	out := make([]conv.ContentBlock, 0, len(bs))
	for i, b := range bs {
		if !r.ok() {
			return out
		}
		if !r.take() { // b is counted; count what it holds and the rest
			r.dropped += countCanonical(b.Content, level+1)
			for _, rest := range bs[i+1:] {
				r.dropped += 1 + countCanonical(rest.Content, level+1)
			}
			return out
		}
		b.Content = r.limit(b.Content, level+1)
		out = append(out, b)
	}
	return out
}

// countCanonical is how many blocks bs holds at level, nested ones
// included, content past maxBlockDepth one block.
func countCanonical(bs []conv.ContentBlock, level int) int {
	if len(bs) == 0 {
		return 0
	}
	if tooDeep(level) {
		return 1
	}
	n := len(bs)
	for _, b := range bs {
		n += countCanonical(b.Content, level+1)
	}
	return n
}

// contentText is the text of canonical blocks, one part per line: text
// blocks, the text of a text document, an opaque block's wire form, and
// the content of a nested tool_result in place, each collected once.
// Content nested too deep goes to the reader's notes.
func (r *reader) contentText(bs []conv.ContentBlock) string {
	var parts []string
	r.flattenCanonical(bs, &parts)
	return strings.Join(parts, "\n")
}

func (r *reader) flattenCanonical(bs []conv.ContentBlock, parts *[]string) {
	for _, b := range bs {
		if !r.ok() {
			return
		}
		switch b.Type {
		case deepType:
			r.deep = append(r.deep, b.Text)
			continue
		case conv.ContentToolResult:
			r.flattenCanonical(b.Content, parts)
			continue
		}
		if t := blockText(b); t != "" {
			*parts = append(*parts, t)
		}
	}
}
