package llmplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// chatUpstream is an httptest server speaking OpenAI Chat Completions: it
// records what it received and answers chatJSON (or chatSSE for a stream).
type chatUpstream struct {
	*httptest.Server
	hits   atomic.Int32
	mu     sync.Mutex
	body   []byte
	header http.Header
	path   string
}

func newChatUpstream(t *testing.T) *chatUpstream {
	t.Helper()
	u := &chatUpstream{}
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
			_, _ = io.WriteString(w, chatSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatJSON)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *chatUpstream) got() ([]byte, http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body, u.header, u.path
}

// An Anthropic-dialect call for a registry row whose target is
// openai_compat goes through the translation engine: Chat Completions
// upstream with the target's credential and model id, an Anthropic message
// back, and a capture row that says so.
func TestRegistryTranslated_AnthropicToOpenAICompat(t *testing.T) {
	f := newRegistryFixture(t)
	chat := newChatUpstream(t)
	f.credential(t, "vendor", map[string]string{"api_key": "sk-managed"})
	f.model(t, "coder", nil, store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: chat.URL + "/v1", Credential: "vendor"})
	gw := f.gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1"})

	for _, stream := range []bool{false, true} {
		f.rec.last = nil
		body := strings.Replace(aliasBody, "sonnet", "coder", 1)
		if stream {
			body = strings.Replace(body, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)
		}
		resp, out := do(t, "POST", gw.URL+"/v1/messages?beta=true",
			map[string]string{"X-Api-Key": "sk-ant-client", "Anthropic-Beta": "b1", "X-Provider-Key-openai": "sk-caller"}, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v: status %d body %s", stream, resp.StatusCode, out)
		}
		got, hdr, path := chat.got()
		if path != "/v1/chat/completions" || hdr.Get("Authorization") != "Bearer sk-managed" || !strings.Contains(string(got), `"model":"gpt-4o"`) {
			t.Errorf("stream=%v: vendor saw path=%q auth=%q body=%s", stream, path, hdr.Get("Authorization"), got)
		}
		if hdr.Get("X-Api-Key") != "" || hdr.Get("Anthropic-Beta") != "" || hdr.Get("X-Provider-Key-Openai") != "" {
			t.Errorf("stream=%v: client headers reached the vendor: %v", stream, hdr)
		}
		if stream {
			if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") || strings.Contains(out, "chat.completion.chunk") {
				t.Errorf("not an Anthropic stream:\n%s", out)
			}
		} else {
			var m map[string]any
			_ = json.Unmarshal([]byte(out), &m)
			if m["type"] != "message" || m["model"] != "coder" || m["stop_reason"] != "end_turn" {
				t.Errorf("not an Anthropic message: %s", out)
			}
		}
		c := f.rec.wait(t)
		if c.Provider != "anthropic" || c.RequestedModel != "coder" || c.ResolvedVendor != "openai_compat" || c.ResolvedModel != "gpt-4o" ||
			c.Model != "gpt-4o" || !c.Translated || c.FallbackIndex != 0 || c.StatusCode != 200 || c.Stream != stream {
			t.Errorf("stream=%v: capture provider=%q requested=%q vendor=%q resolved=%q model=%q translated=%v fallback=%d status=%d stream=%v",
				stream, c.Provider, c.RequestedModel, c.ResolvedVendor, c.ResolvedModel, c.Model, c.Translated, c.FallbackIndex, c.StatusCode, c.Stream)
		}
		// Priced on the vendor model under the openai card, OpenAI's usage
		// convention (input includes the cached tokens).
		want, _ := pricing.Default.Cost("openai", "gpt-4o", pricing.Usage{Input: 15, CacheRead: 5, Output: 3})
		if c.InputTokens != 15 || c.CacheReadTokens != 5 || c.OutputTokens != 3 || c.CostUSD == nil || !near(*c.CostUSD, *want) {
			t.Errorf("stream=%v: usage in=%d cache=%d out=%d cost=%v, want 15/5/3 %v", stream, c.InputTokens, c.CacheReadTokens, c.OutputTokens, c.CostUSD, *want)
		}
	}
}

// Whose key reaches a translated target follows the registry's rules: the
// target's credential; else the caller's key only with allow_caller_key
// (never the gateway's nor an Anthropic one); else the target is unusable.
func TestRegistryTranslated_WhoseKey(t *testing.T) {
	f := newRegistryFixture(t)
	chat := newChatUpstream(t)
	f.model(t, "closed", nil, store.ModelTarget{Vendor: "openai_compat", Model: "m", BaseURL: chat.URL + "/v1"})
	f.model(t, "open", nil, store.ModelTarget{Vendor: "openai_compat", Model: "m", BaseURL: chat.URL + "/v1", AllowCallerKey: true})
	f.model(t, "open-groq", nil, store.ModelTarget{Vendor: "openai_compat", Model: "m", BaseURL: chat.URL + "/v1", AllowCallerKey: true, Label: "groq"})
	gw := f.gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1"})
	call := func(model string, hdr map[string]string) (int, string) {
		f.rec.last = nil
		resp, out := do(t, "POST", gw.URL+"/v1/messages", hdr, strings.Replace(aliasBody, "sonnet", model, 1))
		return resp.StatusCode, out
	}

	// No credential and no allow_caller_key: refused before any call, even
	// when the caller offers a key addressed to the vendor.
	status, out := call("closed", map[string]string{"X-Provider-Key": "sk-caller", "X-Api-Key": "sk-generic"})
	if status != http.StatusBadRequest || !strings.Contains(out, "model closed: no credential for target 0") || chat.hits.Load() != 0 {
		t.Errorf("closed: status %d body %s hits %d", status, out, chat.hits.Load())
	}
	if c := f.rec.wait(t); c.Error != "no_credential" || c.FallbackIndex != -1 {
		t.Errorf("closed: capture error=%q fallback=%d", c.Error, c.FallbackIndex)
	}

	for _, tc := range []struct {
		model string
		hdr   map[string]string
		want  string // Authorization the vendor sees; "" = none sent
	}{
		{"open", map[string]string{"X-Api-Key": "sk-generic"}, "Bearer sk-generic"},
		{"open", map[string]string{"Authorization": "Bearer sk-bearer"}, "Bearer sk-bearer"},
		{"open", map[string]string{"X-Provider-Key": "sk-vendor", "X-Api-Key": "sk-generic"}, "Bearer sk-vendor"},
		{"open", map[string]string{"X-Provider-Key-groq": "gsk", "X-Api-Key": "sk-generic"}, "Bearer sk-generic"},
		{"open", map[string]string{"X-Api-Key": "sk-ant-api03-x"}, ""},
		{"open", map[string]string{"Authorization": "Bearer gk_gateway"}, ""},
		{"open-groq", map[string]string{"X-Provider-Key-groq": "gsk", "X-Api-Key": "sk-generic"}, "Bearer gsk"},
		{"open-groq", map[string]string{"X-Provider-Key": "sk-vendor", "X-Api-Key": "sk-ant-api03-x"}, ""},
	} {
		status, out := call(tc.model, tc.hdr)
		_, hdr, _ := chat.got()
		if status != http.StatusOK || hdr.Get("Authorization") != tc.want {
			t.Errorf("%s %v: status %d Authorization %q, want 200 and %q (%s)", tc.model, tc.hdr, status, hdr.Get("Authorization"), tc.want, out)
		}
		for name := range hdr {
			if strings.HasPrefix(strings.ToLower(name), "x-provider-key") || strings.EqualFold(name, "X-Api-Key") {
				t.Errorf("%s %v: %s reached the vendor", tc.model, tc.hdr, name)
			}
		}
	}
}

// A target that needs no key (a local Ollama or vLLM): with
// allow_caller_key and a caller that sends no vendor key -- only what
// authenticates it to the gateway, and the Anthropic key Claude Code always
// carries -- the call goes out with no Authorization header at all, and is
// served.
func TestRegistryTranslated_KeylessTarget(t *testing.T) {
	f := newRegistryFixture(t)
	chat := newChatUpstream(t)
	f.model(t, "local", nil, store.ModelTarget{Vendor: "openai_compat", Model: "qwen", BaseURL: chat.URL + "/v1", AllowCallerKey: true, Label: "ollama"})
	gw := f.gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1"})

	for name, hdr := range map[string]map[string]string{
		"nothing":               nil,
		"gateway bearer":        {"Authorization": "Bearer gk_gateway"},
		"claude code's own key": {"X-Api-Key": "sk-ant-api03-x", "Authorization": "Bearer gk_gateway"},
	} {
		for _, stream := range []bool{false, true} {
			f.rec.last = nil
			body := strings.Replace(aliasBody, "sonnet", "local", 1)
			if stream {
				body = strings.Replace(body, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)
			}
			resp, out := do(t, "POST", gw.URL+"/v1/messages", hdr, body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s stream=%v: status %d body %s", name, stream, resp.StatusCode, out)
			}
			_, got, path := chat.got()
			if _, sent := got["Authorization"]; sent || got.Get("X-Api-Key") != "" || path != "/v1/chat/completions" {
				t.Errorf("%s stream=%v: vendor saw path %q Authorization %q X-Api-Key %q, want no credential", name, stream, path, got.Get("Authorization"), got.Get("X-Api-Key"))
			}
			if c := f.rec.wait(t); c.StatusCode != 200 || c.ResolvedVendor != "ollama" || !c.Translated || c.Error != "" {
				t.Errorf("%s stream=%v: capture status=%d vendor=%q translated=%v err=%q", name, stream, c.StatusCode, c.ResolvedVendor, c.Translated, c.Error)
			}
		}
	}

	// A vendor that does want a key answers for itself: its 401 is relayed
	// in the client's envelope, with the vendor's message (the key, or its
	// absence, is the caller's business here).
	strict := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"You didn't provide an API key.","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	t.Cleanup(strict.Close)
	f.model(t, "needs-key", nil, store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: strict.URL + "/v1", AllowCallerKey: true})
	f.registry.Invalidate(f.tenant)
	resp, out := do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(aliasBody, "sonnet", "needs-key", 1))
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(out, `"type":"error"`) || !strings.Contains(out, "You didn't provide an API key.") {
		t.Errorf("needs-key: status %d body %s, want the vendor's 401 in the Anthropic envelope", resp.StatusCode, out)
	}
}

// Fallback is the registry's, whatever the wire format of each target: a
// dead native target falls through to a translated one (and the reverse),
// before the first byte only, and fallback_index says which one answered.
func TestRegistryTranslated_Fallback(t *testing.T) {
	f := newRegistryFixture(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	chat := newChatUpstream(t)
	anth := newAnthropicUpstream(t, http.StatusOK)
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	}))
	t.Cleanup(busy.Close)
	f.model(t, "native-first", nil,
		store.ModelTarget{Vendor: "anthropic", Model: "claude", BaseURL: deadURL, AllowCallerKey: true},
		store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: chat.URL + "/v1", AllowCallerKey: true})
	f.model(t, "translated-first", nil,
		store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: busy.URL + "/v1", AllowCallerKey: true},
		store.ModelTarget{Vendor: "anthropic", Model: "claude", BaseURL: anth.URL, AllowCallerKey: true})
	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid"})

	resp, out := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"X-Api-Key": "sk-caller"}, strings.Replace(aliasBody, "sonnet", "native-first", 1))
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"model":"native-first"`) {
		t.Fatalf("native-first: status %d body %s", resp.StatusCode, out)
	}
	if c := f.rec.wait(t); c.FallbackIndex != 1 || !c.Translated || c.ResolvedVendor != "openai_compat" || c.ResolvedModel != "gpt-4o" || c.RequestedModel != "native-first" {
		t.Errorf("native-first: capture fallback=%d translated=%v vendor=%q resolved=%q requested=%q", c.FallbackIndex, c.Translated, c.ResolvedVendor, c.ResolvedModel, c.RequestedModel)
	}

	f.rec.last = nil
	resp, out = do(t, "POST", gw.URL+"/v1/messages", map[string]string{"X-Api-Key": "sk-caller"}, strings.Replace(aliasBody, "sonnet", "translated-first", 1))
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "msg_1") {
		t.Fatalf("translated-first: status %d body %s", resp.StatusCode, out)
	}
	if c := f.rec.wait(t); c.FallbackIndex != 1 || c.Translated || c.ResolvedVendor != "anthropic" || c.ResolvedModel != "claude" {
		t.Errorf("translated-first: capture fallback=%d translated=%v vendor=%q resolved=%q", c.FallbackIndex, c.Translated, c.ResolvedVendor, c.ResolvedModel)
	}
}

// count_tokens on a translated target is the adapter's estimate, answered
// locally and captured with the registry fields.
func TestRegistryTranslated_CountTokens(t *testing.T) {
	f := newRegistryFixture(t)
	chat := newChatUpstream(t)
	f.model(t, "coder", nil, store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: chat.URL + "/v1", AllowCallerKey: true})
	gw := f.gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1"})

	resp, out := do(t, "POST", gw.URL+"/v1/messages/count_tokens", nil, `{"model":"coder","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(out) != `{"input_tokens":2,"estimated":true}` || chat.hits.Load() != 0 {
		t.Errorf("status %d body %s hits %d, want the estimate and no upstream call", resp.StatusCode, out, chat.hits.Load())
	}
	c := f.rec.wait(t)
	if c.Provider != "anthropic" || c.RequestedModel != "coder" || c.ResolvedVendor != "openai_compat" || c.ResolvedModel != "gpt-4o" || !c.Translated ||
		c.InputTokens != 2 || c.StopReason != "estimated" || c.CostUSD != nil {
		t.Errorf("capture = provider %q requested %q vendor %q resolved %q translated %v in %d stop %q cost %v",
			c.Provider, c.RequestedModel, c.ResolvedVendor, c.ResolvedModel, c.Translated, c.InputTokens, c.StopReason, c.CostUSD)
	}
}

// What the engine cannot serve stays a 400: a client dialect with no
// adapter (OpenAI -> an Anthropic target), and an Anthropic path that is
// not the Messages call (batches).
func TestRegistryTranslated_NoAdapterStill400(t *testing.T) {
	f := newRegistryFixture(t)
	chat := newChatUpstream(t)
	f.model(t, "claude-alias", nil, store.ModelTarget{Vendor: "anthropic", Model: "claude", AllowCallerKey: true})
	f.model(t, "coder", nil, store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: chat.URL + "/v1", AllowCallerKey: true})
	gw := f.gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1", OpenAIEnabled: true, OpenAIBaseURL: "http://127.0.0.1:1"})

	resp, out := do(t, "POST", gw.URL+"/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer sk-x"},
		`{"model":"claude-alias","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "model claude-alias requires translation (not available)") {
		t.Errorf("openai -> anthropic: status %d body %s", resp.StatusCode, out)
	}
	if c := f.rec.wait(t); c.Error != "requires_translation" || c.FallbackIndex != -1 || c.Translated {
		t.Errorf("openai -> anthropic: capture error=%q fallback=%d translated=%v", c.Error, c.FallbackIndex, c.Translated)
	}

	f.rec.last = nil
	resp, out = do(t, "POST", gw.URL+"/v1/messages/batches", map[string]string{"X-Api-Key": "sk-x"}, `{"model":"coder","requests":[]}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "model coder requires translation (not available)") || chat.hits.Load() != 0 {
		t.Errorf("batches: status %d body %s hits %d", resp.StatusCode, out, chat.hits.Load())
	}
}

// A target's label is the vendor capture names (resolved_vendor), the
// provider the call is priced as, and the suffix of the caller's key header
// -- for an Anthropic-dialect client (translated) and an OpenAI-dialect one
// (same wire) alike, in one token convention.
func TestRegistryTranslated_Label(t *testing.T) {
	f := newRegistryFixture(t)
	chat := newChatUpstream(t)
	f.model(t, "fast", nil, store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: chat.URL + "/v1", AllowCallerKey: true, Label: "groq"})
	gw := f.gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1", OpenAIEnabled: true, OpenAIBaseURL: "http://127.0.0.1:1"})

	// "groq" is not openai/gemini: input and cache_read are separate counts
	// (chatJSON: 15 prompt tokens of which 5 cached -> 10 + 5).
	want, _ := pricing.Default.Cost("groq", "gpt-4o", pricing.Usage{Input: 10, CacheRead: 5, Output: 3})
	check := func(what string, translated bool, provider string) {
		t.Helper()
		c := f.rec.wait(t)
		if c.Provider != provider || c.RequestedModel != "fast" || c.ResolvedVendor != "groq" || c.ResolvedModel != "gpt-4o" || c.Translated != translated || c.StatusCode != 200 {
			t.Errorf("%s: capture provider=%q requested=%q vendor=%q resolved=%q translated=%v status=%d",
				what, c.Provider, c.RequestedModel, c.ResolvedVendor, c.ResolvedModel, c.Translated, c.StatusCode)
		}
		if c.InputTokens != 10 || c.CacheReadTokens != 5 || c.OutputTokens != 3 || c.CostUSD == nil || !near(*c.CostUSD, *want) {
			t.Errorf("%s: usage in=%d cache=%d out=%d cost=%v, want 10/5/3 %v", what, c.InputTokens, c.CacheReadTokens, c.OutputTokens, c.CostUSD, *want)
		}
	}

	resp, out := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"X-Provider-Key-groq": "gsk_caller", "X-Api-Key": "sk-ant-api03-x"},
		strings.Replace(aliasBody, "sonnet", "fast", 1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anthropic dialect: status %d body %s", resp.StatusCode, out)
	}
	if _, hdr, _ := chat.got(); hdr.Get("Authorization") != "Bearer gsk_caller" {
		t.Errorf("anthropic dialect: Authorization %q, want the caller's X-Provider-Key-groq", hdr.Get("Authorization"))
	}
	check("anthropic dialect", true, "anthropic")

	f.rec.last = nil
	resp, out = do(t, "POST", gw.URL+"/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer gsk_caller", "X-Provider-Key-groq": "unused"},
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("openai dialect: status %d body %s", resp.StatusCode, out)
	}
	if _, hdr, _ := chat.got(); hdr.Get("Authorization") != "Bearer gsk_caller" || hdr.Get("X-Provider-Key-Groq") != "" {
		t.Errorf("openai dialect: vendor saw Authorization %q X-Provider-Key-groq %q", hdr.Get("Authorization"), hdr.Get("X-Provider-Key-Groq"))
	}
	check("openai dialect", false, "openai")
}
