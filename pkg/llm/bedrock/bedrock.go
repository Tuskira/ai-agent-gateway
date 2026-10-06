// Package bedrock holds the llm.Readers for Amazon Bedrock's own wire
// formats: "bedrock-converse" (the Converse and ConverseStream APIs) and
// "bedrock-invoke" (InvokeModel and InvokeModelWithResponseStream bodies of
// model families other than Anthropic's, whose invoke body is the Anthropic
// Messages body and is read by the "anthropic" Reader). The model id of a
// Bedrock call is in the URL path, not the body, so Request.Model is empty
// unless the caller fills it.
//
// Streams are AWS event-stream binary frames (internal/eventstream), read
// one frame at a time.
package bedrock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/eventstream"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// Registry names of the Readers.
const (
	ConverseName = "bedrock-converse"
	InvokeName   = "bedrock-invoke"
)

func init() {
	llm.RegisterReader(ConverseReader{})
	llm.RegisterReader(InvokeReader{})
}

// ErrUnsupportedFormat reports a body the Reader recognizes as Bedrock's but
// cannot read: an invoke body of a model family it does not map, or an
// Anthropic body sent to the invoke Reader. A request error wraps it inside
// a *llm.RequestError; errors.Is finds it on every path (request, response
// and stream), so a caller can report it apart from a malformed body.
var ErrUnsupportedFormat = errors.New("unsupported Bedrock body format")

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedFormat, fmt.Sprintf(format, a...))
}

// object decodes a JSON object; anything else is an error.
func object(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, err
	}
	return m, nil
}

// take moves m[key] into dst (a null or missing value leaves dst alone).
// Keys match exactly: map lookups, unlike Go's struct decoding, do not fold
// case.
func take(m map[string]json.RawMessage, key string, dst any) error {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	delete(m, key)
	if isNull(raw) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: %v", key, err)
	}
	return nil
}

// takeObject moves m[key], an object, into a map (nil when missing).
func takeObject(m map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	var raw json.RawMessage
	if err := take(m, key, &raw); err != nil || raw == nil {
		return nil, err
	}
	o, err := object(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: must be an object", key)
	}
	return o, nil
}

// member reads a union: an object holding exactly one member, returned
// with its name.
func member(raw json.RawMessage) (string, json.RawMessage, error) {
	o, err := object(raw)
	if err != nil {
		return "", nil, errors.New("must be an object")
	}
	if len(o) != 1 {
		return "", nil, fmt.Errorf("must hold exactly one member, has %d", len(o))
	}
	for k, v := range o {
		return k, v, nil
	}
	panic("unreachable")
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func nonEmpty(m map[string]json.RawMessage) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return m
}

// checkedObject is a response-side object read: size cap, strict keys, an
// object.
func checkedObject(body []byte, what string) (map[string]json.RawMessage, error) {
	if len(body) > llm.MaxBodyBytes {
		return nil, llm.ErrFrameTooLarge
	}
	if err := strictjson.Check(body); err != nil {
		return nil, fmt.Errorf("invalid %s: %v", what, err)
	}
	m, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %v", what, err)
	}
	return m, nil
}

// frames reads event-stream messages for a stream decoder, mapping a cut
// frame onto io.ErrUnexpectedEOF and capping each payload at
// llm.MaxFrameBytes.
type frames struct{ r *eventstream.Reader }

func (f frames) next() (eventstream.Message, error) {
	m, err := f.r.Next()
	switch {
	case err == nil:
		if len(m.Payload) > llm.MaxFrameBytes {
			return m, llm.ErrFrameTooLarge
		}
		return m, nil
	case errors.Is(err, io.EOF):
		return m, io.EOF
	case errors.Is(err, io.ErrUnexpectedEOF):
		return m, io.ErrUnexpectedEOF
	}
	return m, err
}

// exception maps an event-stream exception frame onto a neutral error.
func exception(m eventstream.Message) *llm.Error {
	kind := m.Headers[":exception-type"]
	if kind == "" {
		kind = m.Headers[":error-code"]
	}
	var msg string
	if o, err := object(m.Payload); err == nil {
		_ = take(o, "message", &msg)
		if msg == "" {
			_ = take(o, "Message", &msg)
		}
	}
	if msg == "" {
		msg = string(m.Payload)
	}
	t := llm.ErrorTypeAPI
	switch kind {
	case "throttlingException":
		t = llm.ErrorTypeRateLimit
	case "validationException":
		t = llm.ErrorTypeInvalidRequest
	case "serviceUnavailableException":
		t = llm.ErrorTypeOverloaded
	case "modelTimeoutException":
		t = llm.ErrorTypeTimeout
	case "accessDeniedException":
		t = llm.ErrorTypePermission
	}
	if kind != "" {
		msg = kind + ": " + msg
	}
	return &llm.Error{Type: t, Message: msg}
}
