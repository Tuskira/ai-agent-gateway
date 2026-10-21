package llmplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gwauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// stubAuth stands in for an authenticator other than the API-key one: it
// accepts a token that does not start with gk_, either as
// "Authorization: Bearer corp.<...>" or bare in X-Corp-Token, and keeps it as
// RawCredential. This is a unit test of header handling, so the stub lives
// here; the upstream is a real HTTP server and the plane is the real Handler.
type stubAuth struct{}

func (stubAuth) Name() string { return "stub" }

func (stubAuth) Authenticate(_ context.Context, r *http.Request) (*pkgauth.Principal, error) {
	tok := r.Header.Get("X-Corp-Token")
	if tok == "" {
		// RFC 7235: the scheme is case-insensitive.
		if scheme, b, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") && strings.HasPrefix(b, "corp.") {
			tok = b
		}
	}
	if tok == "" {
		return nil, pkgauth.ErrNoCredential
	}
	return &pkgauth.Principal{Subject: "u1", TenantID: "t", Roles: []string{"agent"}, AuthMethod: "stub", RawCredential: tok}, nil
}

// headerSink is an upstream that records the headers it received.
type headerSink struct {
	mu   sync.Mutex
	last http.Header
}

func (s *headerSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.last = r.Header.Clone()
	s.mu.Unlock()
	_, _ = io.WriteString(w, `{"id":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
}

func (s *headerSink) got(t *testing.T) http.Header {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		t.Fatal("the upstream received no request")
	}
	return s.last
}

// authedPlane serves the real plane (all passthrough providers pointed at one
// recording upstream) behind the real auth middleware and a stub authenticator.
func authedPlane(t *testing.T) (*httptest.Server, *headerSink) {
	t.Helper()
	sink := &headerSink{}
	up := httptest.NewServer(sink)
	t.Cleanup(up.Close)
	h, err := Handler(Config{
		UpstreamBaseURL: up.URL, OpenAIEnabled: true, OpenAIBaseURL: up.URL, GeminiEnabled: true, GeminiBaseURL: up.URL,
		Authorizer: pkgauth.NewRoleAuthorizer(),
	}, &lastRec{})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(gwauth.Middleware(stubAuth{}, nil)(h))
	t.Cleanup(gw.Close)
	return gw, sink
}

var passthroughPaths = map[string]string{
	"anthropic": "/anthropic/v1/messages",
	"openai":    "/openai/v1/chat/completions",
	"gemini":    "/gemini/v1beta/models/gemini-pro:generateContent",
}

// A bearer accepted by an authenticator other than the API-key one (here a
// non-gk_ token) is the gateway's credential: it must never reach a
// passthrough upstream, whichever header carried it.
func TestPassthrough_StripsNonGKCredential(t *testing.T) {
	const cred = "corp.abc123"
	cases := []struct {
		name string
		hdr  map[string]string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer " + cred}},
		{"bearer other case", map[string]string{"Authorization": "bearer " + cred}},
		{"custom header", map[string]string{"X-Corp-Token": cred}},
		{"custom header and a repeat in Authorization", map[string]string{"X-Corp-Token": cred, "Authorization": "Bearer " + cred}},
		{"repeat in a vendor key header", map[string]string{"X-Corp-Token": cred, "X-Api-Key": cred, "X-Goog-Api-Key": cred}},
	}
	for prov, path := range passthroughPaths {
		for _, c := range cases {
			t.Run(prov+"/"+c.name, func(t *testing.T) {
				gw, sink := authedPlane(t)
				resp, body := do(t, "POST", gw.URL+path, c.hdr, `{"model":"m"}`)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d: %s", resp.StatusCode, body)
				}
				for name, vals := range sink.got(t) {
					for _, v := range vals {
						if strings.Contains(v, cred) {
							t.Errorf("the gateway credential reached the upstream in %s: %q", name, v)
						}
					}
				}
			})
		}
	}
}

// The provider key rides along beside the gateway credential: only the value
// that carries the credential is removed.
func TestPassthrough_ForwardsProviderKeyBesideCredential(t *testing.T) {
	const cred = "corp.abc123"
	cases := []struct {
		prov string
		hdr  map[string]string
		name string // upstream header expected to hold the provider key
		want string
	}{
		{"anthropic", map[string]string{"X-Corp-Token": cred, "X-Api-Key": "sk-ant-byok"}, "X-Api-Key", "sk-ant-byok"},
		{"anthropic", map[string]string{"Authorization": "Bearer " + cred, "X-Api-Key": "sk-ant-byok"}, "X-Api-Key", "sk-ant-byok"},
		{"openai", map[string]string{"X-Corp-Token": cred, "Authorization": "Bearer sk-openai-byok"}, "Authorization", "Bearer sk-openai-byok"},
		{"gemini", map[string]string{"Authorization": "Bearer " + cred, "X-Goog-Api-Key": "AIza-byok"}, "X-Goog-Api-Key", "AIza-byok"},
		// a provider key that merely contains the credential text is not the credential
		{"anthropic", map[string]string{"X-Corp-Token": cred, "X-Api-Key": "sk-ant-" + cred + "-x"}, "X-Api-Key", "sk-ant-" + cred + "-x"},
	}
	for _, c := range cases {
		t.Run(c.prov+"/"+c.name+"/"+c.want, func(t *testing.T) {
			gw, sink := authedPlane(t)
			if resp, body := do(t, "POST", gw.URL+passthroughPaths[c.prov], c.hdr, `{"model":"m"}`); resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.StatusCode, body)
			}
			if got := sink.got(t).Get(c.name); got != c.want {
				t.Errorf("upstream %s = %q, want %q", c.name, got, c.want)
			}
			if got := sink.got(t).Get("X-Corp-Token"); got != "" {
				t.Errorf("X-Corp-Token reached the upstream: %q", got)
			}
		})
	}
}

// The API-key case keeps working: gk_ in Authorization or X-Gateway-Key
// never reaches the upstream, with or without a Principal naming it.
func TestPassthrough_StripsGatewayKey(t *testing.T) {
	const key = "gk_ABCdef123"
	for _, hdr := range []map[string]string{
		{"Authorization": "Bearer " + key},
		{"X-Gateway-Key": key},
		{"X-Gateway-Key": key, "X-Api-Key": "sk-ant-byok"},
	} {
		sink := &headerSink{}
		up := httptest.NewServer(sink)
		gw := gateway(t, Config{UpstreamBaseURL: up.URL}, &lastRec{})
		do(t, "POST", gw.URL+passthroughPaths["anthropic"], hdr, `{"model":"m"}`)
		for name, vals := range sink.got(t) {
			for _, v := range vals {
				if strings.Contains(v, key) {
					t.Errorf("%v: the gateway key reached the upstream in %s: %q", hdr, name, v)
				}
			}
		}
		if _, ok := hdr["X-Api-Key"]; ok && sink.got(t).Get("X-Api-Key") != "sk-ant-byok" {
			t.Errorf("%v: the BYOK key was not forwarded", hdr)
		}
		up.Close()
	}
}

// The registry builders share newUpstream; it strips by Principal too.
func TestNewUpstream_StripsPrincipalCredential(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer corp.abc123")
	r.Header.Set("X-Api-Key", "sk-ant-byok")
	r = r.WithContext(pkgauth.WithPrincipal(r.Context(), &pkgauth.Principal{AuthMethod: "stub", RawCredential: "corp.abc123"}))
	up, err := newUpstream(r.Context(), r, nil, "https://x.test", "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := up.Header["Authorization"]; sent {
		t.Errorf("the gateway credential reached the upstream: %q", up.Header.Get("Authorization"))
	}
	if up.Header.Get("X-Api-Key") != "sk-ant-byok" {
		t.Errorf("X-Api-Key = %q, want the BYOK key", up.Header.Get("X-Api-Key"))
	}
}

// Bedrock signed passthrough (Flow A) keeps the client's SigV4 Authorization
// and X-Amz-* headers; only the gateway credential beside them goes.
func TestBedrockPassthrough_KeepsSignatureDropsCredential(t *testing.T) {
	const sig = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261019/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc"
	for _, hdr := range []map[string]string{
		{"X-Gateway-Key": "gk_ABCdef123"},
		{"X-Corp-Token": "corp.abc123"},
	} {
		r := httptest.NewRequest("POST", "/model/anthropic.claude/invoke", nil)
		r.Header.Set("Authorization", sig)
		r.Header.Set("X-Amz-Date", "20261019T000000Z")
		cred := ""
		for k, v := range hdr {
			r.Header.Set(k, v)
			cred = v
		}
		r = r.WithContext(pkgauth.WithPrincipal(r.Context(), &pkgauth.Principal{AuthMethod: "stub", RawCredential: cred}))
		up, err := newBedrockProvider("us-east-1", "", "").BuildUpstream(r.Context(), r, []byte(`{}`), "/model/anthropic.claude/invoke")
		if err != nil {
			t.Fatal(err)
		}
		if up.Header.Get("Authorization") != sig || up.Header.Get("X-Amz-Date") != "20261019T000000Z" {
			t.Errorf("%v: the signed headers were altered: %v", hdr, up.Header)
		}
		for name, vals := range up.Header {
			for _, v := range vals {
				if strings.Contains(v, cred) {
					t.Errorf("%v: the gateway credential reached the upstream in %s", hdr, name)
				}
			}
		}
	}
}

func TestCarriesCredential(t *testing.T) {
	for _, c := range []struct {
		v, cred string
		want    bool
	}{
		{"corp.abc", "corp.abc", true},
		{"Bearer corp.abc", "corp.abc", true},
		{"bearer corp.abc", "corp.abc", true},
		{"Token  corp.abc ", "corp.abc", true},
		{"Bearer corp.abcd", "corp.abc", false},
		{"Bearer x corp.abc", "corp.abc", false},
		{"sk-corp.abc", "corp.abc", false},
		{"Bearer sk-ant", "", false},
		{"", "", false},
	} {
		if got := carriesCredential(c.v, c.cred); got != c.want {
			t.Errorf("carriesCredential(%q, %q) = %v, want %v", c.v, c.cred, got, c.want)
		}
	}
}
