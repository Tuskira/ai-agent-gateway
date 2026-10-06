package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// CompletionsReaderName is the registry name of CompletionsReader.
const CompletionsReaderName = "openai_completions"

func init() { llm.RegisterReader(CompletionsReader{}) }

// CompletionsReader is the llm.Reader for the legacy OpenAI Completions wire
// (/v1/completions): one prompt text in, one completion text out.
//
// The prompt is one user turn: a string is one text block, an array of
// strings one text block per string (the API completes each separately; they
// are a batch, not a conversation). A prompt of token ids (an array of
// integers, or of such arrays) is refused: there is no text to read.
// max_tokens, temperature, top_p, stop and stream map to their neutral
// slots; suffix and every other field go to Request.Extra. The request is
// one flat prompt, so Extra[llm.HistoryKey] is llm.HistoryPrompt.
//
// A response is read from its choice with index 0 (the first prompt's); its
// finish_reason and usage map as for Chat Completions.
type CompletionsReader struct{}

func (CompletionsReader) Name() string { return CompletionsReaderName }

// DecodeRequest reads a Completions request body strictly.
func (CompletionsReader) DecodeRequest(body []byte) (*llm.Request, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	req, err := readCompletionsRequest(body)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

func readCompletionsRequest(body []byte) (*llm.Request, error) {
	top, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	if err := reserveHistory(top); err != nil {
		return nil, err
	}
	req := &llm.Request{}
	var prompt, stop json.RawMessage
	for _, f := range []struct {
		key string
		dst any
	}{
		{"model", &req.Model}, {"prompt", &prompt}, {"max_tokens", &req.MaxTokens}, {"temperature", &req.Temperature},
		{"top_p", &req.TopP}, {"stop", &stop}, {"stream", &req.Stream},
	} {
		if err := take(top, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	if len(stop) > 0 {
		var s string
		if json.Unmarshal(stop, &s) == nil {
			req.StopSequences = []string{s}
		} else if err := json.Unmarshal(stop, &req.StopSequences); err != nil {
			return nil, errors.New("stop: must be a string or an array of strings")
		}
	}
	if len(prompt) > 0 {
		texts, err := promptTexts(prompt)
		if err != nil {
			return nil, fmt.Errorf("prompt: %v", err)
		}
		if len(texts) > 0 {
			m := llm.Message{Role: "user"}
			for _, t := range texts {
				m.Content = append(m.Content, llm.Block{Type: llm.BlockText, Text: t})
			}
			req.Messages = []llm.Message{m}
		}
	}
	req.Extra = markHistory(nonEmpty(top), llm.HistoryPrompt)
	return req, nil
}

// promptTexts reads a prompt: a string, or an array of strings.
func promptTexts(raw json.RawMessage) ([]string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, errors.New("must be a string or an array of strings")
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		if json.Unmarshal(it, &s) == nil {
			out = append(out, s)
			continue
		}
		var tokens []json.RawMessage
		var n json.Number
		if json.Unmarshal(it, &n) == nil || json.Unmarshal(it, &tokens) == nil {
			return nil, errors.New("token-id prompts are not supported; send the prompt as text")
		}
		return nil, fmt.Errorf("%d: must be a string", i)
	}
	return out, nil
}

// completionsResponse is a non-stream body and a stream chunk.
type completionsResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		Text         string `json:"text"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// first returns choice 0's text and finish_reason, and whether it is there.
func (c *completionsResponse) first() (text, finish string, ok bool) {
	for _, ch := range c.Choices {
		if ch.Index == 0 {
			return ch.Text, ch.FinishReason, true
		}
	}
	return "", "", false
}

// DecodeResponse reads a non-stream Completions response body.
func (CompletionsReader) DecodeResponse(body []byte) (*llm.Response, error) {
	if len(body) > llm.MaxBodyBytes {
		return nil, llm.ErrFrameTooLarge
	}
	if err := strictjson.Check(body); err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	if _, err := object(body); err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	var in completionsResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	text, finish, _ := in.first()
	return &llm.Response{ID: in.ID, Model: in.Model, Role: "assistant",
		Content: []llm.Block{{Type: llm.BlockText, Text: text}}, StopReason: stopReason(finish, false, ""),
		Usage: in.Usage.neutral()}, nil
}

// NewResponseDecoder reads Completions SSE (data: chunks whose choices carry
// text, then data: [DONE]) over frames that passed the strict check, with
// the Chat decoder's block and ending rules: the stream ends at [DONE], or
// at a chunk with usage once choice 0 has finished; ended before choice 0
// finished it is cut.
func (CompletionsReader) NewResponseDecoder(r io.Reader) llm.StreamDecoder {
	return &completionsDecoder{decoder{
		r:     sse.NewReader(sse.Checked(r, llm.MaxFrameBytes, llm.ErrFrameTooLarge, strictjson.Check), llm.MaxFrameBytes, llm.ErrFrameTooLarge),
		byKey: map[int]*pendingTool{},
	}}
}

// completionsDecoder reuses the Chat decoder's block state and ending, over
// Completions chunks.
type completionsDecoder struct{ decoder }

func (d *completionsDecoder) Next() (llm.Event, error) {
	for len(d.queue) == 0 {
		if d.err != nil {
			return llm.Event{}, d.err
		}
		d.readText()
	}
	ev := d.queue[0]
	d.queue = d.queue[1:]
	return ev, nil
}

func (d *completionsDecoder) readText() {
	_, data, err := d.r.Next()
	switch {
	case errors.Is(err, io.EOF):
		if d.finish == "" {
			d.err = io.ErrUnexpectedEOF
			return
		}
		d.end()
	case err != nil:
		d.err = err
	default:
		d.textChunk(data)
	}
}

func (d *completionsDecoder) textChunk(data string) {
	if data == "[DONE]" {
		if !d.started {
			d.start(nil)
		}
		d.end()
		return
	}
	var c completionsResponse
	if json.Unmarshal([]byte(data), &c) != nil {
		return
	}
	if !d.started {
		d.start(c.Usage)
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		d.upstreamError(c.Error)
		return
	}
	if c.Usage != nil {
		d.usage = c.Usage
	}
	if text, finish, ok := c.first(); ok {
		d.delta(llm.BlockText, text)
		if finish != "" {
			d.finish = finish
		}
	}
	if d.finish != "" && c.Usage != nil {
		d.end()
	}
}
