package llmplane

import (
	"bytes"
	"io"
	"mime"
	"net/http"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// assembledBody folds the captured bytes of a streamed response into the one
// message the call would have returned without streaming (see
// llm.StreamAssembler). ok is false when the response was not a 2xx, the
// reader cannot assemble its streams (only Anthropic Messages today; OpenAI,
// Gemini and Bedrock Converse streams are stored raw), or the stream is not
// complete and well formed: the caller then keeps the raw bytes.
//
// raw is what the client was sent, so an Anthropic model reached on Bedrock's
// invoke route natively arrives as event-stream frames (the client speaks
// Bedrock there); they are re-framed as SSE first, as the registry's
// Anthropic-on-Bedrock path already does before the relay.
func assembledBody(reader string, resp *http.Response, raw []byte) ([]byte, bool) {
	if resp.StatusCode/100 != 2 {
		return nil, false
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/vnd.amazon.eventstream" && reader == readerAnthropic {
		sse, err := io.ReadAll(newEventStreamToSSE(bytes.NewReader(raw)))
		if err != nil {
			return nil, false
		}
		raw = sse
	}
	return llm.AssembleStream(reader, raw)
}
