package oidc

import (
	"html"
	"net/http"
	"net/url"
	"strings"
)

// maxReturnTo bounds return_to.
const maxReturnTo = 2048

// CleanReturnTo validates where to send the user after a login. It must be
// a same-origin relative path: it starts with exactly one "/" (so not
// "//host" or "/\host", which browsers treat as another origin), holds no
// backslash, and only printable ASCII without spaces (anything else must
// be percent-encoded), with no scheme or host once parsed. "" means "/".
func CleanReturnTo(raw string) (string, bool) {
	if raw == "" {
		return "/", true
	}
	if len(raw) > maxReturnTo || raw[0] != '/' || strings.HasPrefix(raw, "//") || strings.ContainsRune(raw, '\\') {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= 0x20 || c >= 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "", false
	}
	return raw, true
}

// LoginErrorPath is the console login page carrying a failure code.
func LoginErrorPath(code string) string { return "/login?sso_error=" + url.QueryEscape(code) }

// completeCSP allows nothing: the page has no script, style or image, and
// navigates by <meta http-equiv=refresh>, which CSP does not govern.
const completeCSP = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// WriteCompletePage answers a successful callback. The session cookie is
// SameSite=Strict, and a browser does not send a Strict cookie on a
// navigation that started on another site -- which the callback is, coming
// from the IdP. Redirecting with a 302 would keep that cross-site origin,
// so the first console page would load without its session. This tiny
// page instead navigates to returnTo from the gateway's own origin, a
// same-site navigation that carries the cookie the response just set.
// returnTo must have passed CleanReturnTo; it is HTML-escaped anyway.
func WriteCompletePage(w http.ResponseWriter, returnTo string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", completeCSP)
	h.Set("Referrer-Policy", "no-referrer")
	esc := html.EscapeString(returnTo)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="referrer" content="no-referrer">` +
		`<meta http-equiv="refresh" content="0;url=` + esc + `">` +
		`<title>Signing in</title></head><body>` +
		`<p>Signed in. <a href="` + esc + `">Continue</a></p></body></html>`))
}
