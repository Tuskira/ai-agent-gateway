package llmplane

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// conversationCase is one golden in testdata/conversation/<reader>.json: a
// client request, a non-stream response and a response stream (SSE, or a
// binary stream as base64 with its content type), and the canonical
// conversation and answers the tee makes of them. The detection agent's
// tests read copies of these files (detection/turn/testdata/conversation).
type conversationCase struct {
	Expect struct {
		Answer       json.RawMessage `json:"answer"`
		Conversation json.RawMessage `json:"conversation"`
		StreamAnswer json.RawMessage `json:"stream_answer"`
	} `json:"expect"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	SSE        string          `json:"sse,omitempty"`
	StreamB64  string          `json:"stream_b64,omitempty"`
	StreamType string          `json:"stream_type,omitempty"`
}

// updateGoldens, set by UPDATE_CONVERSATION_GOLDENS=1, rewrites each
// golden's expect from what the tee makes of its inputs.
var updateGoldens = os.Getenv("UPDATE_CONVERSATION_GOLDENS") == "1"

// normalized is what the worker makes of a turn on dialect's generate route.
func normalized(t *testing.T, dialect string, req, resp []byte, respType string, cut bool) teeTurn {
	t.Helper()
	tt := teeTurn{Dialect: dialect, Op: routeGenerate, Request: req, Response: resp, respType: respType, respCut: cut}
	tt.normalize()
	return tt
}

func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sameJSONValue(t *testing.T, what string, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: golden: %v", what, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s\nwant %s", what, got, want)
	}
}

// The canonical conversation and answer of each reader's golden: request,
// whole response and stream, read through the registered Reader.
func TestConversationGoldens(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "conversation", "*.json"))
	if len(files) == 0 {
		t.Fatal("no conversation goldens")
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var c conversationCase
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			stream, streamType := []byte(c.SSE), "text/event-stream; charset=utf-8"
			if c.StreamB64 != "" {
				if stream, err = base64.StdEncoding.DecodeString(c.StreamB64); err != nil {
					t.Fatal(err)
				}
			}
			if c.StreamType != "" {
				streamType = c.StreamType
			}
			whole := normalized(t, name, c.Request, c.Response, "application/json", false)
			if whole.NormalizeError != "" || whole.Conversation == nil || whole.Answer == nil {
				t.Fatalf("normalize: %q", whole.NormalizeError)
			}
			streamed := normalized(t, name, c.Request, stream, streamType, false)
			if streamed.NormalizeError != "" || streamed.Answer == nil {
				t.Fatalf("normalize stream: %q", streamed.NormalizeError)
			}
			if updateGoldens {
				c.Expect.Conversation = marshalJSON(t, whole.Conversation)
				c.Expect.Answer = marshalJSON(t, whole.Answer)
				c.Expect.StreamAnswer = marshalJSON(t, streamed.Answer)
				out, err := json.MarshalIndent(c, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, append(out, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			sameJSONValue(t, "conversation", marshalJSON(t, whole.Conversation), c.Expect.Conversation)
			sameJSONValue(t, "answer", marshalJSON(t, whole.Answer), c.Expect.Answer)
			sameJSONValue(t, "stream answer", marshalJSON(t, streamed.Answer), c.Expect.StreamAnswer)
			// Deterministic: the same input marshals to the same bytes.
			again := normalized(t, name, c.Request, stream, streamType, false)
			if string(marshalJSON(t, again.Conversation)) != string(marshalJSON(t, streamed.Conversation)) ||
				string(marshalJSON(t, again.Answer)) != string(marshalJSON(t, streamed.Answer)) {
				t.Error("two reads of the same turn marshal differently")
			}
		})
	}
}

// A block the canonical shape has no slot for is opaque: its wire form
// verbatim up to teeOpaqueRaw, past that a JSON string of its first
// teeOpaqueRaw bytes (never splitting a rune), with its full size.
func TestOpaqueBlocks(t *testing.T) {
	small := json.RawMessage(`{"type":"search_result","title":"t"}`)
	if got := toBlock(llm.Block{Type: "search_result", Raw: small}); got.Type != teeBlockOpaque ||
		string(got.Raw) != string(small) || got.Bytes != len(small) {
		t.Errorf("small = %+v", got)
	}
	// "é" (two bytes) straddles the cap.
	big := []byte(`{"t":"` + strings.Repeat("a", teeOpaqueRaw-len(`{"t":"`)-1) + "é" + strings.Repeat("b", 100) + `"}`)
	got := toBlock(llm.Block{Type: "x", Raw: big})
	var clipped string
	if err := json.Unmarshal(got.Raw, &clipped); err != nil {
		t.Fatalf("clipped raw is not a JSON string: %v", err)
	}
	if got.Bytes != len(big) || len(clipped) != teeOpaqueRaw-1 || !strings.HasPrefix(string(big), clipped) {
		t.Errorf("clipped to %d bytes (size %d); want %d, the rune left out", len(clipped), got.Bytes, teeOpaqueRaw-1)
	}
	// A neutral kind with no slot keeps its neutral form, minus a redacted
	// payload.
	got = toBlock(llm.Block{Type: llm.BlockRedactedThinking, Data: "c2VjcmV0"})
	if got.Type != teeBlockOpaque || string(got.Raw) != `{"type":"redacted_thinking"}` {
		t.Errorf("redacted_thinking = %+v (raw %s)", got, got.Raw)
	}
}

// A stream cut mid-call folds into what was read: the open tool call keeps
// its partial arguments as a JSON string.
func TestFoldEventsCut(t *testing.T) {
	resp := foldEvents([]llm.Event{
		{Type: llm.EventMessageStart, Message: &llm.Response{ID: "msg_1", Role: "assistant"}},
		{Type: llm.EventContentBlockStart, Index: 0, Block: &llm.Block{Type: llm.BlockThinking}},
		{Type: llm.EventContentBlockDelta, Index: 0, Delta: &llm.Delta{Type: llm.DeltaThinking, Thinking: "hm"}},
		{Type: llm.EventContentBlockStop, Index: 0},
		{Type: llm.EventContentBlockStart, Index: 1, Block: &llm.Block{Type: llm.BlockToolUse, ID: "t1", Name: "f"}},
		{Type: llm.EventContentBlockDelta, Index: 1, Delta: &llm.Delta{Type: llm.DeltaInputJSON, PartialJSON: `{"a":`}},
	})
	sameJSONValue(t, "answer", marshalJSON(t, toAnswer(resp, true)), []byte(
		`{"content":[{"type":"thinking","text":"hm"},{"type":"tool_use","id":"t1","name":"f","input":"{\"a\":"}],"truncated":true}`))
}

// How the worker reads a response it could not read whole: a stream that
// ended early is truncated; a copy cut mid-frame too; a whole body cut at
// the copy's cap is an empty truncated answer and an error; a request it
// cannot read leaves no conversation.
func TestNormalizeEdges(t *testing.T) {
	req := []byte(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n"
	for _, c := range []struct {
		name, resp, respType string
		cut                  bool
		wantAnswer, wantErr  string
	}{
		{"stream ended early", start, "text/event-stream", false,
			`{"content":[{"type":"text","text":"par"}],"truncated":true}`, ""},
		{"copy cut mid-frame", start + "event: content_block_delta\ndata: {\"type\":\"content_bl", "text/event-stream", true,
			`{"content":[{"type":"text","text":"par"}],"truncated":true}`, ""},
		{"whole body cut", `{"type":"message","content":[{"type":"text","text":"par`, "application/json", true,
			`{"content":[],"truncated":true}`, "decode response: "},
		{"whole body malformed", `{"type":"message","type":"x"}`, "application/json", false, "", "decode response: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := normalized(t, readerAnthropic, req, []byte(c.resp), c.respType, c.cut)
			if got.Conversation == nil || !strings.HasPrefix(got.NormalizeError, c.wantErr) || (c.wantErr == "") != (got.NormalizeError == "") {
				t.Fatalf("conversation %v, error %q; want error %q", got.Conversation, got.NormalizeError, c.wantErr)
			}
			if c.wantAnswer == "" {
				if got.Answer != nil {
					t.Errorf("answer = %+v, want none", got.Answer)
				}
				return
			}
			sameJSONValue(t, "answer", marshalJSON(t, got.Answer), []byte(c.wantAnswer))
		})
	}
	got := normalized(t, readerAnthropic, []byte(`{"messages":[{"role":"tool","content":"x"}]}`), nil, "", false)
	if got.Conversation != nil || !strings.HasPrefix(got.NormalizeError, "decode request: ") {
		t.Errorf("unknown role: conversation %v, error %q", got.Conversation, got.NormalizeError)
	}
}

// Bedrock's InvokeModelWithResponseStream for an Anthropic model carries
// the Messages events in eventstream frames: they are read as the stream
// they frame.
func TestNormalizeBedrockEventStream(t *testing.T) {
	var body []byte
	for _, ev := range []string{
		`{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	} {
		payload := marshalJSON(t, map[string][]byte{"bytes": []byte(ev)})
		body = append(body, encodeEventStreamFrame(map[string]string{":message-type": "event", ":event-type": "chunk"}, payload)...)
	}
	req := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	got := normalized(t, readerAnthropic, req, body, "application/vnd.amazon.eventstream", false)
	if got.NormalizeError != "" {
		t.Fatal(got.NormalizeError)
	}
	sameJSONValue(t, "answer", marshalJSON(t, got.Answer), []byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`))
}
