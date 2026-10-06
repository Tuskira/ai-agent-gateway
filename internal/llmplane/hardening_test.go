package llmplane

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// lastRec records the most recent captured call.
type lastRec struct {
	mu   sync.Mutex
	last *pkgsink.LLMCall
}

func (l *lastRec) Record(_ context.Context, c *pkgsink.LLMCall) error {
	l.mu.Lock()
	l.last = c
	l.mu.Unlock()
	return nil
}

func (l *lastRec) get() *pkgsink.LLMCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// wait polls for a recorded call (capture runs after the relay finishes).
func (l *lastRec) wait(t *testing.T) *pkgsink.LLMCall {
	t.Helper()
	for i := 0; i < 200; i++ {
		if c := l.get(); c != nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no call recorded")
	return nil
}

func do(t *testing.T, method, url string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no redirect following in the test client either
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// gateway serves the plane the way cmd/gateway does: a RoleAuthorizer (the
// plane refuses to build without one) behind a stand-in for the auth
// middleware, which puts an `agent` Principal on the context. A test that needs
// a different caller wires Handler itself (see TestPlane_RequiresLLMAccess).
func gateway(t *testing.T, cfg Config, rec Recorder) *httptest.Server {
	t.Helper()
	if cfg.Authorizer == nil {
		cfg.Authorizer = pkgauth.NewRoleAuthorizer()
	}
	h, err := Handler(cfg, rec)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(withPrincipal(h, &pkgauth.Principal{TenantID: "t", Roles: []string{"agent"}}))
	t.Cleanup(gw.Close)
	return gw
}

func withPrincipal(h http.Handler, p *pkgauth.Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(pkgauth.WithPrincipal(r.Context(), p)))
	})
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestBedrockHost_RegionValidation(t *testing.T) {
	for _, ok := range []string{"us-east-1", "eu-central-2", "ap-southeast-4", "us-gov-west-1", "ap-northeast-3", "il-central-1"} {
		if h, err := bedrockHost(ok); err != nil || h != "bedrock-runtime."+ok+".amazonaws.com" {
			t.Errorf("bedrockHost(%q) = %q, %v", ok, h, err)
		}
	}
	for _, bad := range []string{"", "x@evil.com/#", "evil.com#", "us-east-1.evil.com/", "US-EAST-1", "us-east-1/", "us-east-1?a", "a@b", "us_east_1"} {
		if _, err := bedrockHost(bad); err == nil {
			t.Errorf("bedrockHost(%q) accepted", bad)
		}
	}
}

// A caller-chosen region must never redirect the gateway to another host,
// neither via X-Bedrock-Region (Flows B/C) nor via the SigV4 credential scope (A).
func TestBedrock_RegionInjectionRejected(t *testing.T) {
	var hits atomic.Int32
	evil := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer evil.Close()
	target := "x@" + strings.TrimPrefix(evil.URL, "https://") + "/admin#"
	gw := gateway(t, Config{BedrockEnabled: true, BedrockRegion: "us-east-1"}, &lastRec{})

	resp, _ := do(t, "POST", gw.URL+"/bedrock/model/m/invoke", map[string]string{
		"X-Bedrock-Access-Key-Id": "AKIDFAKE", "X-Bedrock-Secret-Access-Key": "x", "X-Bedrock-Region": target,
	}, `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("flow B: status %d, want 400", resp.StatusCode)
	}
	resp, _ = do(t, "POST", gw.URL+"/bedrock/model/m/invoke", map[string]string{
		"Authorization": "AWS4-HMAC-SHA256 Credential=AKID/20260925/x@" + strings.TrimPrefix(evil.URL, "https://") + "#/bedrock/aws4_request, SignedHeaders=host, Signature=00",
	}, `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("flow A: status %d, want 400", resp.StatusCode)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("gateway reached the injected host %d times", n)
	}
}

// fakeSTS answers AssumeRole and records each call's ExternalId.
func fakeSTS(t *testing.T) (*atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var ext atomic.Value
	ext.Store("")
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		hits.Add(1)
		ext.Store(r.PostForm.Get("ExternalId") + "|" + r.PostForm.Get("RoleSessionName"))
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>ASIAFAKE</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>t</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials><AssumedRoleUser><Arn>arn:aws:sts::1:assumed-role/r/s</Arn><AssumedRoleId>X:s</AssumedRoleId></AssumedRoleUser></AssumeRoleResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></AssumeRoleResponse>`)
	}))
	t.Cleanup(sts.Close)
	t.Setenv("AWS_ENDPOINT_URL_STS", sts.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDFAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_PROFILE", "")
	return &hits, &ext
}

func roleRequest(tenant, externalID string) *http.Request {
	r := httptest.NewRequest("POST", "/bedrock/model/m/converse", strings.NewReader(`{}`))
	r.Header.Set(hdrBedrockRoleARN, "arn:aws:iam::111122223333:role/bedrock")
	if externalID != "" {
		r.Header.Set(hdrBedrockExternalID, externalID)
	}
	if tenant != "" {
		r = r.WithContext(pkgauth.WithPrincipal(r.Context(), &pkgauth.Principal{TenantID: tenant}))
	}
	return r
}

// The ExternalID is the caller's tenant ID, never the caller's choice, and the
// assumed-role credentials are reused across requests (one STS call).
func TestBedrock_AssumeRoleExternalIDPinnedAndCached(t *testing.T) {
	hits, ext := fakeSTS(t)
	p := newBedrockProvider("us-east-1", "", "111122223333")
	for i := 0; i < 3; i++ {
		r := roleRequest("tenant-a", "")
		if _, err := p.BuildUpstream(r.Context(), r, []byte(`{}`), "/model/m/converse"); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("AssumeRole calls = %d, want 1 (cached)", hits.Load())
	}
	if got := ext.Load().(string); got != "tenant-a|gw-tenant-a" {
		t.Errorf("ExternalId|RoleSessionName = %q, want the tenant id and gw-<tenant>", got)
	}

	var ce clientError
	r := roleRequest("tenant-a", "tenant-b")
	if _, err := p.BuildUpstream(r.Context(), r, []byte(`{}`), "/model/m/converse"); !errors.As(err, &ce) {
		t.Errorf("caller-supplied foreign external id: err = %v, want clientError", err)
	}
	r = roleRequest("tenant-a", "tenant-a") // matching value is accepted
	if _, err := p.BuildUpstream(r.Context(), r, []byte(`{}`), "/model/m/converse"); err != nil {
		t.Errorf("matching external id rejected: %v", err)
	}
	r = roleRequest("", "")
	if _, err := p.BuildUpstream(r.Context(), r, []byte(`{}`), "/model/m/converse"); !errors.As(err, &ce) {
		t.Errorf("no tenant: err = %v, want clientError", err)
	}
}

// Role assumption is off unless the operator allowlists AWS accounts, and only
// well-formed role ARNs in an allowed account AND partition are assumed: the
// same 12 digits name different accounts in aws and aws-us-gov, so a bare entry
// allows the commercial partition only.
func TestBedrock_RoleAccountAllowlist(t *testing.T) {
	fakeSTS(t)
	build := func(accounts, arn string) error {
		r := roleRequest("tenant-a", "")
		r.Header.Set(hdrBedrockRoleARN, arn)
		_, err := newBedrockProvider("us-east-1", "", accounts).BuildUpstream(r.Context(), r, []byte(`{}`), "/model/m/converse")
		return err
	}
	var ce clientError
	for _, c := range []struct {
		accounts, arn string
		ok            bool
	}{
		{"", "arn:aws:iam::111122223333:role/bedrock", false},             // disabled by default
		{"111122223333", "arn:aws:iam::999999999999:role/bedrock", false}, // account not allowed
		{"111122223333", "arn:aws:iam::111122223333:user/bob", false},     // not a role
		{"111122223333", "arn:aws:iam::111122223333:role/x|y", false},     // malformed
		{"111122223333", "arn:aws:iam::111122223333:role/bedrock", true},
		{" 444455556666 , 111122223333 ", "arn:aws:iam::111122223333:role/path/bedrock", true},
		// A bare entry is the "aws" partition only: the same digits in GovCloud
		// or China are a different AWS account and must be listed explicitly.
		{"999999999999", "arn:aws-us-gov:iam::999999999999:role/bedrock", false},
		{"999999999999", "arn:aws-cn:iam::999999999999:role/bedrock", false},
		{"aws-us-gov:999999999999", "arn:aws-us-gov:iam::999999999999:role/bedrock", true},
		{"aws-us-gov:999999999999", "arn:aws:iam::999999999999:role/bedrock", false},
		// "*" stays the blanket wildcard: every account in every partition.
		{"*", "arn:aws-us-gov:iam::999999999999:role/bedrock", true},
	} {
		err := build(c.accounts, c.arn)
		if c.ok && err != nil || !c.ok && !errors.As(err, &ce) {
			t.Errorf("accounts=%q arn=%q: err = %v, want ok=%v", c.accounts, c.arn, err, c.ok)
		}
	}
	if got := sessionName("t@x/y" + strings.Repeat("z", 100)); got != "gw-t@xy"+strings.Repeat("z", 57) {
		t.Errorf("sessionName = %q (len %d)", got, len(got))
	}
}

func TestRouter_ClientErrorIs400(t *testing.T) {
	rec := &lastRec{}
	gw := gateway(t, Config{BedrockEnabled: true, BedrockRegion: "us-east-1"}, rec)
	resp, body := do(t, "POST", gw.URL+"/bedrock/model/m/invoke",
		map[string]string{"X-Bedrock-Access-Key-Id": "AKID", "User-Agent": "curl/8.4.0"}, `{}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "X-Bedrock-Secret-Access-Key") {
		t.Errorf("status %d body %s", resp.StatusCode, body)
	}
	c := rec.wait(t)
	if c.StatusCode != http.StatusBadRequest {
		t.Errorf("recorded status %d, want 400", c.StatusCode)
	}
	// The emitError path (a request refused before any upstream call) still
	// classifies and captures the caller's User-Agent like a successful call.
	if c.ClientName != "other" || c.UserAgent != "curl/8.4.0" {
		t.Errorf("recorded client_name=%q user_agent=%q, want other / curl/8.4.0", c.ClientName, c.UserAgent)
	}
}

// Every LLM-plane emit path classifies the caller's User-Agent the same
// way, via the same substring rule the MCP plane's requestsByClient
// grouping uses (pkgsink.ClientFamily) -- including "no header at all"
// coming back as "" rather than "other".
func TestRouter_RecordsClientNameAndUserAgent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	cases := []struct {
		name           string
		userAgent      string
		wantClientName string
	}{
		{"claude code cli", "claude-cli/2.1.0 (external, cli)", "claude-code"},
		{"cursor", "Cursor/1.5", "cursor"},
		{"vscode", "Visual Studio Code", "vscode"},
		{"no header", "", ""},
		{"unrecognized", "curl/8.4.0", "other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &lastRec{}
			gw := gateway(t, Config{UpstreamBaseURL: up.URL}, rec)
			do(t, "POST", gw.URL+"/anthropic/v1/messages", map[string]string{"User-Agent": c.userAgent}, `{"model":"claude-sonnet-4-5"}`)
			got := rec.wait(t)
			if got.ClientName != c.wantClientName || got.UserAgent != c.userAgent {
				t.Errorf("client_name=%q user_agent=%q, want %q / %q", got.ClientName, got.UserAgent, c.wantClientName, c.userAgent)
			}
		})
	}
}

// A transport error must not persist a credential carried in the query string.
func TestRouter_ErrorRedactsQuery(t *testing.T) {
	rec := &lastRec{}
	gw := gateway(t, Config{GeminiEnabled: true, GeminiBaseURL: "http://127.0.0.1:1"}, rec)
	do(t, "POST", gw.URL+"/gemini/v1beta/models/gemini-2.5-flash:generateContent?key=AIzaSECRET", nil, `{}`)
	e := rec.wait(t).Error
	if strings.Contains(e, "AIzaSECRET") || !strings.HasPrefix(e, "upstream_error: ") || !strings.Contains(e, "127.0.0.1:1/v1beta/models/gemini-2.5-flash:generateContent") {
		t.Errorf("error = %q", e)
	}
	if got := errorText(&url.Error{Op: "Post", URL: "https://u:p@h/x?key=S", Err: io.EOF}); got != "Post https://h/x: EOF" {
		t.Errorf("errorText = %q", got)
	}
}

// 1-hour cache writes and web-search requests survive mergeUsage into the cost.
func TestRouter_Prices1hCacheAndWebSearch(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"m","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":10,"cache_creation_input_tokens":100000,"cache_creation":{"ephemeral_1h_input_tokens":60000},"server_tool_use":{"web_search_requests":3}}}`)
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL}, rec)
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-sonnet-4-5"}`)
	want := (10*3.0+40000*3.75+60000*6.0+10*15.0)/1e6 + 3*0.01
	if c := rec.wait(t); c.CostUSD == nil || !near(*c.CostUSD, want) {
		t.Errorf("cost = %v, want %v", c.CostUSD, want)
	}
}

// Free endpoints and successful calls with no readable usage store NULL; an
// error response with no usage is correctly $0.
func TestRouter_CostNullVsZero(t *testing.T) {
	status, body := http.StatusOK, `{}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL}, rec)

	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-sonnet-4-5"}`)
	if c := rec.wait(t); c.CostUSD != nil {
		t.Errorf("2xx without usage: cost = %v, want NULL", *c.CostUSD)
	}
	rec.last = nil
	body = `{"input_tokens":6024}`
	do(t, "POST", gw.URL+"/anthropic/v1/messages/count_tokens", nil, `{"model":"claude-sonnet-4-5"}`)
	if c := rec.wait(t); c.CostUSD != nil || c.InputTokens != 6024 {
		t.Errorf("count_tokens: cost = %v tokens = %d, want NULL and 6024", c.CostUSD, c.InputTokens)
	}
	rec.last = nil
	status, body = http.StatusBadRequest, `{"type":"error"}`
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-sonnet-4-5"}`)
	if c := rec.wait(t); c.CostUSD == nil || *c.CostUSD != 0 {
		t.Errorf("4xx: cost = %v, want $0", c.CostUSD)
	}
}

// fakeBedrock is the real Bedrock provider pointed at a local upstream.
type fakeBedrock struct {
	*bedrockProvider
	url string
}

func (f fakeBedrock) BuildUpstream(ctx context.Context, r *http.Request, body []byte, path string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, r.Method, f.url+path, strings.NewReader(string(body)))
}

func bedrockRouter(t *testing.T, h http.HandlerFunc, rec Recorder) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	fb := fakeBedrock{newBedrockProvider("us-east-1", "", ""), up.URL}
	rt := &router{byID: map[string]Provider{"bedrock": fb}, recorder: rec, client: &http.Client{}, pricing: pricing.Default}
	gw := httptest.NewServer(withTags(rt))
	t.Cleanup(gw.Close)
	return gw
}

// Bedrock's token headers fill in models whose body has no counts, and never
// override counts the body does carry.
func TestRouter_BedrockHeaderUsageFallback(t *testing.T) {
	body := `{"outputs":[{"text":"ok","stop_reason":"length"}]}`
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Amzn-Bedrock-Input-Token-Count", "11")
		w.Header().Set("X-Amzn-Bedrock-Output-Token-Count", "10")
		_, _ = io.WriteString(w, body)
	}
	rec := &lastRec{}
	gw := bedrockRouter(t, h, rec)
	do(t, "POST", gw.URL+"/bedrock/model/mistral.mistral-7b-instruct-v0:2/invoke", nil, `{}`)
	if c := rec.wait(t); c.InputTokens != 11 || c.OutputTokens != 10 {
		t.Errorf("mistral: tokens %d/%d, want 11/10 from headers", c.InputTokens, c.OutputTokens)
	}
	rec.last = nil
	body = `{"id":"x","stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":4}}`
	do(t, "POST", gw.URL+"/bedrock/model/anthropic.claude-haiku-4-5-20251001-v1:0/invoke", nil, `{}`)
	if c := rec.wait(t); c.InputTokens != 9 || c.OutputTokens != 4 {
		t.Errorf("claude: tokens %d/%d, want body's 9/4", c.InputTokens, c.OutputTokens)
	}
}

func TestScanBedrockUsage_InvocationMetricsFallback(t *testing.T) {
	stream := `{"outputs":[{"text":"hi"}]} {"outputs":[{"text":"!"}],"amazon-bedrock-invocationMetrics":{"inputTokenCount":11,"outputTokenCount":10,"invocationLatency":400,"firstByteLatency":100}}`
	if u := scanBedrockUsage([]byte(stream)); u.InputTokens != 11 || u.OutputTokens != 10 {
		t.Errorf("tokens %d/%d, want 11/10", u.InputTokens, u.OutputTokens)
	}
	// Claude's own counts win over the metrics block.
	claude := `{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}} {"amazon-bedrock-invocationMetrics":{"inputTokenCount":999,"outputTokenCount":999}} {"type":"message_delta","usage":{"output_tokens":7}}`
	if u := scanBedrockUsage([]byte(claude)); u.InputTokens != 5 || u.OutputTokens != 7 {
		t.Errorf("claude tokens %d/%d, want 5/7", u.InputTokens, u.OutputTokens)
	}
}

// The response body is stored only with store_bodies on (usage is parsed either
// way). The storage buffer itself is not allocated when off (router.ServeHTTP).
func TestRouter_ResponseBodyStoredOnlyWhenStoring(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	for _, store := range []bool{false, true} {
		rec := &lastRec{}
		gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: store, MaxCaptureRequestBytes: 100, MaxCaptureResponseBytes: 100}, rec)
		do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"x"}`)
		c := rec.wait(t)
		if got := len(c.ResponseBody) > 0; got != store || c.InputTokens != 1 {
			t.Errorf("store=%v: captured body=%v tokens=%d", store, got, c.InputTokens)
		}
	}
}

// An upstream redirect is relayed to the client, never followed with the
// caller's provider key.
func TestRouter_RedirectRelayedNotFollowed(t *testing.T) {
	var stolen atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { stolen.Store(true) }))
	defer other.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	gw := gateway(t, Config{UpstreamBaseURL: up.URL}, &lastRec{})
	resp, _ := do(t, "POST", gw.URL+"/anthropic/v1/messages", map[string]string{"x-api-key": "sk-ant-SECRET"}, `{"model":"x"}`)
	if resp.StatusCode != http.StatusTemporaryRedirect || stolen.Load() {
		t.Errorf("status %d, redirect target reached=%v", resp.StatusCode, stolen.Load())
	}
}

func TestRouter_RelayErrorLabels(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 50 && r.Context().Err() == nil; i++ {
			_, _ = io.WriteString(w, "data: {}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Run("stream deadline", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(slow))
		defer up.Close()
		rec := &lastRec{}
		gw := gateway(t, Config{UpstreamBaseURL: up.URL, MaxStreamDuration: 150 * time.Millisecond}, rec)
		do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"x","stream":true}`)
		if e := rec.wait(t).Error; e != "stream_deadline" {
			t.Errorf("error = %q, want stream_deadline", e)
		}
	})
	t.Run("deadline before response headers", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body) // the server notices the gateway hanging up only after the body is read
			select {                  // a slow non-stream upstream that never answers in time
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer up.Close()
		rec := &lastRec{}
		gw := gateway(t, Config{UpstreamBaseURL: up.URL, MaxStreamDuration: 150 * time.Millisecond}, rec)
		resp, _ := do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"x"}`)
		if c := rec.wait(t); resp.StatusCode != http.StatusGatewayTimeout || c.Error != "stream_deadline" || c.StatusCode != http.StatusGatewayTimeout {
			t.Errorf("status %d, recorded %d %q; want 504 stream_deadline", resp.StatusCode, c.StatusCode, c.Error)
		}
	})
	t.Run("upstream cut mid-body", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, buf, _ := w.(http.Hijacker).Hijack()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n{\"partial\":")
			_ = buf.Flush()
			_ = conn.Close()
		}))
		defer up.Close()
		rec := &lastRec{}
		gw := gateway(t, Config{UpstreamBaseURL: up.URL}, rec)
		do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"x"}`)
		if e := rec.wait(t).Error; !strings.HasPrefix(e, "upstream_error: ") {
			t.Errorf("error = %q, want upstream_error", e)
		}
	})
	t.Run("client disconnect", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(slow))
		defer up.Close()
		rec := &lastRec{}
		gw := gateway(t, Config{UpstreamBaseURL: up.URL}, rec)
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"x","stream":true}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = bufio.NewReader(resp.Body).ReadString('\n')
		cancel()
		resp.Body.Close()
		if e := rec.wait(t).Error; e != "client_closed" {
			t.Errorf("error = %q, want client_closed", e)
		}
	})
}

const openaiStream = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":100,\"prompt_tokens_details\":{\"cached_tokens\":200}}}\n\n" +
	"data: [DONE]\n\n"

// A Chat Completions stream without stream_options gets include_usage upstream;
// the client's stream has the usage-only chunk removed, and the call is priced.
func TestOpenAI_InjectStreamUsage(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range strings.SplitAfter(openaiStream, "\n\n") {
			_, _ = io.WriteString(w, ev)
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()

	cases := []struct {
		name, body    string
		inject        bool
		wantInjected  bool
		wantUsageSeen bool
	}{
		{"injected and stripped", `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true, true, false},
		{"client asked for usage", `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true}}`, true, false, true},
		{"injection disabled", `{"model":"gpt-4o","stream":true}`, false, false, true},
		{"non-stream untouched", `{"model":"gpt-4o"}`, true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &lastRec{}
			gw := gateway(t, Config{OpenAIEnabled: true, OpenAIBaseURL: up.URL, OpenAIStreamUsage: c.inject,
				StoreBodies: true, MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: 1 << 20}, rec)
			_, got := do(t, "POST", gw.URL+"/openai/v1/chat/completions", nil, c.body)
			if injected := strings.HasPrefix(gotBody, `{"stream_options":{"include_usage":true},`); injected != c.wantInjected {
				t.Errorf("upstream body %s", gotBody)
			}
			if c.wantInjected && strings.TrimPrefix(gotBody, `{"stream_options":{"include_usage":true},`) != strings.TrimPrefix(c.body, "{") {
				t.Errorf("rest of body changed: %s", gotBody)
			}
			if seen := strings.Contains(got, `"prompt_tokens"`); seen != c.wantUsageSeen {
				t.Errorf("client saw usage chunk = %v; stream: %q", seen, got)
			}
			if !strings.Contains(got, `"content":"hi"`) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
				t.Errorf("client stream damaged: %q", got)
			}
			rc := rec.wait(t)
			if string(rc.RequestBody) != c.body { // capture keeps the client's own body, never the injected one
				t.Errorf("captured request body %s, want %s", rc.RequestBody, c.body)
			}
			if rc.InputTokens != 1000 || rc.OutputTokens != 100 || rc.StopReason != "stop" {
				t.Errorf("tokens %d/%d stop %q", rc.InputTokens, rc.OutputTokens, rc.StopReason)
			}
			want := (800*2.50 + 200*1.25 + 100*10.0) / 1e6
			if rc.CostUSD == nil || !near(*rc.CostUSD, want) {
				t.Errorf("cost %v, want %v", rc.CostUSD, want)
			}
		})
	}
}

func TestUsageChunkStripper_SplitWritesAndCRLF(t *testing.T) {
	for _, stream := range []string{openaiStream, strings.ReplaceAll(openaiStream, "\n\n", "\r\n\r\n")} {
		var out strings.Builder
		s := &usageChunkStripper{w: &out}
		for i := 0; i < len(stream); i++ { // one byte per write
			if _, err := s.Write([]byte{stream[i]}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if strings.Contains(got, "prompt_tokens") || strings.Count(got, "data: ") != 3 {
			t.Errorf("stripped stream = %q", got)
		}
	}
	// Events are forwarded as soon as they are complete, not held to the end.
	var out strings.Builder
	s := &usageChunkStripper{w: &out}
	_, _ = s.Write([]byte("data: {\"choices\":[{}]}\n\ndata: {\"cho"))
	if out.String() != "data: {\"choices\":[{}]}\n\n" {
		t.Errorf("complete event not forwarded: %q", out.String())
	}
}

func TestProviderRequestID(t *testing.T) {
	for h, want := range map[string]string{"request-id": "a", "x-amzn-requestid": "b", "x-request-id": "c"} {
		hdr := http.Header{}
		hdr.Set(h, want)
		if got := providerRequestID(hdr); got != want {
			t.Errorf("%s: got %q", h, got)
		}
	}
}

func TestPermissionLLMAccess_BuiltinRoles(t *testing.T) {
	z := pkgauth.NewRoleAuthorizer()
	z.Rules["viewer"] = []string{"*.read"}
	ctx := context.Background()
	for role, want := range map[string]bool{"admin": true, "agent": true, "viewer": false} {
		if got := z.Allow(ctx, &pkgauth.Principal{Roles: []string{role}}, PermissionLLMAccess); got != want {
			t.Errorf("%s: Allow(llm.access) = %v, want %v", role, got, want)
		}
	}
}

func TestParseUsage_AnthropicLargeBodyKeepsStopReason(t *testing.T) {
	tail := `aaaa"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":4000}}`
	if u := (anthropicProvider{}).ParseUsage([]byte(tail)); u.StopReason != "max_tokens" || u.OutputTokens != 4000 {
		t.Errorf("usage = %+v", u)
	}
}

func TestParseUsage_BedrockCacheShapes(t *testing.T) {
	p := &bedrockProvider{}
	for name, c := range map[string]struct {
		body         string
		cr, cw, cw1h int64
	}{
		"converse 1h cacheDetails": {`{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":0,"cacheWriteInputTokens":3000,"cacheDetails":[{"ttl":"1h","inputTokens":3000}]}}`, 0, 3000, 3000},
		"converse 5m only":         {`{"usage":{"inputTokens":10,"outputTokens":5,"cacheWriteInputTokens":3000,"cacheDetails":[{"inputTokens":3000,"ttl":"5m"}]}}`, 0, 3000, 0},
		"nova invoke TokenCount":   {`{"output":{},"usage":{"inputTokens":100,"outputTokens":20,"cacheReadInputTokenCount":5000,"cacheWriteInputTokenCount":7}}`, 5000, 7, 0},
		"converse-stream metadata": {"\x00\x00:event-type metadata{\"usage\":{\"inputTokens\":10,\"outputTokens\":5,\"cacheWriteInputTokens\":3000,\"cacheDetails\":[{\"inputTokens\":3000,\"ttl\":\"1h\"}]},\"metrics\":{}}\x00", 0, 3000, 3000},
		"nova stream metrics":      {`{"contentBlockDelta":{}} {"metadata":{"usage":{"inputTokens":100,"outputTokens":20}},"amazon-bedrock-invocationMetrics":{"inputTokenCount":100,"outputTokenCount":20,"cacheReadInputTokenCount":5000}}`, 5000, 0, 0},
	} {
		u := p.ParseUsage([]byte(c.body))
		if u.CacheReadTokens != c.cr || u.CacheCreationTokens != c.cw || u.CacheCreation1hTokens != c.cw1h {
			t.Errorf("%s: cache r/w/1h = %d/%d/%d, want %d/%d/%d", name, u.CacheReadTokens, u.CacheCreationTokens, u.CacheCreation1hTokens, c.cr, c.cw, c.cw1h)
		}
	}
}

// Every Usage field must survive mergeUsage: a field added to Usage but not to
// mergeUsage is silently lost before pricing (how the 1h-cache and web-search
// counts were once dropped).
func TestMergeUsage_KeepsEveryField(t *testing.T) {
	var full Usage
	v := reflect.ValueOf(&full).Elem()
	for i := 0; i < v.NumField(); i++ {
		switch f := v.Field(i); f.Kind() {
		case reflect.Int64:
			f.SetInt(int64(i + 1))
		case reflect.String:
			f.SetString("x")
		default:
			t.Fatalf("Usage.%s: unhandled kind %s", v.Type().Field(i).Name, f.Kind())
		}
	}
	if got := mergeUsage(full, Usage{}); got != full {
		t.Errorf("mergeUsage(full, zero) = %+v, want %+v", got, full)
	}
	if got := mergeUsage(Usage{}, full); got != full {
		t.Errorf("mergeUsage(zero, full) = %+v, want %+v", got, full)
	}
}

func TestParseUsage_PricingContext(t *testing.T) {
	a := anthropicProvider{}
	if u := a.ParseUsage([]byte(`{"usage":{"input_tokens":5,"output_tokens":2,"speed":"fast","inference_geo":"us"}}`)); u.Speed != "fast" || u.InferenceGeo != "us" {
		t.Errorf("anthropic: %+v", u)
	}
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"inference_geo\":\"us\"}}}\n\n"
	if u := a.ParseUsage([]byte(sse)); u.InferenceGeo != "us" {
		t.Errorf("anthropic sse: %+v", u)
	}
	o := openaiProvider{}
	for name, body := range map[string]string{
		"chat":      `{"service_tier":"priority","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		"stream":    "data: {\"service_tier\":\"priority\",\"choices\":[{\"delta\":{}}]}\n\ndata: {\"service_tier\":\"priority\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n",
		"responses": "data: {\"type\":\"response.completed\",\"response\":{\"service_tier\":\"priority\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n",
	} {
		if u := o.ParseUsage([]byte(body)); u.ServiceTier != "priority" || u.InputTokens != 5 {
			t.Errorf("openai %s: %+v", name, u)
		}
	}
	if tier, _, _ := requestHints([]byte(`{"contents":[],"serviceTier":"flex"}`)); tier != "flex" {
		t.Errorf("gemini tier hint = %q", tier)
	}
	if _, speed, _ := requestHints([]byte(`{"model":"claude-opus-5-5","speed":"fast"}`)); speed != "fast" {
		t.Errorf("anthropic speed hint = %q", speed)
	}
	if _, _, geo := requestHints([]byte(`{"model":"claude-sonnet-5","inference_geo":"us"}`)); geo != "us" {
		t.Errorf("anthropic inference_geo hint = %q", geo)
	}
}

// inference_geo falls back to the request body, like service_tier and speed: a
// response that does not echo it must still bill the us multiplier the caller
// asked and will be invoiced for. (docs/llm-plane.md promised this fallback
// before requestHints parsed the field.)
func TestRouter_InferenceGeoFromRequestBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1000,"output_tokens":100}}`) // no inference_geo echoed
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL}, rec)
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-sonnet-5","inference_geo":"us"}`)
	if c := rec.wait(t); c.CostUSD == nil || !near(*c.CostUSD, 1.1*(1000*2.0+100*10.0)/1e6) {
		t.Errorf("cost = %v, want the 1.1x us_geo_multiplier applied from the request body", c.CostUSD)
	}
}

// A streamed 2xx that declares a Content-Length is relayed verbatim: dropping
// the usage chunk would make the body shorter than the length the upstream
// promised, so the stripper is not installed at all.
func TestUsageChunkStripper_NotInstalledWithContentLength(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(openaiStream)))
		_, _ = io.WriteString(w, openaiStream)
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{OpenAIEnabled: true, OpenAIBaseURL: up.URL, OpenAIStreamUsage: true}, rec)
	_, body := do(t, "POST", gw.URL+"/openai/v1/chat/completions", nil, `{"model":"gpt-4o","stream":true}`)
	if body != openaiStream {
		t.Errorf("body was rewritten under a Content-Length: %q", body)
	}
	if c := rec.wait(t); c.InputTokens != 1000 || c.OutputTokens != 100 {
		t.Errorf("usage still parsed? tokens %d/%d, want 1000/100", c.InputTokens, c.OutputTokens)
	}
}

// Bedrock stream metadata repeats the same cumulative cacheDetails totals, so a
// buffer holding the metadata event twice must not double the 1h cache write.
func TestScanBedrockUsage_CacheDetails1hNotAccumulated(t *testing.T) {
	event := `{"usage":{"inputTokens":10,"outputTokens":5,"cacheWriteInputTokens":3000,"cacheDetails":[{"ttl":"1h","inputTokens":3000}]},"metrics":{}}`
	if u := scanBedrockUsage([]byte(event + " " + event)); u.CacheCreation1hTokens != 3000 {
		t.Errorf("1h cache = %d, want 3000 (the repeated event must not double it)", u.CacheCreation1hTokens)
	}
}

// The pricing context reaches the cost end to end through the router.
func TestRouter_PricingContextReachesCost(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1beta") {
			_, _ = io.WriteString(w, `{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100}}`)
			return
		}
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1000,"output_tokens":100,"inference_geo":"us"}}`)
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, GeminiEnabled: true, GeminiBaseURL: up.URL}, rec)
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-sonnet-5"}`)
	if c := rec.wait(t); c.CostUSD == nil || !near(*c.CostUSD, 1.1*(1000*2.0+100*10.0)/1e6) {
		t.Errorf("anthropic us geo cost = %v", c.CostUSD)
	}
	rec.last = nil
	do(t, "POST", gw.URL+"/gemini/v1beta/models/gemini-3.8-flash:generateContent", nil, `{"serviceTier":"flex"}`)
	if c := rec.wait(t); c.CostUSD == nil || !near(*c.CostUSD, (1000*0.375+100*1.875)/1e6) {
		t.Errorf("gemini flex cost = %v", c.CostUSD)
	}
}

func TestBedrockRegionOf(t *testing.T) {
	resp := &http.Response{Request: httptest.NewRequest("POST", "https://bedrock-runtime.us-gov-west-1.amazonaws.com/model/m/converse", nil)}
	if got := bedrockRegionOf("bedrock", resp); got != "us-gov-west-1" {
		t.Errorf("region = %q", got)
	}
	if got := bedrockRegionOf("openai", resp); got != "" {
		t.Errorf("non-bedrock region = %q", got)
	}
}

// The plane enforces llm.access itself: a viewer key (no llm.*) is 403; agent
// gets through. Reverting the wiring fails this test.
func TestPlane_RequiresLLMAccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer up.Close()
	z := pkgauth.NewRoleAuthorizer()
	z.Rules["viewer"] = []string{"*.read"}
	h, err := Handler(Config{UpstreamBaseURL: up.URL, Authorizer: z}, &lastRec{})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(roles ...string) int {
		req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(`{"model":"x"}`))
		req = req.WithContext(pkgauth.WithPrincipal(req.Context(), &pkgauth.Principal{TenantID: "t", Roles: roles}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Result().StatusCode
	}
	if got := serve("viewer"); got != http.StatusForbidden {
		t.Errorf("viewer status = %d, want 403", got)
	}
	if got := serve("agent"); got != http.StatusOK {
		t.Errorf("agent status = %d, want 200", got)
	}
}

// The plane never runs without an authorizer: Handler refuses to build one, and
// the permission middleware fails CLOSED if a chain is assembled by hand
// anyway. Both halves matter — a nil Authorizer used to wave every caller
// through the llm.access check.
func TestPlane_NilAuthorizerDenies(t *testing.T) {
	if _, err := Handler(Config{UpstreamBaseURL: "http://127.0.0.1:1"}, &lastRec{}); err == nil {
		t.Error("Handler with no Authorizer built successfully, want an error")
	}
	reached := false
	h := requirePermission(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(pkgauth.WithPrincipal(req.Context(), &pkgauth.Principal{TenantID: "t", Roles: []string{"admin"}}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusForbidden || reached {
		t.Errorf("nil authorizer: status = %d, handler reached = %v; want 403 and false", w.Result().StatusCode, reached)
	}
}

// captureTargets is the router's seam for the storage tee; reverting the
// storeBodies guard would make it return a non-nil bufferized target.
func TestCaptureTargets(t *testing.T) {
	base := []io.Writer{io.Discard}
	if dst, cap := captureTargets(false, 100, base...); cap != nil || len(dst) != 1 {
		t.Errorf("store=false: cap=%v len(dst)=%d, want nil and 1", cap, len(dst))
	}
	if dst, cap := captureTargets(true, 100, base...); cap == nil || len(dst) != 2 {
		t.Errorf("store=true: cap=%v len(dst)=%d, want non-nil and 2", cap, len(dst))
	}
}

func TestGemini_ServiceTierFromTrafficType(t *testing.T) {
	g := geminiProvider{}
	for tt, want := range map[string]string{"ON_DEMAND": "", "ON_DEMAND_PRIORITY": "priority", "PRIORITY": "priority", "FLEX": "flex", "ON_DEMAND_FLEX": "flex", "": ""} {
		body := `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"trafficType":"` + tt + `"}}`
		if u := g.ParseUsage([]byte(body)); u.ServiceTier != want {
			t.Errorf("trafficType %q: tier %q, want %q", tt, u.ServiceTier, want)
		}
	}
}

// A non-SSE 2xx to an injected request is passed through (not buffered).
func TestUsageChunkStripper_NotBufferedForNonSSE(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json") // upstream ignored the stream flag
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	rec := &lastRec{}
	gw := gateway(t, Config{OpenAIEnabled: true, OpenAIBaseURL: up.URL, OpenAIStreamUsage: true}, rec)
	_, body := do(t, "POST", gw.URL+"/openai/v1/chat/completions", nil, `{"model":"gpt-4o-mini","stream":true}`)
	if !strings.Contains(body, `"prompt_tokens":1`) {
		t.Errorf("non-SSE body swallowed: %q", body)
	}
}

// The stripper falls back to passthrough when the response has no event
// terminator inside usageScanBytes: bytes reach the client, the buffer is bounded.
func TestUsageChunkStripper_PassthroughOnUnterminated(t *testing.T) {
	var out strings.Builder
	s := &usageChunkStripper{w: &out}
	junk := strings.Repeat("a", usageScanBytes+8)
	if _, err := s.Write([]byte(junk)); err != nil {
		t.Fatal(err)
	}
	if !s.passthr || len(s.buf) != 0 {
		t.Fatalf("passthr=%v buf=%d", s.passthr, len(s.buf))
	}
	// Further writes flow through, and the accumulated bytes have already reached w.
	if _, err := s.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != junk+"more" {
		t.Errorf("output length %d, want %d", len(got), len(junk)+len("more"))
	}
}

// Provider response metadata (incl. the rate-limit family) is stored; credentials never are.
func TestHeaderAllowed(t *testing.T) {
	for name, want := range map[string]bool{
		"x-ratelimit-reset-tokens": true, "anthropic-ratelimit-requests-limit": true, "cf-ray": true,
		"authorization": false, "x-api-key": false, "x-gateway-key": false, "set-cookie": false, "x-bedrock-secret-access-key": false,
	} {
		if got := headerAllowed(name); got != want {
			t.Errorf("headerAllowed(%q) = %v, want %v", name, got, want)
		}
	}
}
