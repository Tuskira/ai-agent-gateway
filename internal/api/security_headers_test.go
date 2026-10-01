package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// TestSecurityHeaders_OnEveryResponse asserts that every response class
// this router serves -- an unauthenticated JSON route, the embedded admin
// console (mounted at "/"), and the public docs page -- carries the fixed
// browser-hardening headers securityHeaders sets, and that each gets the
// right Content-Security-Policy: the docs page overrides the default (see
// handlers.Docs.Page / cspDocs), everything else keeps cspDefault.
func TestSecurityHeaders_OnEveryResponse(t *testing.T) {
	uiHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<div id="root"></div>`))
	})

	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &fakeAuthenticator{err: pkgauth.ErrNoCredential},
		ServeUI:        true,
		UIHandler:      uiHandler,
	})

	cases := []struct {
		name       string
		path       string
		defaultCSP bool // true: must equal cspDefault exactly
		cdnAllowed bool // true: must reference jsdelivr and NOT equal cspDefault
	}{
		{name: "health (public API route)", path: "/api/v1/health", defaultCSP: true},
		{name: "UI root (admin console)", path: "/", defaultCSP: true},
		{name: "docs page", path: "/api/v1/docs", cdnAllowed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}

			hdr := w.Header()
			if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := hdr.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want no-referrer", got)
			}
			if got := hdr.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want DENY", got)
			}
			if got := hdr.Get("Permissions-Policy"); got != "camera=(), microphone=(), geolocation=()" {
				t.Errorf("Permissions-Policy = %q, want camera=(), microphone=(), geolocation=()", got)
			}

			csp := hdr.Get("Content-Security-Policy")
			if csp == "" {
				t.Fatal("Content-Security-Policy is empty")
			}
			if tc.defaultCSP && csp != cspDefault {
				t.Errorf("Content-Security-Policy = %q, want cspDefault %q", csp, cspDefault)
			}
			if tc.cdnAllowed {
				if csp == cspDefault {
					t.Error("docs page CSP equals cspDefault -- it should override with a jsDelivr allowance")
				}
				if !strings.Contains(csp, "https://cdn.jsdelivr.net") {
					t.Errorf("docs page Content-Security-Policy = %q, want it to allow https://cdn.jsdelivr.net", csp)
				}
			}
		})
	}
}
