//go:build e2e

package e2e

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/gateway"
)

// runTaggedCall starts gateway.Run with the built-in Postgres capture store in
// front of a recording upstream, sends one call carrying the attribution
// headers, and returns the headers the upstream saw plus the captured row's
// user and tags read straight from Postgres.
func runTaggedCall(t *testing.T, a runAddrs, hdr map[string]string) (upstreamSaw http.Header, user, tags string) {
	t.Helper()
	var mu sync.Mutex
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_tags","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("GATEWAY_LLM_PROXY_UPSTREAM_BASE_URL", upstream.URL)
	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "127.0.0.0/8")

	tenant := uuid.NewString()
	startRun(t, a, gateway.Options{Authenticators: []pkgauth.Authenticator{headerAuth{tenant: tenant, keyID: "key-tags"}}})

	req, _ := http.NewRequest("POST", "http://"+a.llm+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Run-Test", "1")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("llm call: status %d", resp.StatusCode)
	}

	pw := os.Getenv("GATEWAY_DATABASE_PASSWORD")
	dsn := (&url.URL{Scheme: "postgres", User: url.UserPassword(os.Getenv("GATEWAY_DATABASE_USER"), pw),
		Host: os.Getenv("GATEWAY_DATABASE_HOST") + ":" + os.Getenv("GATEWAY_DATABASE_PORT"),
		Path: os.Getenv("GATEWAY_DATABASE_DATABASE"), RawQuery: "sslmode=" + os.Getenv("GATEWAY_DATABASE_SSL_MODE")}).String()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := db.QueryRow(`SELECT "user", tags::text FROM llm_calls WHERE tenant_id = $1`, tenant).Scan(&user, &tags)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured row never appeared: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return seen, user, tags
}

// X-Gateway-User and X-Gateway-Tags reach the Postgres llm_calls row through
// the real gateway.Run wiring (config defaults, middleware chain, capture
// store), are never sent upstream, and invalid pairs are dropped without
// failing the call.
func TestRequestTagsCapturedAndNotForwarded(t *testing.T) {
	a := setupRunEnv(t)
	saw, user, tags := runTaggedCall(t, a, map[string]string{
		"X-Gateway-User": "alice@example.com",
		"X-Gateway-Tags": "team=search,run_id=r%2F1,Bad=x,novalue",
	})
	if user != "alice@example.com" {
		t.Errorf("stored user = %q", user)
	}
	if tags != `{"team": "search", "run_id": "r/1"}` && tags != `{"run_id": "r/1", "team": "search"}` {
		t.Errorf("stored tags = %s, want team and run_id only", tags)
	}
	for _, h := range []string{"X-Gateway-User", "X-Gateway-Tags"} {
		if v := saw.Get(h); v != "" {
			t.Errorf("%s was forwarded upstream: %q", h, v)
		}
	}
}

// llm_proxy.capture.tags.enabled=false (env override): nothing is stored, the
// headers are still not forwarded.
func TestRequestTagsDisabledByConfig(t *testing.T) {
	a := setupRunEnv(t)
	t.Setenv("GATEWAY_LLM_PROXY_CAPTURE_TAGS_ENABLED", "false")
	saw, user, tags := runTaggedCall(t, a, map[string]string{"X-Gateway-User": "alice", "X-Gateway-Tags": "team=search"})
	if user != "" || tags != "{}" {
		t.Errorf("stored user=%q tags=%s with tags disabled", user, tags)
	}
	if saw.Get("X-Gateway-User") != "" || saw.Get("X-Gateway-Tags") != "" {
		t.Errorf("headers forwarded upstream: %v", saw)
	}
}
