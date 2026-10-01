package llmplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"

	// The adapters a translated target needs, registered as cmd/gateway does.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/anthropic"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/openaicompat"
)

const (
	chatJSON = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":3,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":5}}}`
	chatSSE  = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":3,\"total_tokens\":18,\"prompt_tokens_details\":{\"cached_tokens\":5}}}\n\n" +
		"data: [DONE]\n\n"
	msgReq = `{"model":"gpt-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
)

// routedGateway serves the plane with a model registry holding "gpt-5": one
// openai_compat target at chatURL (vendor model gpt-4o, so the call is
// priced) with its own credential, key. Every other model goes to anthURL.
func routedGateway(t *testing.T, anthURL, chatURL, key string, rec Recorder) *httptest.Server {
	t.Helper()
	f := newRegistryFixture(t)
	f.credential(t, "vendor-key", map[string]string{"api_key": key})
	f.model(t, "gpt-5", nil, store.ModelTarget{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: chatURL + "/v1", Credential: "vendor-key"})
	return gateway(t, Config{UpstreamBaseURL: anthURL, Registry: f.registry}, rec)
}

// translatedTarget drives one registry target through the translation
// engine the way the router does (translatorFor -> buildTarget), for the
// tests that look at the built upstream request rather than a whole round
// trip. creds is the decrypted credential the target names, nil for none.
type translatedTarget struct {
	t     resolvedTarget
	creds map[string]string
}

// mustTranslated is an Anthropic-dialect call's view of target (vendor
// openai_compat unless set); key, when not empty, is the credential it names.
func mustTranslated(t *testing.T, target store.ModelTarget, key string) translatedTarget {
	t.Helper()
	if target.Vendor == "" {
		target.Vendor = "openai_compat"
	}
	tr := translatorFor("anthropic", "/v1/messages", target)
	if tr == nil {
		t.Fatalf("no translator for anthropic -> %s", target.Vendor)
	}
	tt := translatedTarget{t: resolvedTarget{ModelTarget: target, tr: tr}}
	if key != "" {
		tt.t.Credential, tt.creds = "vendor-key", map[string]string{"api_key": key}
	}
	return tt
}

func (p translatedTarget) BuildUpstream(ctx context.Context, r *http.Request, body []byte, path string) (*http.Request, error) {
	a, err := (&router{}).buildTarget(ctx, anthropicProvider{}, r, body, path, p.t, p.creds)
	if err != nil {
		return nil, err
	}
	return a.req, nil
}

func (p translatedTarget) ParseUsage(body []byte) Usage {
	return translatedUsage(pricingProviderOf(p.t.ModelTarget))(body)
}

func countingServer(t *testing.T, hits *atomic.Int32, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if h != nil {
			h(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// A model the registry does not hold keeps the untouched Anthropic
// passthrough: same bytes, same query, the client's own headers.
func TestTranslated_MissIsAnthropicPassthrough(t *testing.T) {
	var chatHits, anthHits atomic.Int32
	var gotBody, gotQuery, gotKey, gotBeta string
	anth := countingServer(t, &anthHits, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotQuery, gotKey, gotBeta = string(b), r.URL.RawQuery, r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	chat := countingServer(t, &chatHits, nil)
	gw := routedGateway(t, anth.URL, chat.URL, "route-key", &lastRec{})

	body := `{"model":"claude-sonnet-5",  "max_tokens":1,"messages":[]}`
	resp, _ := do(t, "POST", gw.URL+"/v1/messages?beta=true", map[string]string{"X-Api-Key": "sk-ant-client", "Anthropic-Beta": "b1"}, body)
	if resp.StatusCode != 200 || anthHits.Load() != 1 || chatHits.Load() != 0 {
		t.Fatalf("status %d, anthropic hits %d, chat hits %d", resp.StatusCode, anthHits.Load(), chatHits.Load())
	}
	if gotBody != body || gotQuery != "beta=true" || gotKey != "sk-ant-client" || gotBeta != "b1" {
		t.Errorf("passthrough altered: body=%q query=%q key=%q beta=%q", gotBody, gotQuery, gotKey, gotBeta)
	}
}

// count_tokens for a translated target is answered locally with the adapter's
// estimate (another vendor's tokenizer is not reachable, and Anthropic does not
// know the model), marked estimated, and captured.
func TestTranslated_CountTokensEstimated(t *testing.T) {
	var chatHits, anthHits atomic.Int32
	anth := countingServer(t, &anthHits, nil)
	chat := countingServer(t, &chatHits, nil)
	rec := &lastRec{}
	gw := routedGateway(t, anth.URL, chat.URL, "route-key", rec)

	// "hello" (5 chars) -> ceil(5/4) = 2.
	resp, out := do(t, "POST", gw.URL+"/v1/messages/count_tokens?beta=true", map[string]string{"User-Agent": "Cursor/1.5"}, msgReq)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(out) != `{"input_tokens":2,"estimated":true}` {
		t.Errorf("status %d body %s, want the estimate", resp.StatusCode, out)
	}
	if anthHits.Load()+chatHits.Load() != 0 {
		t.Errorf("forwarded: anthropic %d, chat %d", anthHits.Load(), chatHits.Load())
	}
	c := rec.wait(t)
	if c.StatusCode != 200 || c.InputTokens != 2 || c.StopReason != "estimated" || c.Provider != "anthropic" || c.ResolvedVendor != "openai_compat" ||
		c.RequestedModel != "gpt-5" || c.Model != "gpt-4o" || !c.Translated || c.CostUSD != nil {
		t.Errorf("capture status=%d in=%d stop=%q provider=%q vendor=%q requested=%q model=%q translated=%v cost=%v",
			c.StatusCode, c.InputTokens, c.StopReason, c.Provider, c.ResolvedVendor, c.RequestedModel, c.Model, c.Translated, c.CostUSD)
	}
	// The serveEstimate emit path (local, never reaches an upstream) still
	// classifies and captures the caller's User-Agent like every other path.
	if c.ClientName != "cursor" || c.UserAgent != "Cursor/1.5" {
		t.Errorf("capture client_name=%q user_agent=%q, want cursor / Cursor/1.5", c.ClientName, c.UserAgent)
	}
}

// A request the vendor cannot carry is refused before any upstream call, in
// Anthropic's envelope, naming the field -- never silently dropped.
func TestTranslated_UnsupportedRefused(t *testing.T) {
	var chatHits atomic.Int32
	chat := countingServer(t, &chatHits, nil)
	gw := routedGateway(t, "http://127.0.0.1:1", chat.URL, "route-key", &lastRec{})
	for body, field := range map[string]string{
		`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`: "unsupported_by_route: messages[0].content[0] (document source base64)",
		`{"model":"gpt-5","messages":[{"role":"user","content":"a"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`:                              "unsupported_by_route: tools[0] (web_search_20250305)",
		`{"model":"gpt-5","messages":[{"role":"user","content":"a"}],"mcp_servers":[{"type":"url","url":"https://m.test"}]}`:                                     "unsupported_by_route: mcp_servers",
	} {
		resp, out := do(t, "POST", gw.URL+"/v1/messages", nil, body)
		var e struct {
			Error     struct{ Type, Message string }
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal([]byte(out), &e)
		if resp.StatusCode != http.StatusBadRequest || e.Error.Type != "invalid_request_error" || e.Error.Message != field || e.RequestID == "" {
			t.Errorf("status %d body %s, want 400 %q", resp.StatusCode, out, field)
		}
	}
	// cache_control, metadata, top_k and context_management are on the drop list.
	ok := `{"model":"gpt-5","max_tokens":5,"metadata":{"user_id":"u"},"top_k":3,"context_management":{"edits":[]},
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"a"}]}`
	if resp, out := do(t, "POST", gw.URL+"/v1/messages", nil, ok); resp.StatusCode == http.StatusBadRequest {
		t.Errorf("drop-list request refused: %s", out)
	}
	if chatHits.Load() != 1 {
		t.Errorf("chat hits = %d, want only the drop-list request", chatHits.Load())
	}
}

// Only the resolved key and the translated body reach a third-party vendor:
// no client header, no query, never the gateway key or an Anthropic key, and
// never an X-Provider-Key header.
func TestTranslatedTarget_HeaderIsolation(t *testing.T) {
	build := func(p translatedTarget, hdr map[string]string) (*http.Request, error) {
		r := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(msgReq))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return p.BuildUpstream(r.Context(), r, []byte(msgReq), "/v1/messages")
	}
	onlyVendorHeaders := func(what string, up *http.Request) {
		t.Helper()
		for name := range up.Header {
			switch strings.ToLower(name) {
			case "authorization", "content-type", "accept":
			default:
				t.Errorf("%s: client header %s reached the vendor", what, name)
			}
		}
	}
	all := map[string]string{
		"X-Api-Key": "sk-byok", "Authorization": "Bearer gk_gateway", "X-Gateway-Key": "gk_gateway",
		"Anthropic-Beta": "b1", "Anthropic-Version": "2023-06-01", "Cookie": "c=1", "X-Claude-Code-Session-Id": "s",
		"X-Provider-Key": "sk-vendor", "X-Provider-Key-groq": "gsk",
	}

	// The target's own credential: the only key sent, whatever the caller offers.
	managed := mustTranslated(t, store.ModelTarget{Model: "gpt-4o", BaseURL: "https://x.test/v1/"}, "target-key")
	up, err := build(managed, all)
	if err != nil {
		t.Fatal(err)
	}
	if up.Method != "POST" || up.URL.String() != "https://x.test/v1/chat/completions" {
		t.Errorf("upstream = %s %s", up.Method, up.URL)
	}
	if got := up.Header.Get("Authorization"); got != "Bearer target-key" {
		t.Errorf("Authorization = %q, want the target's credential", got)
	}
	onlyVendorHeaders("managed", up)

	// Neither a credential nor allow_caller_key: no caller key is taken, not
	// even one addressed to the vendor. (The router never builds such a
	// target -- see TestRegistryTranslated_WhoseKey; this is its last line.)
	closed := mustTranslated(t, store.ModelTarget{Model: "gpt-4o", BaseURL: "https://x.test/v1"}, "")
	for _, hdr := range []map[string]string{all, {"X-Provider-Key": "sk-vendor"}, {"Authorization": "Bearer sk-openai"}} {
		var ce clientError
		if _, err := build(closed, hdr); !errors.As(err, &ce) {
			t.Errorf("closed %v: err = %v, want clientError", hdr, err)
		}
	}

	type tc struct {
		hdr  map[string]string
		want string // Authorization the vendor sees; "" = none at all
	}
	for name, group := range map[string]struct {
		target store.ModelTarget
		cases  []tc
	}{
		"no label": {store.ModelTarget{Model: "gpt-4o", BaseURL: "https://x.test/v1", AllowCallerKey: true}, []tc{
			{all, "Bearer sk-vendor"},
			// The key addressed to the vendor beats the dialect's own header.
			{map[string]string{"X-Provider-Key": "sk-vendor", "X-Api-Key": "sk-byok"}, "Bearer sk-vendor"},
			{map[string]string{"X-Provider-Key": "sk-vendor", "X-Api-Key": "sk-ant-api03-x"}, "Bearer sk-vendor"},
			// The dialect's own credential header, unless it is the gateway's
			// or an Anthropic credential.
			{map[string]string{"X-Api-Key": "sk-byok", "Authorization": "Bearer gk_gateway"}, "Bearer sk-byok"},
			{map[string]string{"Authorization": "Bearer sk-openai"}, "Bearer sk-openai"},
			{map[string]string{"Authorization": "Bearer gk_gateway", "X-Gateway-Key": "gk_gateway"}, ""},
			{map[string]string{"X-Api-Key": "sk-ant-api03-x"}, ""},
			{map[string]string{"Authorization": "Bearer sk-ant-oat01-x"}, ""},
			{map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=x"}, ""},
			// A key addressed to a labelled vendor is not this target's.
			{map[string]string{"X-Provider-Key-zhipu": "zk", "X-Api-Key": "sk-ant-api03-x"}, ""},
			{nil, ""},
		}},
		"label groq": {store.ModelTarget{Model: "llama", BaseURL: "https://x.test/v1", AllowCallerKey: true, Label: "groq"}, []tc{
			{all, "Bearer gsk"},
			{map[string]string{"X-Provider-Key-groq": "gsk", "X-Api-Key": "sk-byok"}, "Bearer gsk"},
			// With a label only the labelled header is the vendor's; the bare
			// one and another vendor's are not read.
			{map[string]string{"X-Provider-Key": "sk-vendor"}, ""},
			{map[string]string{"X-Provider-Key-zhipu": "zk"}, ""},
			{map[string]string{"X-Provider-Key": "sk-vendor", "X-Api-Key": "sk-byok"}, "Bearer sk-byok"},
			{map[string]string{"X-Api-Key": "sk-ant-api03-x"}, ""},
		}},
	} {
		target := mustTranslated(t, group.target, "")
		for _, c := range group.cases {
			up, err := build(target, c.hdr)
			if err != nil {
				t.Errorf("%s %v: err = %v", name, c.hdr, err)
				continue
			}
			if got := up.Header.Get("Authorization"); got != c.want {
				t.Errorf("%s %v: Authorization = %q, want %q", name, c.hdr, got, c.want)
			}
			if _, sent := up.Header["Authorization"]; c.want == "" && sent {
				t.Errorf("%s %v: an Authorization header was sent without a key", name, c.hdr)
			}
			onlyVendorHeaders(name, up)
		}
	}
}

// Per-vendor keys ride on every request of a mixed session, so the native
// passthrough (Anthropic, OpenAI, ...) must never forward them.
func TestCopyHeaders_DropsProviderKeys(t *testing.T) {
	src := http.Header{"X-Provider-Key": {"sk-y"}, "X-Provider-Key-Openai": {"sk-x"}, "X-Provider-Key-Zhipu": {"zk"}, "Anthropic-Beta": {"b"},
		"X-Provider-Keychain": {"not ours"}}
	dst := http.Header{}
	copyHeaders(dst, src)
	if len(dst) != 2 || dst.Get("Anthropic-Beta") != "b" || dst.Get("X-Provider-Keychain") != "not ours" {
		t.Errorf("forwarded = %v, want Anthropic-Beta and X-Provider-Keychain only", dst)
	}
	managed := mustTranslated(t, store.ModelTarget{Model: "gpt-4o", BaseURL: "https://x.test/v1", Label: "openai"}, "target-key")
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Provider-Key-openai", "sk-x")
	if up, _ := managed.BuildUpstream(r.Context(), r, []byte(msgReq), "/v1/messages"); up.Header.Get("Authorization") != "Bearer target-key" {
		t.Errorf("the target's credential must win over the caller's per-vendor key")
	}
}

// A Bearer token that authenticated the caller to the gateway (an OIDC JWT,
// kept as RawCredential) is the gateway's credential, not a vendor key: the
// call goes out without one.
func TestTranslatedTarget_GatewayBearerNotBYOK(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer eyJhbGciOi.jwt")
	r = r.WithContext(pkgauth.WithPrincipal(r.Context(), &pkgauth.Principal{AuthMethod: "oidc", RawCredential: "eyJhbGciOi.jwt"}))
	up, err := mustTranslated(t, store.ModelTarget{Model: "gpt-4o", BaseURL: "https://x.test/v1", AllowCallerKey: true}, "").
		BuildUpstream(r.Context(), r, []byte(msgReq), "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := up.Header["Authorization"]; sent {
		t.Errorf("the gateway credential reached the vendor: %q", up.Header.Get("Authorization"))
	}
}

// The translated body is Anthropic-shaped (input excludes cache); capture for
// a target costed as openai or gemini stores OpenAI's convention, exactly
// what the native OpenAI parser would have stored, and any other label keeps
// Anthropic's split.
func TestTranslatedTarget_UsageConvention(t *testing.T) {
	anth := []byte(`{"id":"m","stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":3}}`)
	native := openaiProvider{}.ParseUsage([]byte(chatJSON))
	for _, label := range []string{"", "openai", "gemini"} {
		got := mustTranslated(t, store.ModelTarget{Model: "m", BaseURL: "https://x.test/v1", Label: label}, "k").ParseUsage(anth)
		if got.InputTokens != native.InputTokens || got.CacheReadTokens != native.CacheReadTokens || got.OutputTokens != native.OutputTokens {
			t.Errorf("label %q: %+v, native OpenAI parse %+v", label, got, native)
		}
	}
	if u := mustTranslated(t, store.ModelTarget{Model: "m", BaseURL: "https://x.test/v1", Label: "deepseek"}, "k").ParseUsage(anth); u.InputTokens != 10 || u.CacheReadTokens != 5 {
		t.Errorf("other label: %+v, want Anthropic split (10 + 5 cache)", u)
	}
	// The same label reached by an OpenAI-dialect client (no translation)
	// is stored in the same convention.
	if u := openAIUsageAs("deepseek", openaiProvider{}.ParseUsage)([]byte(chatJSON)); u.InputTokens != 10 || u.CacheReadTokens != 5 || u.OutputTokens != 3 {
		t.Errorf("same-wire, other label: %+v, want 10 + 5 cache", u)
	}
	if openAIUsageAs("openai", openaiProvider{}.ParseUsage) != nil {
		t.Error("same-wire, openai: the dialect's own parser already has the convention")
	}
}

func assertRoutedCapture(t *testing.T, rec *lastRec) {
	t.Helper()
	c := rec.wait(t)
	if c.Provider != "anthropic" || c.RequestedModel != "gpt-5" || c.ResolvedVendor != "openai_compat" || c.ResolvedModel != "gpt-4o" ||
		c.Model != "gpt-4o" || !c.Translated || c.StatusCode != 200 || c.Error != "" {
		t.Errorf("capture provider=%q requested=%q vendor=%q resolved=%q model=%q translated=%v status=%d err=%q",
			c.Provider, c.RequestedModel, c.ResolvedVendor, c.ResolvedModel, c.Model, c.Translated, c.StatusCode, c.Error)
	}
	if c.InputTokens != 15 || c.CacheReadTokens != 5 || c.OutputTokens != 3 || c.StopReason != "end_turn" {
		t.Errorf("capture usage in=%d cache=%d out=%d stop=%q", c.InputTokens, c.CacheReadTokens, c.OutputTokens, c.StopReason)
	}
	want, _ := pricing.Default.Cost("openai", "gpt-4o", pricing.Usage{Input: 15, CacheRead: 5, Output: 3})
	if c.CostUSD == nil || !near(*c.CostUSD, *want) {
		t.Errorf("cost = %v, want %v", c.CostUSD, *want)
	}
}

func TestTranslated_EndToEndJSON(t *testing.T) {
	var hits atomic.Int32
	var gotPath, gotAuth, gotModel string
	chat := countingServer(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotPath, gotAuth, gotModel = r.URL.String(), r.Header.Get("Authorization"), req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-up")
		w.Header().Set("Openai-Organization", "org-operator")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "9")
		w.Header().Set("Set-Cookie", "__cf_bm=x")
		_, _ = io.WriteString(w, chatJSON)
	})
	rec := &lastRec{}
	gw := routedGateway(t, "http://127.0.0.1:1", chat.URL, "route-key", rec)

	resp, out := do(t, "POST", gw.URL+"/v1/messages?beta=true", map[string]string{"X-Api-Key": "sk-ant-client"}, msgReq)
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer route-key" || gotModel != "gpt-4o" {
		t.Errorf("upstream path=%q auth=%q model=%q", gotPath, gotAuth, gotModel)
	}
	var msg struct {
		Content []struct{ Type, Text string }
		Usage   struct {
			InputTokens          int64 `json:"input_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
		}
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || json.Unmarshal([]byte(out), &msg) != nil {
		t.Fatalf("status %d ct %q body %s", resp.StatusCode, resp.Header.Get("Content-Type"), out)
	}
	if resp.Header.Get("X-Request-Id") != "req-up" || resp.Header.Get("Openai-Organization") != "" ||
		resp.Header.Get("X-Ratelimit-Remaining-Tokens") != "" || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("vendor account headers relayed: %v", resp.Header)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	if m["type"] != "message" || m["role"] != "assistant" || m["model"] != "gpt-5" || m["stop_reason"] != "end_turn" {
		t.Errorf("not an Anthropic message: %s", out)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "hi" {
		t.Errorf("content = %+v", msg.Content)
	}
	if msg.Usage.InputTokens != 10 || msg.Usage.CacheReadInputTokens != 5 || msg.Usage.OutputTokens != 3 {
		t.Errorf("client usage = %+v, want 10/5/3", msg.Usage)
	}
	assertRoutedCapture(t, rec)
}

func TestTranslated_EndToEndSSE(t *testing.T) {
	var hits atomic.Int32
	var includeUsage bool
	chat := countingServer(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream        bool
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		includeUsage = req.Stream && req.StreamOptions.IncludeUsage
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	})
	rec := &lastRec{}
	gw := routedGateway(t, "http://127.0.0.1:1", chat.URL, "route-key", rec)

	body := strings.Replace(msgReq, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)
	resp, out := do(t, "POST", gw.URL+"/v1/messages", nil, body)
	if !includeUsage {
		t.Error("upstream stream did not ask for include_usage")
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"event: message_start", "event: content_block_delta", `"hi"`, "event: message_delta", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "chat.completion.chunk") || strings.Contains(out, "[DONE]") {
		t.Errorf("OpenAI frames leaked to the client:\n%s", out)
	}
	assertRoutedCapture(t, rec)
}

// A vendor error frame inside a 200 stream reaches the client as an error
// event and is captured as a failure, not a clean 200.
func TestTranslated_StreamErrorFrameCaptured(t *testing.T) {
	var hits atomic.Int32
	chat := countingServer(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(`{"choices":[{"delta":{"content":"a"}}]}`, `{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`))
	})
	rec := &lastRec{}
	gw := routedGateway(t, "http://127.0.0.1:1", chat.URL, "route-key", rec)
	_, out := do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(msgReq, `"max_tokens"`, `"stream":true,"max_tokens"`, 1))
	if !strings.Contains(out, "event: error") {
		t.Errorf("client stream lacks the error event:\n%s", out)
	}
	if c := rec.wait(t); !strings.Contains(c.Error, "Rate limit reached") {
		t.Errorf("capture error = %q, want the upstream failure", c.Error)
	}
}

// A vendor redirect must not reach the client: following it would re-send the
// Anthropic request and the client's credentials to the vendor's host.
func TestTranslated_UpstreamRedirectIs502(t *testing.T) {
	var hits atomic.Int32
	chat := countingServer(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.test/x", http.StatusTemporaryRedirect)
	})
	gw := routedGateway(t, "http://127.0.0.1:1", chat.URL, "route-key", &lastRec{})
	req, _ := http.NewRequest("POST", gw.URL+"/v1/messages", strings.NewReader(msgReq))
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
		t.Errorf("status %d Location %q, want 502 and no Location", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// A vendor error keeps its status and arrives in Anthropic's envelope; a
// rejected operator key does not leak the vendor's message about it.
func TestTranslated_UpstreamError(t *testing.T) {
	var hits atomic.Int32
	chat := countingServer(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	})
	gw := routedGateway(t, "http://127.0.0.1:1", chat.URL, "route-key", &lastRec{})
	resp, out := do(t, "POST", gw.URL+"/v1/messages", nil, msgReq)
	var e struct {
		Type      string
		Error     struct{ Type, Message string }
		RequestID string `json:"request_id"`
	}
	if resp.StatusCode != http.StatusUnauthorized || json.Unmarshal([]byte(out), &e) != nil || e.Type != "error" || e.Error.Type == "" {
		t.Errorf("status %d body %s, want 401 Anthropic error envelope", resp.StatusCode, out)
	}
	if e.Error.Message != "upstream credential rejected for model gpt-5" {
		t.Errorf("message = %q, want the generic operator-key message", e.Error.Message)
	}
	if e.RequestID == "" {
		t.Error("vendor-mapped error lacks the gateway request_id")
	}
}

// blockingBody blocks every Read until Close.
type blockingBody struct {
	done   chan struct{}
	closed atomic.Bool
}

func (b *blockingBody) Read([]byte) (int, error) { <-b.done; return 0, io.ErrUnexpectedEOF }
func (b *blockingBody) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.done)
	}
	return nil
}

// Closing the translated stream (the relay ending on client disconnect) must
// close the upstream body and let the translating goroutine exit.
func TestTranslatedTarget_StreamCloseNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	src := &blockingBody{done: make(chan struct{})}
	target := mustTranslated(t, store.ModelTarget{Model: "gpt-4o", BaseURL: "https://x.test/v1"}, "k")
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	up, err := target.BuildUpstream(r.Context(), r, []byte(msgReq), "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: src, Request: up}
	translateResponse(resp)
	_ = resp.Body.Close()
	if !src.closed.Load() {
		t.Error("upstream body not closed")
	}
	for i := 0; runtime.NumGoroutine() > before; i++ {
		if i == 200 {
			t.Fatalf("translating goroutine leaked (%d > %d)", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Through the router: a client that goes away mid-stream closes the vendor
// connection instead of leaving it (and the goroutine) hanging.
func TestTranslated_ClientCancelClosesUpstream(t *testing.T) {
	upstreamGone := make(chan struct{})
	var hits atomic.Int32
	chat := countingServer(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.SplitAfter(chatSSE, "\n\n")[0])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamGone)
	})
	gw := routedGateway(t, "http://127.0.0.1:1", chat.URL, "route-key", &lastRec{})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/messages", strings.NewReader(strings.Replace(msgReq, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = resp.Body.Read(make([]byte, 1))
	cancel()
	resp.Body.Close()
	select {
	case <-upstreamGone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream connection still open after the client left")
	}
}

func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

// Live: with LLMTEST_OPENAI_COMPAT_BASE_URL (+ _MODEL) set, a Claude-Code-shaped
// call goes through the plane to a real OpenAI-compatible vendor registered
// as a labelled, keyless target (allow_caller_key, and the caller sends only
// an Anthropic key, which must not reach it), streamed and not, and is
// captured with the vendor's usage under the target's label.
func TestTranslated_Live(t *testing.T) {
	base := os.Getenv("LLMTEST_OPENAI_COMPAT_BASE_URL")
	if base == "" {
		t.Skip("LLMTEST_OPENAI_COMPAT_BASE_URL not set")
	}
	model := os.Getenv("LLMTEST_OPENAI_COMPAT_MODEL")
	for _, stream := range []bool{false, true} {
		f := newRegistryFixture(t)
		f.model(t, "local", nil, store.ModelTarget{Vendor: "openai_compat", Model: model, BaseURL: base, Label: "ollama", AllowCallerKey: true})
		gw := f.gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1"})
		body := fmt.Sprintf(`{"model":"local","max_tokens":64,"stream":%v,"metadata":{"user_id":"u"},
			"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2;"},{"type":"text","text":"Be terse.","cache_control":{"type":"ephemeral"}}],
			"tools":[{"name":"Bash","description":"Run a command","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
			"messages":[{"role":"user","content":[{"type":"text","text":"Say hi.","cache_control":{"type":"ephemeral"}}]}]}`, stream)
		resp, out := do(t, "POST", gw.URL+"/v1/messages?beta=true", map[string]string{"X-Api-Key": "sk-ant-client"}, body)
		if resp.StatusCode != 200 || !strings.Contains(out, `"model":"local"`) {
			t.Fatalf("stream=%v: status %d body %s", stream, resp.StatusCode, out)
		}
		c := f.rec.wait(t)
		if c.Provider != "anthropic" || c.ResolvedVendor != "ollama" || c.RequestedModel != "local" || c.Model != model || !c.Translated ||
			c.StatusCode != 200 || c.Error != "" || c.InputTokens == 0 || c.OutputTokens == 0 || c.Stream != stream {
			t.Errorf("stream=%v: capture provider=%q vendor=%q requested=%q model=%q translated=%v status=%d err=%q in=%d out=%d stream=%v",
				stream, c.Provider, c.ResolvedVendor, c.RequestedModel, c.Model, c.Translated, c.StatusCode, c.Error, c.InputTokens, c.OutputTokens, c.Stream)
		}
	}
}
