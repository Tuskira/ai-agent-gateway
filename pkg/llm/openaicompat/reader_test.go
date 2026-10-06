package openaicompat

import (
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

func TestReaderConformance(t *testing.T) { llmtest.RunReader(t, ChatReader{}, nil) }

func TestReaderRegistered(t *testing.T) {
	r, err := llm.ReaderByName(ReaderName)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(ChatReader); !ok {
		t.Errorf("reader %q is %T, want ChatReader", ReaderName, r)
	}
}

// Requests the reader refuses rather than guess at.
func TestReaderRejects(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"m","max_tokens":1,"max_completion_tokens":2,"messages":[]}`:                                       "only one may be set",
		`{"model":"m","messages":[{"role":"user"}]}`:                                                                 "messages.0.content: required",
		`{"model":"m","messages":[{"role":"tool","content":"x"}]}`:                                                   "messages.0.tool_call_id: required",
		`{"model":"m","messages":[{"role":"user","content":[{"text":"x"}]}]}`:                                        "messages.0.content.0: type: required",
		`{"model":"m","messages":[{"role":"user","content":7}]}`:                                                     "messages.0.content: must be a string",
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f"}}]}]}`: "tool_calls.0: id: required",
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"custom","custom":{}}]}]}`:      "unsupported tool call type",
		`{"model":"m","messages":[],"tools":[{"function":{"name":"f"}}]}`:                                            "tools.0: type: required",
		`{"model":"m","messages":[],"tool_choice":"sometimes"}`:                                                      "tool_choice: unsupported value",
		`{"model":"m","messages":[],"stop":7}`:                                                                       "stop: must be",
		`{"model":"m","Model":"n","messages":[]}`:                                                                    "differ only by case",
		`[]`: "invalid request body",
	} {
		_, err := ChatReader{}.DecodeRequest([]byte(body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeRequest(%s) = %v, want an error containing %q", body, err, want)
		}
	}
}

func TestResponsesReaderConformance(t *testing.T) { llmtest.RunReader(t, ResponsesReader{}, nil) }

func TestCompletionsReaderConformance(t *testing.T) { llmtest.RunReader(t, CompletionsReader{}, nil) }

func TestLegacyReadersRegistered(t *testing.T) {
	for name, want := range map[string]llm.Reader{ResponsesReaderName: ResponsesReader{}, CompletionsReaderName: CompletionsReader{}} {
		r, err := llm.ReaderByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if r != want {
			t.Errorf("reader %q is %T, want %T", name, r, want)
		}
	}
}

// Responses requests the reader refuses rather than guess at.
func TestResponsesReaderRejects(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"m","input":[{"content":"x"}]}`:                                                                "input.0.type: required",
		`{"model":"m","input":[{"type":"message","content":"x"}]}`:                                               "input.0.role: required",
		`{"model":"m","input":[{"role":"user"}]}`:                                                                "input.0.content: required",
		`{"model":"m","input":[{"type":"function_call","name":"f","arguments":"{}"}]}`:                           "input.0.call_id: required",
		`{"model":"m","input":[{"type":"function_call_output","output":"x"}]}`:                                   "input.0.call_id: required",
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_image"}]}]}`:                             "image_url or file_id required",
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"a","file_url":"b"}]}]}`: "exactly one of",
		`{"model":"m","input":7}`:                                                                                "input: must be",
		`{"model":"m","tools":[{"name":"f"}]}`:                                                                   "tools.0: type: required",
		`{"model":"m","tool_choice":"sometimes"}`:                                                                "tool_choice: unsupported value",
		`{"model":"m","_gateway_history":"none"}`:                                                                "_gateway_history: reserved",
	} {
		_, err := ResponsesReader{}.DecodeRequest([]byte(body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeRequest(%s) = %v, want an error containing %q", body, err, want)
		}
	}
}

func TestCompletionsReaderRejects(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"m","prompt":[1212,318]}`:                 "token-id prompts are not supported",
		`{"model":"m","prompt":[[1,2],[3]]}`:                "token-id prompts are not supported",
		`{"model":"m","prompt":{"text":"x"}}`:               "prompt: must be a string",
		`{"model":"m","prompt":["a",{}]}`:                   "prompt: 1: must be a string",
		`{"model":"m","prompt":"x","stop":7}`:               "stop: must be",
		`{"model":"m","prompt":"x","_gateway_history":"x"}`: "_gateway_history: reserved",
	} {
		_, err := CompletionsReader{}.DecodeRequest([]byte(body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeRequest(%s) = %v, want an error containing %q", body, err, want)
		}
	}
}

// A conversation id, like previous_response_id, means the vendor holds the
// earlier turns.
func TestResponsesReaderConversation(t *testing.T) {
	req, err := ResponsesReader{}.DecodeRequest([]byte(`{"model":"m","conversation":{"id":"conv_1"},"input":"next"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(req.Extra[llm.HistoryKey]); got != `"server_side"` {
		t.Errorf("history = %s, want \"server_side\"", got)
	}
	req, err = ResponsesReader{}.DecodeRequest([]byte(`{"model":"m","input":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Extra[llm.HistoryKey]; ok {
		t.Errorf("history set on a request carrying its own conversation: %s", req.Extra[llm.HistoryKey])
	}
}
