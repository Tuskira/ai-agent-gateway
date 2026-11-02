package llmplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// authAgent records the Authorization header of every request it gets, by
// "METHOD /path".
type authAgent struct {
	url string
	mu  sync.Mutex
	got map[string][]string
}

func newAuthAgent(t *testing.T) *authAgent {
	t.Helper()
	a := &authAgent{got: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		key := r.Method + " " + r.URL.Path
		a.mu.Lock()
		a.got[key] = append(a.got[key], r.Header.Get("Authorization"))
		a.mu.Unlock()
		switch key {
		case "GET /v1/policy":
			_ = json.NewEncoder(w).Encode(teePolicy{Inline: true})
		case "POST /v1/turns/request":
			_ = json.NewEncoder(w).Encode(teeVerdict{})
		case "POST /v1/turns":
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func (a *authAgent) wait(t *testing.T, key string) []string {
	t.Helper()
	for range 200 {
		a.mu.Lock()
		v := append([]string(nil), a.got[key]...)
		a.mu.Unlock()
		if len(v) > 0 {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s request reached the agent", key)
	return nil
}

// With agent_token set, the turn post, the policy lookup and the inline
// verdict request all carry it as a bearer token.
func TestDetectionTee_AgentTokenSentOnEveryRequest(t *testing.T) {
	agent := newAuthAgent(t)
	var upCalls atomic.Int32
	gw := gateway(t, Config{UpstreamBaseURL: countingUpstream(t, &upCalls),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, AgentToken: "s3cret-token", Inline: true})}, &lastRec{})
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	for _, key := range []string{"GET /v1/policy", "POST /v1/turns/request", "POST /v1/turns"} {
		for _, h := range agent.wait(t, key) {
			if h != "Bearer s3cret-token" {
				t.Errorf("%s: Authorization = %q, want the bearer token", key, h)
			}
		}
	}
}

// Without agent_token no Authorization header is sent to the agent.
func TestDetectionTee_NoTokenNoAuthorization(t *testing.T) {
	agent := newAuthAgent(t)
	var upCalls atomic.Int32
	gw := gateway(t, Config{UpstreamBaseURL: countingUpstream(t, &upCalls),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, Inline: true})}, &lastRec{})
	if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	for _, key := range []string{"GET /v1/policy", "POST /v1/turns/request", "POST /v1/turns"} {
		for _, h := range agent.wait(t, key) {
			if h != "" {
				t.Errorf("%s: Authorization = %q, want none", key, h)
			}
		}
	}
}
