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
