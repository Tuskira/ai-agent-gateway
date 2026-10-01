package translate

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/anthropic"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

// TestRunLive drives the whole engine against a real OpenAI-compatible
// endpoint (the same env as pkg/llm/openaicompat's conformance test): an
// Anthropic Messages request in, an Anthropic response out. The streamed
// answer is read back with the anthropic Provider's decoder and must keep
// Anthropic's event order.
func TestRunLive(t *testing.T) {
	base := os.Getenv("LLMTEST_OPENAI_COMPAT_BASE_URL")
	if base == "" {
		t.Skip("LLMTEST_OPENAI_COMPAT_BASE_URL not set")
	}
	model := os.Getenv("LLMTEST_OPENAI_COMPAT_MODEL")
	target := llm.Target{Vendor: "ollama", BaseURL: base, Model: model, Auth: llm.Auth{APIKey: "llmtest"}}
	run := func(body string) (*http.Response, []byte, Result) {
		results := make(chan Result, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			results <- Run(r.Context(), w, r, dialect, provider, target, Options{RequestID: "req-live"})
		}))
		defer srv.Close()
		resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b, <-results
	}

	t.Run("json", func(t *testing.T) {
		resp, b, res := run(`{"model":"my-alias","max_tokens":64,"system":[{"type":"text","text":"Be terse.","cache_control":{"type":"ephemeral"}}],
			"messages":[{"role":"user","content":"Say hello."}]}`)
		var m map[string]any
		if resp.StatusCode != 200 || json.Unmarshal(b, &m) != nil || m["type"] != "message" || m["model"] != "my-alias" {
			t.Fatalf("status %d body %s", resp.StatusCode, b)
		}
		if res.Err != nil || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("stream", func(t *testing.T) {
		resp, b, res := run(`{"model":"my-alias","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Count to three."}]}`)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status %d ct %q body %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
		}
		dec := anthropic.Provider{}.NewStreamDecoder(strings.NewReader(string(b)))
		var evs []llm.Event
		for {
			ev, err := dec.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("re-decoding the translated stream: %v\n%s", err, b)
			}
			evs = append(evs, ev)
		}
		if err := llmtest.ValidateStream(evs); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		if evs[0].Message.Model != "my-alias" || res.Err != nil || res.Usage.OutputTokens == 0 {
			t.Errorf("model %q result %+v", evs[0].Message.Model, res)
		}
	})

	t.Run("refused before the vendor", func(t *testing.T) {
		resp, b, _ := run(`{"model":"my-alias","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA"}}]}]}`)
		if resp.StatusCode != 400 || !strings.Contains(string(b), "unsupported_by_route: messages[0].content[0] (document source base64)") || !strings.Contains(string(b), "req-live") {
			t.Errorf("status %d body %s", resp.StatusCode, b)
		}
	})
}
