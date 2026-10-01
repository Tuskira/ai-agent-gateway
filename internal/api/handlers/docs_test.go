package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDocs_Page_PinsScalarWithIntegrity guards the CSP-hardening fix: the
// docs page must load a specific, hashed @scalar/api-reference build --
// not "whatever jsDelivr's unversioned @latest resolves to today" -- and
// must set its own (wider) CSP rather than inherit the control plane's
// default. See scalarVersion/scalarIntegrity/cspDocs' doc comments in
// docs.go for how to keep this pin current.
func TestDocs_Page_PinsScalarWithIntegrity(t *testing.T) {
	d := &Docs{Spec: []byte(`{}`)}

	w := httptest.NewRecorder()
	d.Page(w, httptest.NewRequest(http.MethodGet, "/api/v1/docs", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()

	wantScriptSrc := "https://cdn.jsdelivr.net/npm/@scalar/api-reference@" + scalarVersion + "/dist/browser/standalone.min.js"
	if !strings.Contains(body, wantScriptSrc) {
		t.Errorf("body does not reference the pinned, versioned Scalar URL %q\nbody:\n%s", wantScriptSrc, body)
	}
	// The old vulnerable form -- no @version at all -- must be gone.
	if strings.Contains(body, `src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"`) {
		t.Error("body still loads the unversioned (unpinned) Scalar URL")
	}

	wantIntegrity := `integrity="` + scalarIntegrity + `"`
	if !strings.Contains(body, wantIntegrity) {
		t.Errorf("body does not contain %q\nbody:\n%s", wantIntegrity, body)
	}
	if !strings.Contains(scalarIntegrity, "sha384-") {
		t.Errorf("scalarIntegrity = %q, want a sha384- SRI hash", scalarIntegrity)
	}

	if !strings.Contains(body, `crossorigin="anonymous"`) {
		t.Error("body's pinned <script> tag is missing crossorigin=\"anonymous\" (required for SRI on a cross-origin script)")
	}

	// The Scalar configuration (data-url) must live as a data attribute,
	// not as executed inline JS -- otherwise this page would need a
	// script-src hash or 'unsafe-inline' in cspDocs for it.
	if !strings.Contains(body, `data-url="/api/v1/openapi.json"`) {
		t.Error(`body is missing data-url="/api/v1/openapi.json" on the Scalar script tag`)
	}
	if strings.Contains(body, "Scalar.createApiReference") {
		t.Error("body configures Scalar via an inline script call, not data attributes -- cspDocs' script-src has no allowance for that")
	}

	if got := w.Header().Get("Content-Security-Policy"); got != cspDocs {
		t.Errorf("Content-Security-Policy = %q, want cspDocs %q", got, cspDocs)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
}

// TestCSPDocs_AllowsJSDelivrNotArbitraryOrigins pins cspDocs' shape: it
// must widen script/style/font-src to jsDelivr specifically (what the
// pinned Scalar build needs), not to '*' or 'unsafe-eval', and must keep
// connect-src at 'self' so the docs page can't be used as an open proxy
// to whatever server URL an OpenAPI document's `servers` list names.
func TestCSPDocs_AllowsJSDelivrNotArbitraryOrigins(t *testing.T) {
	for _, want := range []string{
		"script-src 'self' https://cdn.jsdelivr.net",
		"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net",
		"font-src 'self' data: https://cdn.jsdelivr.net",
		"connect-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(cspDocs, want) {
			t.Errorf("cspDocs = %q, want it to contain %q", cspDocs, want)
		}
	}
	if strings.Contains(cspDocs, "'unsafe-eval'") || strings.Contains(cspDocs, "script-src *") {
		t.Errorf("cspDocs = %q, too permissive", cspDocs)
	}
}
