package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func inboundStatus(t *testing.T, method, url, authorization, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// With InboundToken set, a request without the matching bearer token is
// refused with 401 before it reaches a handler; the matching token is let
// through; /healthz stays open.
func TestInboundToken(t *testing.T) {
	f := newFakeEngine(t)
	_, url := startAgent(t, f, Config{InboundToken: "gw-secret"})

	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/turns", `{}`},
		{http.MethodPost, "/v1/turns/request", `{}`},
		{http.MethodPost, "/v1/turns/response", `{}`},
		{http.MethodPost, "/v1/events", `{"events":[]}`},
		{http.MethodGet, "/v1/policy?tenant_id=t1", ``},
	}
	for _, rt := range routes {
		for name, authz := range map[string]string{
			"no header":       "",
			"wrong token":     "Bearer gw-wrong",
			"prefix of it":    "Bearer gw-secre",
			"extension of it": "Bearer gw-secret-and-more",
			"wrong scheme":    "Basic gw-secret",
			"bare token":      "gw-secret",
		} {
			if got := inboundStatus(t, rt.method, url+rt.path, authz, rt.body); got != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: status %d, want 401", rt.method, rt.path, name, got)
			}
		}
		if got := inboundStatus(t, rt.method, url+rt.path, "Bearer gw-secret", rt.body); got == http.StatusUnauthorized {
			t.Errorf("%s %s with the right token: 401", rt.method, rt.path)
		}
		if got := inboundStatus(t, rt.method, url+rt.path, "bearer gw-secret", rt.body); got == http.StatusUnauthorized {
			t.Errorf("%s %s with a lower-case scheme: 401", rt.method, rt.path)
		}
	}
	if got := inboundStatus(t, http.MethodPost, url+"/v1/events", "Bearer gw-secret", `{"events":[]}`); got != http.StatusAccepted {
		t.Errorf("events with the right token: %d, want 202", got)
	}
	if got := inboundStatus(t, http.MethodGet, url+"/healthz", "", ""); got != http.StatusOK {
		t.Errorf("/healthz without a token: %d, want 200", got)
	}
}

// A refused request never reaches a handler: the engine is not asked
// anything, and the refusal says how to authenticate.
func TestInboundTokenRefusalReachesNothing(t *testing.T) {
	f := newFakeEngine(t)
	a, url := startAgent(t, f, Config{InboundToken: "gw-secret"})
	if got := inboundStatus(t, http.MethodGet, url+"/v1/policy?tenant_id=t1", "", ""); got != http.StatusUnauthorized {
		t.Fatalf("policy without a token: %d", got)
	}
	if got := inboundStatus(t, http.MethodPost, url+"/v1/turns/request", "Bearer nope", `{"tenant_id":"t1"}`); got != http.StatusUnauthorized {
		t.Fatalf("verdict with a wrong token: %d", got)
	}
	f.mu.Lock()
	gets := f.policyGets
	f.mu.Unlock()
	if gets != 0 {
		t.Errorf("engine policy fetched %d times for refused requests", gets)
	}
	req, _ := http.NewRequest(http.MethodPost, "/v1/turns", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("refusal = %d %v", rec.Code, rec.Header())
	}
}

// Without InboundToken the agent serves everyone, as before.
func TestInboundTokenUnsetIsOpen(t *testing.T) {
	f := newFakeEngine(t)
	_, url := startAgent(t, f, Config{})
	if got := inboundStatus(t, http.MethodPost, url+"/v1/events", "", `{"events":[]}`); got != http.StatusAccepted {
		t.Errorf("events without a token and none required: %d, want 202", got)
	}
}
