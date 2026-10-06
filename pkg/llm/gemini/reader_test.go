package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

func TestReaderConformance(t *testing.T) { llmtest.RunReader(t, Reader{}, nil) }

func TestReaderRegistered(t *testing.T) {
	r, err := llm.ReaderByName(Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(Reader); !ok {
		t.Errorf("reader %q is %T, want Reader", Name, r)
	}
}

// Function calls without ids get one each, by position, and responses
// without ids answer the first unanswered call of their name in the latest
// model turn.
func TestReaderPairsCallIDs(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"text":"go"}]},
		{"role":"model","parts":[
			{"functionCall":{"name":"lookup","args":{"q":"a"}}},
			{"functionCall":{"name":"fetch","args":{}}},
			{"functionCall":{"name":"lookup","args":{"q":"b"}}}]},
		{"role":"user","parts":[
			{"functionResponse":{"name":"lookup","response":{"r":"A"}}},
			{"functionResponse":{"name":"lookup","response":{"r":"B"}}},
			{"functionResponse":{"name":"fetch","response":{"r":"F"}}}]},
		{"role":"model","parts":[{"functionCall":{"id":"own-1","name":"lookup"}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{}}}]}]}`
	req, err := Reader{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var calls, results []string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolUse:
				calls = append(calls, b.ID)
			case llm.BlockToolResult:
				results = append(results, b.ToolUseID+"="+b.Content[0].Text)
			}
		}
	}
	wantCalls := []string{"gemini-call-0-lookup", "gemini-call-1-fetch", "gemini-call-2-lookup", "own-1"}
	wantResults := []string{
		`gemini-call-0-lookup={"r":"A"}`, `gemini-call-2-lookup={"r":"B"}`, `gemini-call-1-fetch={"r":"F"}`, `own-1={}`,
	}
	if strings.Join(calls, " ") != strings.Join(wantCalls, " ") {
		t.Errorf("call ids = %v, want %v", calls, wantCalls)
	}
	if strings.Join(results, " ") != strings.Join(wantResults, " ") {
		t.Errorf("results = %v, want %v", results, wantResults)
	}
}

// Requests the reader refuses rather than guess at.
func TestReaderRejects(t *testing.T) {
	for body, want := range map[string]string{
		`{"systemInstruction":{"parts":[{"text":"a"}]},"system_instruction":{"parts":[{"text":"b"}]}}`:                                                                                              `keys "systemInstruction" and "system_instruction" name the same field`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","mime_type":"text/plain","data":"x"}}]}]}`:                                                                      `name the same field`,
		`{"contents":[{"role":"user","parts":[]}]}`:                                                                                                                                                 "contents.0.parts: required",
		`{"contents":[{"role":"user","parts":[{"text":"a","inlineData":{}}]}]}`:                                                                                                                     "only one may be set",
		`{"contents":[{"role":"user","parts":[{"thought":true}]}]}`:                                                                                                                                 "contents.0.parts.0: one of text,",
		`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`:                                                                                                  `no unanswered functionCall named "f"`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f"}}]},{"role":"model","parts":[{"text":"x"}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`: `no unanswered functionCall named "f"`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":[1]}}]}]}`:                                                                                                        "functionCall.args: must be an object",
		`{"contents":[{"role":"function","parts":[{"text":"x"}]}]}`:                                                                                                                                 "a function turn holds only functionResponse parts",
		`{"contents":["hi"]}`: "contents.0: must be an object",
		`{"systemInstruction":{"parts":[{"inlineData":{"mimeType":"image/png","data":"x"}}]}}`: "systemInstruction.parts.0: only text parts",
		`{"tools":[{"functionDeclarations":[{"description":"x"}]}]}`:                           "tools.0.functionDeclarations.0.name: required",
		`{"toolConfig":{"functionCallingConfig":{"mode":"SOMETIMES"}}}`:                        "toolConfig.functionCallingConfig.mode: unsupported value",
		`{"generationConfig":{"thinkingConfig":{"thinkingBudget":-5}}}`:                        "thinkingBudget: unsupported value",
		`[]`: "invalid request body",
	} {
		_, err := Reader{}.DecodeRequest([]byte(body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeRequest(%s) = %v, want an error containing %q", body, err, want)
		}
	}
}

// A blocked prompt has no candidates: an empty refusal.
func TestReaderBlockedPrompt(t *testing.T) {
	resp, err := Reader{}.DecodeResponse([]byte(`{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "refusal" || len(resp.Content) != 0 || resp.Usage.InputTokens != 3 {
		t.Errorf("response = %+v, want an empty refusal with 3 input tokens", resp)
	}
}

// An error frame ends the stream with an error event.
func TestReaderStreamError(t *testing.T) {
	dec := Reader{}.NewResponseDecoder(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}]}\n\n" +
		"data: {\"error\":{\"code\":429,\"message\":\"quota\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n"))
	var last llm.Event
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		last = ev
	}
	got, _ := json.Marshal(last.Error)
	if last.Type != llm.EventError || last.Error.Type != llm.ErrorTypeRateLimit {
		t.Errorf("last event = %s %s, want a rate_limit_error event", last.Type, got)
	}
}

// A JSON-array stream cut inside a chunk is a cut stream, and a chunk over
// llm.MaxFrameBytes ends it with ErrFrameTooLarge.
func TestReaderArrayStreamEnds(t *testing.T) {
	drain := func(body string) error {
		dec := Reader{}.NewResponseDecoder(strings.NewReader(body))
		for {
			if _, err := dec.Next(); err != nil {
				return err
			}
		}
	}
	cut := `[{"candidates":[{"content":{"parts":[{"text":"a"}]}}]},{"candidates":[{"content":{"parts":[{"te`
	if err := drain(cut); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("cut array stream ended with %v, want io.ErrUnexpectedEOF", err)
	}
	big := `[{"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("x", llm.MaxFrameBytes+1) + `"}]}}]}]`
	if err := drain(big); !errors.Is(err, llm.ErrFrameTooLarge) {
		t.Errorf("oversized array chunk ended with %v, want llm.ErrFrameTooLarge", err)
	}
}
