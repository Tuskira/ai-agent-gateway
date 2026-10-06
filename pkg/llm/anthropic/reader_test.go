package anthropic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

func TestReaderConformance(t *testing.T) { llmtest.RunReader(t, Dialect{}, nil) }

func TestReaderRegistered(t *testing.T) {
	r, err := llm.ReaderByName(Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(Dialect); !ok {
		t.Errorf("reader %q is %T, want Dialect", Name, r)
	}
}

func TestCompleteReaderConformance(t *testing.T) { llmtest.RunReader(t, CompleteReader{}, nil) }

func TestCompleteReaderRegistered(t *testing.T) {
	r, err := llm.ReaderByName(CompleteReaderName)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(CompleteReader); !ok {
		t.Errorf("reader %q is %T, want CompleteReader", CompleteReaderName, r)
	}
}

func TestSplitPrompt(t *testing.T) {
	for _, c := range []struct {
		prompt, want string
	}{
		{"just text", `null|[user:"just text"]`},
		{"Human: hi\n\nAssistant:", `null|[user:"hi"]`},
		{"Be brief.\n\nHuman: a\n\nAssistant: b\n\nHuman: c\n\nAssistant:", `"Be brief."|[user:"a" assistant:"b" user:"c"]`},
		{"\n\nHuman:\n\nHuman: x", `null|[user:"x"]`},
	} {
		sys, msgs := splitPrompt(c.prompt)
		s := "null"
		if len(sys) > 0 {
			s = fmt.Sprintf("%q", sys[0].Text)
		}
		var turns []string
		for _, m := range msgs {
			turns = append(turns, fmt.Sprintf("%s:%q", m.Role, m.Content[0].Text))
		}
		if got := s + "|[" + strings.Join(turns, " ") + "]"; got != c.want {
			t.Errorf("splitPrompt(%q) = %s, want %s", c.prompt, got, c.want)
		}
	}
}

func TestCompleteReaderRejects(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"m","prompt":7}`:                                 "prompt:",
		`{"model":"m","prompt":"x","_gateway_history":"x"}`:        "_gateway_history: reserved",
		`{"model":"m","prompt":"x","stop_sequences":"\n\nHuman:"}`: "stop_sequences:",
	} {
		_, err := CompleteReader{}.DecodeRequest([]byte(body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeRequest(%s) = %v, want an error containing %q", body, err, want)
		}
	}
}
