package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// AssembleStream implements llm.StreamAssembler: it folds an Anthropic
// Messages SSE stream, as the client received it, into the body the same call
// returns without streaming:
//
//	{"id","type":"message","role","model","content":[...],"stop_reason",...,"usage":{...}}
//
// The fold works on the JSON of the events, not on the neutral llm.Response,
// which has no slot for fields the gateway does not know (caller, container,
// service_tier, a usage detail, ...) and would drop them. The message starts
// as message_start's message and each block as its content_block_start block,
// so every field they carry survives; deltas then fill in the blocks, and
// message_delta's delta, usage (over the start usage) and any other
// message-level field are merged in. What only a delta carries and no message
// has (a delta's estimated_tokens) is not added. Key order is the stream's:
// message_start's order for the message, content_block_start's for a block,
// with a key a later event adds placed after the existing ones in the order
// they arrive; the output is compact JSON and the same stream always gives
// the same bytes.
//
// ok is false, and the caller keeps the stream, when body is not an SSE
// stream, lacks message_start or message_stop (a cut stream), carries an
// error event, has a frame that does not decode (or a key twice), has block
// indexes out of order or a delta for a block that is not open, or has a
// delta type that is not known here (folding it would silently lose it).
func (Dialect) AssembleStream(body []byte) ([]byte, bool) { return AssembleStream(body) }

// AssembleStream is Dialect.AssembleStream.
func AssembleStream(body []byte) (assembled []byte, ok bool) {
	var f folder
	rd := sse.NewReader(bytes.NewReader(body), llm.MaxFrameBytes, llm.ErrFrameTooLarge)
	for !f.done {
		_, data, err := rd.Next()
		if err != nil {
			return nil, false // io.EOF here is a stream with no message_stop
		}
		if strictjson.Check([]byte(data)) != nil || !f.event([]byte(data)) {
			return nil, false
		}
	}
	return f.result()
}

// object is a JSON object that keeps its keys in order and its values as the
// raw JSON they arrived as.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

// parseObject reads raw, which must be a JSON object.
func parseObject(raw []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	o := newObject()
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(key, v)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	return o, nil
}

// set replaces key's value where it is, or adds key after the others.
func (o *object) set(key string, v json.RawMessage) {
	if _, has := o.vals[key]; !has {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// str is key's value when it is a JSON string, else "".
func (o *object) str(key string) string {
	var s string
	_ = json.Unmarshal(o.vals[key], &s)
	return s
}

func (o *object) setString(key, s string) { o.set(key, jsonString(s)) }

// bytes renders o as JSON. It does not go through json.Marshal, which would
// rewrite <, > and & in the values it was handed.
func (o *object) bytes() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonString is s as a JSON string, as is (no HTML escaping).
func jsonString(s string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return bytes.TrimRight(b.Bytes(), "\n")
}

// block is one content block being filled in.
type block struct {
	obj    *object
	closed bool
	// text, thinking, signature and args collect the deltas of their field;
	// the field is set from them when the block stops. A used flag tells a
	// field no delta touched from one that is empty.
	text, thinking, signature, args strings.Builder
	used                            struct{ text, thinking, signature, args bool }
	citations                       []json.RawMessage
}

// toolUse reports a block that carries a tool call's input (tool_use,
// server_tool_use, mcp_tool_use).
func (b *block) toolUse() bool { return strings.HasSuffix(b.obj.str("type"), "tool_use") }

// folder folds the events of one stream.
type folder struct {
	msg    *object
	blocks []*block
	done   bool // message_stop seen
}

// event folds one event's JSON; false when the stream cannot be assembled.
func (f *folder) event(data []byte) bool {
	ev, err := parseObject(data)
	if err != nil {
		return false
	}
	typ := llm.EventType(ev.str("type"))
	if typ != llm.EventMessageStart && typ != llm.EventPing && typ != llm.EventError && f.msg == nil {
		return false // an event before message_start
	}
	switch typ {
	case llm.EventMessageStart:
		if f.msg != nil {
			return false
		}
		m, err := parseObject(ev.vals["message"])
		f.msg = m
		return err == nil
	case llm.EventContentBlockStart:
		idx, ok := index(ev)
		if !ok || idx != len(f.blocks) {
			return false
		}
		o, err := parseObject(ev.vals["content_block"])
		if err != nil {
			return false
		}
		f.blocks = append(f.blocks, &block{obj: o})
	case llm.EventContentBlockDelta:
		return f.delta(ev)
	case llm.EventContentBlockStop:
		idx, ok := index(ev)
		if !ok || idx >= len(f.blocks) || f.blocks[idx].closed {
			return false
		}
		f.blocks[idx].finish()
	case llm.EventMessageDelta:
		return f.messageDelta(ev)
	case llm.EventMessageStop:
		f.done = true
	case llm.EventError:
		return false
	}
	return true // ping, or an event type this does not know: not part of the message
}

func index(ev *object) (int, bool) {
	var i int
	err := json.Unmarshal(ev.vals["index"], &i)
	return i, err == nil && i >= 0
}

// delta applies one content_block_delta.
func (f *folder) delta(ev *object) bool {
	idx, ok := index(ev)
	if !ok || idx >= len(f.blocks) || f.blocks[idx].closed {
		return false
	}
	b := f.blocks[idx]
	d, err := parseObject(ev.vals["delta"])
	if err != nil {
		return false
	}
	switch llm.DeltaType(d.str("type")) {
	case llm.DeltaText:
		b.text.WriteString(d.str("text"))
		b.used.text = true
	case llm.DeltaThinking:
		b.thinking.WriteString(d.str("thinking"))
		b.used.thinking = true
	case llm.DeltaSignature:
		b.signature.WriteString(d.str("signature"))
		b.used.signature = true
	case llm.DeltaInputJSON:
		if !b.toolUse() {
			return false
		}
		b.args.WriteString(d.str("partial_json"))
		b.used.args = true
	case "citations_delta":
		if len(d.vals["citation"]) == 0 {
			return false
		}
		b.citations = append(b.citations, d.vals["citation"])
	default:
		return false
	}
	return true
}

// finish sets the fields the deltas filled in. A field's value in
// content_block_start (usually empty) is the start of what the deltas add to.
func (b *block) finish() {
	b.closed = true
	if b.used.text {
		b.obj.setString("text", b.obj.str("text")+b.text.String())
	}
	if b.used.thinking {
		b.obj.setString("thinking", b.obj.str("thinking")+b.thinking.String())
	}
	if b.used.signature {
		b.obj.setString("signature", b.obj.str("signature")+b.signature.String())
	}
	if len(b.citations) > 0 {
		var all []json.RawMessage
		_ = json.Unmarshal(b.obj.vals["citations"], &all) // null or absent: none yet
		all = append(all, b.citations...)
		b.obj.set("citations", joinArray(all))
	}
	if b.toolUse() {
		switch args := b.args.String(); {
		case b.used.args && json.Valid([]byte(args)):
			b.obj.set("input", json.RawMessage(args))
		case b.used.args && strings.TrimSpace(args) != "":
			b.obj.setString("input", args) // cut or malformed arguments: keep what came
		case b.used.args || len(b.obj.vals["input"]) == 0:
			b.obj.set("input", json.RawMessage("{}"))
		}
	}
}

func joinArray(items []json.RawMessage) json.RawMessage {
	b := []byte{'['}
	for i, it := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, it...)
	}
	return append(b, ']')
}

// messageDelta merges message_delta into the message: its delta's fields over
// the message's, its usage over the start usage, and any other field it
// carries (context_management) as a message field.
func (f *folder) messageDelta(ev *object) bool {
	for _, k := range ev.keys {
		switch k {
		case "type":
		case "delta":
			d, err := parseObject(ev.vals[k])
			if err != nil {
				return false
			}
			for _, dk := range d.keys {
				f.msg.set(dk, d.vals[dk])
			}
		case "usage":
			u, err := parseObject(ev.vals[k])
			if err != nil {
				return false
			}
			// Cumulative counts: the delta's win. A null does not blank a
			// count the start gave.
			base, err := parseObject(f.msg.vals["usage"])
			if err != nil {
				base = newObject() // none, or null, in message_start
			}
			for _, uk := range u.keys {
				if _, had := base.vals[uk]; had && string(u.vals[uk]) == "null" {
					continue
				}
				base.set(uk, u.vals[uk])
			}
			f.msg.set("usage", base.bytes())
		default:
			f.msg.set(k, ev.vals[k])
		}
	}
	return true
}

// result renders the message once the stream has ended.
func (f *folder) result() ([]byte, bool) {
	if f.msg == nil || !f.done {
		return nil, false
	}
	content := make([]json.RawMessage, len(f.blocks))
	for i, b := range f.blocks {
		if !b.closed {
			return nil, false // message_stop with a block still open
		}
		content[i] = b.obj.bytes()
	}
	f.msg.set("content", joinArray(content))
	var out bytes.Buffer
	if json.Compact(&out, f.msg.bytes()) != nil {
		return nil, false
	}
	return out.Bytes(), true
}
