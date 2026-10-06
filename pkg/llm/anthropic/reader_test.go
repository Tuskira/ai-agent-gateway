package anthropic

import (
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
