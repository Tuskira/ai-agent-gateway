package llmtest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// readerCase is one reader golden, testdata/readers/<reader name>/<case>.json.
// Each part is optional: a null or missing request, response or stream is
// not exercised.
type readerCase struct {
	// Request is the client's wire request body.
	Request json.RawMessage `json:"request"`
	// Response is the client-visible non-stream response body.
	Response json.RawMessage `json:"response"`
	// SSE is the client-visible response stream, raw.
	SSE *string `json:"sse"`
	// StreamB64 is the client-visible response stream, base64 (standard
	// encoding), for a binary stream such as an AWS event stream. At most
	// one of SSE and StreamB64 is set.
	StreamB64 *string `json:"stream_b64"`
	Expect    struct {
		// Request is the neutral llm.Request; RequestError instead a
		// substring of the *llm.RequestError the request must fail with.
		Request      json.RawMessage `json:"request"`
		RequestError string          `json:"request_error"`
		// Response is the neutral llm.Response; ResponseError instead a
		// substring of the error.
		Response      json.RawMessage `json:"response"`
		ResponseError string          `json:"response_error"`
		// StreamText is the concatenated text deltas, StreamToolCalls the
		// tool_use blocks ({id, name, input}) and StreamStopReason the
		// message_delta stop_reason, of the events read before the stream
		// ended. StreamError is "" for a complete stream (io.EOF and a
		// valid event order), "unexpected EOF" for a cut one
		// (io.ErrUnexpectedEOF), else a substring of the error.
		StreamText       string          `json:"stream_text"`
		StreamToolCalls  json.RawMessage `json:"stream_tool_calls"`
		StreamStopReason string          `json:"stream_stop_reason"`
		StreamError      string          `json:"stream_error"`
	} `json:"expect"`
}

// RunReader checks r against reader goldens: every *.json case at the root
// of goldens, or, with goldens nil, the samples shipped in
// testdata/readers/<r.Name()>/. Each case's request must decode to the
// neutral request (or fail with a *llm.RequestError), its response to the
// neutral response, and its stream to the expected text, tool calls and stop
// reason, ending as expected. It also checks a malformed body is a
// *llm.RequestError.
func RunReader(t *testing.T, r llm.Reader, goldens fs.FS) {
	t.Run("malformed_request", func(t *testing.T) {
		_, err := r.DecodeRequest([]byte("{not json"))
		if !errors.As(err, new(*llm.RequestError)) {
			t.Errorf("err = %v, want *llm.RequestError", err)
		}
	})
	dir := "."
	if goldens == nil {
		goldens, dir = fixtures, path.Join("testdata", "readers", r.Name())
	}
	entries, err := fs.ReadDir(goldens, dir)
	if err != nil {
		t.Skipf("no reader goldens for %q", r.Name())
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := fs.ReadFile(goldens, path.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var c readerCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Run(strings.TrimSuffix(name, ".json"), func(t *testing.T) { runReaderCase(t, r, c) })
	}
}

func runReaderCase(t *testing.T, r llm.Reader, c readerCase) {
	x := c.Expect
	if !isNullJSON(c.Request) {
		t.Run("request", func(t *testing.T) {
			req, err := r.DecodeRequest(c.Request)
			if x.RequestError != "" {
				if !errors.As(err, new(*llm.RequestError)) || !strings.Contains(err.Error(), x.RequestError) {
					t.Fatalf("err = %v, want a *llm.RequestError containing %q", err, x.RequestError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(req)
			sameJSON(t, got, x.Request)
		})
	}
	if !isNullJSON(c.Response) {
		t.Run("response", func(t *testing.T) {
			resp, err := r.DecodeResponse(c.Response)
			if x.ResponseError != "" {
				if err == nil || !strings.Contains(err.Error(), x.ResponseError) {
					t.Fatalf("err = %v, want one containing %q", err, x.ResponseError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(resp)
			sameJSON(t, got, x.Response)
		})
	}
	if c.SSE != nil || c.StreamB64 != nil {
		t.Run("stream", func(t *testing.T) {
			var stream []byte
			switch {
			case c.SSE != nil && c.StreamB64 != nil:
				t.Fatal("golden sets both sse and stream_b64")
			case c.SSE != nil:
				stream = []byte(*c.SSE)
			default:
				var err error
				if stream, err = base64.StdEncoding.DecodeString(*c.StreamB64); err != nil {
					t.Fatalf("stream_b64: %v", err)
				}
			}
			dec := r.NewResponseDecoder(bytes.NewReader(stream))
			var evs []llm.Event
			var err error
			for {
				var ev llm.Event
				if ev, err = dec.Next(); err != nil {
					break
				}
				evs = append(evs, ev)
			}
			switch x.StreamError {
			case "":
				if !errors.Is(err, io.EOF) {
					t.Fatalf("stream ended with %v after %d events, want io.EOF", err, len(evs))
				}
				if verr := ValidateStream(evs); verr != nil {
					t.Fatal(verr)
				}
			case io.ErrUnexpectedEOF.Error():
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("stream ended with %v after %d events, want io.ErrUnexpectedEOF", err, len(evs))
				}
			default:
				if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), x.StreamError) {
					t.Fatalf("stream ended with %v after %d events, want an error containing %q", err, len(evs), x.StreamError)
				}
			}
			text, calls, stop := assemble(evs)
			if text != x.StreamText {
				t.Errorf("stream text = %q, want %q", text, x.StreamText)
			}
			if stop != x.StreamStopReason {
				t.Errorf("stream stop_reason = %q, want %q", stop, x.StreamStopReason)
			}
			want := x.StreamToolCalls
			if isNullJSON(want) {
				want = json.RawMessage(`[]`)
			}
			got, _ := json.Marshal(calls)
			sameJSON(t, got, want)
		})
	}
}

type streamCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// assemble folds stream events into their text, tool calls and stop reason.
func assemble(evs []llm.Event) (string, []streamCall, string) {
	var text strings.Builder
	calls := []streamCall{}
	args := map[int]*strings.Builder{}
	at := map[int]int{} // block index -> calls index
	stop := ""
	for _, ev := range evs {
		switch {
		case ev.Type == llm.EventContentBlockStart && ev.Block != nil && ev.Block.Type == llm.BlockToolUse:
			at[ev.Index] = len(calls)
			args[ev.Index] = &strings.Builder{}
			calls = append(calls, streamCall{ID: ev.Block.ID, Name: ev.Block.Name})
		case ev.Type == llm.EventContentBlockDelta && ev.Delta != nil && ev.Delta.Type == llm.DeltaText:
			text.WriteString(ev.Delta.Text)
		case ev.Type == llm.EventContentBlockDelta && ev.Delta != nil && ev.Delta.Type == llm.DeltaInputJSON:
			if b := args[ev.Index]; b != nil {
				b.WriteString(ev.Delta.PartialJSON)
			}
		case ev.Type == llm.EventMessageDelta:
			stop = ev.StopReason
		}
	}
	for idx, i := range at {
		switch in := args[idx].String(); {
		case in == "":
			calls[i].Input = json.RawMessage(`{}`)
		case json.Valid([]byte(in)):
			calls[i].Input = json.RawMessage(in)
		default:
			// A cut stream may hold half an input: kept as a JSON string.
			calls[i].Input, _ = json.Marshal(in)
		}
	}
	return text.String(), calls, stop
}

func isNullJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}
