package bedrock

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/eventstream"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

func TestConverseReaderConformance(t *testing.T) { llmtest.RunReader(t, ConverseReader{}, nil) }

func TestInvokeReaderConformance(t *testing.T) { llmtest.RunReader(t, InvokeReader{}, nil) }

func TestReadersRegistered(t *testing.T) {
	for name, want := range map[string]llm.Reader{ConverseName: ConverseReader{}, InvokeName: InvokeReader{}} {
		r, err := llm.ReaderByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if r != want {
			t.Errorf("reader %q is %T, want %T", name, r, want)
		}
	}
}

// The binary stream of every golden is documented frame by frame in its
// stream_frames: check the two agree, so a reviewer can trust the readable
// form.
func TestGoldenStreamFrames(t *testing.T) {
	files, _ := filepath.Glob("../llmtest/testdata/readers/bedrock-*/*.json")
	if len(files) == 0 {
		t.Fatal("no bedrock goldens")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Frames []struct {
				Event, Payload, Chunk string
				Cut                   bool
			} `json:"stream_frames"`
			B64 *string `json:"stream_b64"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if c.B64 == nil {
			continue
		}
		stream, err := base64.StdEncoding.DecodeString(*c.B64)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		r := eventstream.NewReader(bytes.NewReader(stream))
		for i, fr := range c.Frames {
			m, err := r.Next()
			if fr.Cut {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("%s: frame %d: %v, want a cut frame", f, i, err)
				}
				break
			}
			if err != nil {
				t.Fatalf("%s: frame %d: %v", f, i, err)
			}
			got := string(m.Payload)
			want := fr.Payload
			if fr.Chunk != "" {
				var p struct{ Bytes []byte }
				_ = json.Unmarshal(m.Payload, &p)
				got, want = string(p.Bytes), fr.Chunk
			}
			if m.Headers[":event-type"] != fr.Event || got != want {
				t.Errorf("%s: frame %d = %s %s, want %s %s", f, i, m.Headers[":event-type"], got, fr.Event, want)
			}
		}
	}
}

// Requests the readers refuse rather than guess at.
func TestReadersReject(t *testing.T) {
	for _, c := range []struct {
		r    llm.Reader
		body string
		want string
	}{
		{ConverseReader{}, `{"messages":[{"role":"user","content":[{"text":"a","image":{}}]}]}`, "must hold exactly one member, has 2"},
		{ConverseReader{}, `{"messages":[{"role":"user","content":"hi"}]}`, "messages.0.content"},
		{ConverseReader{}, `{"messages":[{"role":"user","content":[],"extra":1}]}`, "extra: unknown message field"},
		{ConverseReader{}, `{"messages":[{"role":"assistant","content":[{"toolUse":{"name":"f"}}]}]}`, "toolUse.toolUseId: required"},
		{ConverseReader{}, `{"messages":[{"role":"user","content":[{"toolResult":{"toolUseId":"t","status":"maybe"}}]}]}`, "unknown status"},
		{ConverseReader{}, `{"messages":[{"role":"user","content":[{"image":{"format":"png","source":{"uri":"x"}}}]}]}`, "unsupported source"},
		{ConverseReader{}, `{"messages":[],"toolConfig":{"toolChoice":{"sometimes":{}}}}`, "unsupported member"},
		{ConverseReader{}, `{"messages":[],"toolConfig":{"tools":[{"toolSpec":{"name":"f","inputSchema":{"yaml":"x"}}}]}}`, "inputSchema"},
		{ConverseReader{}, `{"messages":[],"Messages":[]}`, "differ only by case"},
		{InvokeReader{}, `{"inputText":7}`, "inputText: must be a string"},
		{InvokeReader{}, `{"prompt":"x","max_gen_len":1,"max_tokens":2}`, "only one may be set"},
		{InvokeReader{}, `{"prompt":"x","history":"y"}`, "history: unsupported field"},
		{InvokeReader{}, `{"schemaVersion":"messages-v1","messages":[],"inferenceConfig":{"maxTokens":1,"max_new_tokens":2}}`, "only one may be set"},
		{InvokeReader{}, `{"anthropic_version":"bedrock-2023-05-31","messages":[]}`, "anthropic reader"},
		{InvokeReader{}, `{"messages":[{"role":"tool","content":"x"}]}`, "tool_call_id: required"},
	} {
		_, err := c.r.DecodeRequest([]byte(c.body))
		if !errors.As(err, new(*llm.RequestError)) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s DecodeRequest(%s) = %v, want a *llm.RequestError containing %q", c.r.Name(), c.body, err, c.want)
		}
	}
}

// ErrUnsupportedFormat is reachable with errors.Is on every path.
func TestUnsupportedFormat(t *testing.T) {
	_, err := InvokeReader{}.DecodeRequest([]byte(`{"inputs":"x"}`))
	if !errors.Is(err, ErrUnsupportedFormat) || !errors.As(err, new(*llm.RequestError)) {
		t.Errorf("request: %v", err)
	}
	if _, err := (InvokeReader{}).DecodeResponse([]byte(`{"type":"message","content":[]}`)); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("response: %v", err)
	}
	chunk := func(c string) []byte {
		p, _ := json.Marshal(map[string][]byte{"bytes": []byte(c)})
		return eventstream.Encode(map[string]string{":message-type": "event", ":event-type": "chunk"}, p)
	}
	for name, stream := range map[string][]byte{
		"anthropic":       chunk(`{"type":"message_start","message":{}}`),
		"chat tool calls": chunk(`{"choices":[{"delta":{"tool_calls":[]}}]}`),
	} {
		dec := InvokeReader{}.NewResponseDecoder(bytes.NewReader(stream))
		var err error
		for err == nil {
			_, err = dec.Next()
		}
		if !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("%s stream: %v", name, err)
		}
	}
}

// A Bedrock exception frame mid-stream is an error event, then io.EOF.
func TestStreamException(t *testing.T) {
	stream := append(
		eventstream.Encode(map[string]string{":message-type": "event", ":event-type": "messageStart"}, []byte(`{"role":"assistant"}`)),
		eventstream.Encode(map[string]string{":message-type": "exception", ":exception-type": "throttlingException"}, []byte(`{"message":"Too many requests"}`))...)
	dec := ConverseReader{}.NewResponseDecoder(bytes.NewReader(stream))
	var evs []llm.Event
	var err error
	for {
		var ev llm.Event
		if ev, err = dec.Next(); err != nil {
			break
		}
		evs = append(evs, ev)
	}
	if err != io.EOF || len(evs) != 2 || evs[1].Type != llm.EventError ||
		evs[1].Error.Type != llm.ErrorTypeRateLimit || !strings.Contains(evs[1].Error.Message, "Too many requests") {
		t.Fatalf("events %+v, err %v", evs, err)
	}
}

// A stream that stops but never sends metadata still ends cleanly; one that
// never stops is cut.
func TestStreamEnd(t *testing.T) {
	ev := func(typ, payload string) []byte {
		return eventstream.Encode(map[string]string{":message-type": "event", ":event-type": typ}, []byte(payload))
	}
	start := ev("messageStart", `{"role":"assistant"}`)
	text := ev("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hi"}}`)
	stop := ev("messageStop", `{"stopReason":"max_tokens"}`)
	for name, c := range map[string]struct {
		stream []byte
		want   error
	}{
		"no metadata": {bytes.Join([][]byte{start, text, stop}, nil), io.EOF},
		"no stop":     {bytes.Join([][]byte{start, text}, nil), io.ErrUnexpectedEOF},
	} {
		dec := ConverseReader{}.NewResponseDecoder(bytes.NewReader(c.stream))
		var evs []llm.Event
		var err error
		for {
			var e llm.Event
			if e, err = dec.Next(); err != nil {
				break
			}
			evs = append(evs, e)
		}
		if err != c.want {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
		if c.want == io.EOF {
			if verr := llmtest.ValidateStream(evs); verr != nil {
				t.Errorf("%s: %v", name, verr)
			}
		}
	}
}
