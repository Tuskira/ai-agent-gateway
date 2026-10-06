package anthropic

import (
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

func TestBatchReaderConformance(t *testing.T) { llmtest.RunBatchReader(t, BatchReader{}, nil) }

func TestBatchReaderRegistered(t *testing.T) {
	r, err := llm.BatchReaderByName(BatchReaderName)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(BatchReader); !ok {
		t.Errorf("batch reader %q is %T, want BatchReader", BatchReaderName, r)
	}
}
