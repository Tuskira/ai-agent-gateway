//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	// The translation adapters, registered as cmd/gateway does.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/anthropic"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/openaicompat"
)

// chatCompletionsUpstream is an httptest server speaking OpenAI Chat
// Completions (JSON, or SSE for a streamed request) that remembers the last
// request it saw.
type chatCompletionsUpstream struct {
	*httptest.Server
	hits atomic.Int32

	mu     sync.Mutex
	body   []byte
	header http.Header
	path   string
}

func newChatCompletionsUpstream(t *testing.T) *chatCompletionsUpstream {
	t.Helper()
	u := &chatCompletionsUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.body, u.header, u.path = b, r.Header.Clone(), r.URL.RequestURI()
		u.mu.Unlock()
		var req struct{ Stream bool }
		_ = json.Unmarshal(b, &req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []string{
				`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"translated"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}}`,
				`[DONE]`,
			} {
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-e2e","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"translated"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *chatCompletionsUpstream) last() ([]byte, http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body, u.header, u.path
}

// TestModelRegistryTranslatedEndToEnd: models of another wire format exist
// only as registry rows, created through /api/v1/models. An
// Anthropic-format call naming one is translated to Chat Completions and
// back, non-stream and stream; the vendor sees the target's credential (or
// none, for a keyless target), never the caller's headers; the durable
// capture row says what was asked, which vendor (the label) and model
// answered, that it was translated, and which target it was. The API
// refuses a reserved or malformed label.
func TestModelRegistryTranslatedEndToEnd(t *testing.T) {
	h := newRegistryHarness(t)
	chat := newChatCompletionsUpstream(t)

	if resp := h.api(t, http.MethodPost, "/api/v1/credentials", `{"name":"groq-prod","type":"api_key","payload":{"api_key":"gsk_managed"}}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create credential: %d", resp.StatusCode)
	}
	create := func(body string) (int, string) {
		resp := h.api(t, http.MethodPost, "/api/v1/models", body, nil)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := create(`{"name":"fast","targets":[{"vendor":"openai_compat","model":"llama-3.3-70b","base_url":"` + chat.URL + `/v1","credential":"groq-prod","label":"groq"}]}`); code != http.StatusCreated ||
		!strings.Contains(body, `"label":"groq"`) {
		t.Fatalf("create fast: %d %s", code, body)
	}
	if code, body := create(`{"name":"local","targets":[{"vendor":"openai_compat","model":"qwen","base_url":"` + chat.URL + `/v1","allow_caller_key":true,"label":"ollama"}]}`); code != http.StatusCreated {
		t.Fatalf("create local: %d %s", code, body)
	}
	// dead native target first, translated second: the registry's fallback.
	if code, body := create(`{"name":"resilient","targets":[{"vendor":"anthropic","model":"claude-haiku-4-5","base_url":"http://127.0.0.1:1","allow_caller_key":true},` +
		`{"vendor":"openai_compat","model":"qwen","base_url":"` + chat.URL + `/v1","allow_caller_key":true,"label":"ollama"}]}`); code != http.StatusCreated {
		t.Fatalf("create resilient: %d %s", code, body)
	}
	for name, target := range map[string]string{
		"reserved anthropic": `{"vendor":"openai_compat","model":"m","base_url":"` + chat.URL + `/v1","label":"anthropic"}`,
		"reserved bedrock":   `{"vendor":"openai_compat","model":"m","base_url":"` + chat.URL + `/v1","label":"bedrock"}`,
		"uppercase":          `{"vendor":"openai_compat","model":"m","base_url":"` + chat.URL + `/v1","label":"Groq"}`,
		"on anthropic":       `{"vendor":"anthropic","model":"claude-haiku-4-5","label":"groq"}`,
	} {
		if code, body := create(`{"name":"bad-` + shortID() + `","targets":[` + target + `]}`); code != http.StatusBadRequest || !strings.Contains(body, "label") {
			t.Errorf("label %s: %d %s, want 400 naming the label", name, code, body)
		}
	}

	callerHeaders := map[string]string{"x-api-key": "sk-ant-api03-claude-code", "anthropic-beta": "b1", "X-Provider-Key-groq": "gsk_caller", "X-Provider-Key": "sk-caller"}

	for _, stream := range []bool{false, true} {
		marker := "fast-" + shortID()
		body := `{"model":"fast","max_tokens":32,"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"messages":[{"role":"user","content":"` + marker + `"}]}`
		resp, out := h.messages(t, body, callerHeaders)
		if resp.StatusCode != http.StatusOK || !strings.Contains(out, "translated") || strings.Contains(out, "chat.completion") {
			t.Fatalf("fast stream=%v: %d %s", stream, resp.StatusCode, out)
		}
		if stream && (!strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop")) {
			t.Errorf("fast stream: not Anthropic's events:\n%s", out)
		}
		if !stream && !strings.Contains(out, `"model":"fast"`) {
			t.Errorf("fast: the answer must name the model the client asked for: %s", out)
		}
		sent, hdr, path := chat.last()
		if path != "/v1/chat/completions" || hdr.Get("Authorization") != "Bearer gsk_managed" || !strings.Contains(string(sent), `"model":"llama-3.3-70b"`) {
			t.Errorf("fast stream=%v: vendor saw path=%q auth=%q body=%s", stream, path, hdr.Get("Authorization"), sent)
		}
		for name := range hdr {
			switch l := strings.ToLower(name); {
			case l == "x-api-key", l == "anthropic-beta", strings.HasPrefix(l, "x-provider-key"):
				t.Errorf("fast stream=%v: client header %s reached the vendor", stream, name)
			}
		}
		row := h.capturedRow(t, marker)
		if row.Provider != "anthropic" || row.RequestedModel != "fast" || row.ResolvedVendor != "groq" || row.ResolvedModel != "llama-3.3-70b" ||
			row.Model != "llama-3.3-70b" || !row.Translated || row.FallbackIndex != 0 || row.StatusCode != 200 || row.InputTokens != 12 {
			t.Errorf("fast stream=%v: row = %+v", stream, row)
		}
	}

	// Keyless: the caller has no vendor key (only Claude Code's Anthropic
	// one, which must not leave), and the vendor gets no credential.
	marker := "local-" + shortID()
	resp, out := h.messages(t, `{"model":"local","max_tokens":32,"messages":[{"role":"user","content":"`+marker+`"}]}`, map[string]string{"x-api-key": "sk-ant-api03-claude-code"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local: %d %s", resp.StatusCode, out)
	}
	if _, hdr, _ := chat.last(); len(hdr.Values("Authorization")) != 0 || hdr.Get("X-Api-Key") != "" {
		t.Errorf("local: vendor saw Authorization %q X-Api-Key %q, want no credential", hdr.Get("Authorization"), hdr.Get("X-Api-Key"))
	}
	if row := h.capturedRow(t, marker); row.ResolvedVendor != "ollama" || !row.Translated || row.StatusCode != 200 {
		t.Errorf("local: row = %+v", row)
	}

	// Fallback across wire formats, before the first byte.
	marker = "resilient-" + shortID()
	resp, out = h.messages(t, `{"model":"resilient","max_tokens":32,"messages":[{"role":"user","content":"`+marker+`"}]}`, map[string]string{"X-Provider-Key-ollama": "anything"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "translated") {
		t.Fatalf("resilient: %d %s", resp.StatusCode, out)
	}
	if _, hdr, _ := chat.last(); hdr.Get("Authorization") != "Bearer anything" {
		t.Errorf("resilient: Authorization %q, want the caller's X-Provider-Key-ollama", hdr.Get("Authorization"))
	}
	if row := h.capturedRow(t, marker); row.FallbackIndex != 1 || row.ResolvedVendor != "ollama" || !row.Translated || row.RequestedModel != "resilient" {
		t.Errorf("resilient: row = %+v", row)
	}

	// An unregistered name is still the byte-for-byte passthrough.
	before := chat.hits.Load()
	resp, _ = h.messages(t, `{"model":"claude-haiku-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"x-api-key": "sk-ant-api03-claude-code"})
	if resp.StatusCode != http.StatusOK || chat.hits.Load() != before || h.passthrough.hits.Load() == 0 {
		t.Errorf("unregistered: status %d, chat hits %d -> %d, passthrough hits %d", resp.StatusCode, before, chat.hits.Load(), h.passthrough.hits.Load())
	}
	if _, hdr, _ := h.passthrough.last(); hdr.Get("X-Api-Key") != "sk-ant-api03-claude-code" {
		t.Errorf("unregistered: passthrough saw X-Api-Key %q", hdr.Get("X-Api-Key"))
	}
}
