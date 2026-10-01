package api

import "net/http"

// cspDefault is the Content-Security-Policy every response on this origin
// carries unless a handler overrides it (see handlers.Docs.Page's
// cspDocs). This origin serves three things behind one policy: the
// authenticated JSON API, the public docs page, and the embedded admin
// console -- which keeps the caller's admin API key in sessionStorage
// (see web/src, API_KEY_STORAGE_KEY), so a script-src gap here is a gap an
// attacker can use to exfiltrate it.
//
//   - script-src/style-src: 'self' only, plus 'unsafe-inline' for style --
//     the console has no inline <script> (see web/public/theme-init.js,
//     served same-origin instead of inline specifically so this can stay
//     script-src 'self' with no hash to keep in sync) but Radix/shadcn
//     (web/package.json) set `style=` attributes on elements at runtime,
//     which only 'unsafe-inline' on style-src permits.
//   - font-src: 'self' only. Inter and JetBrains Mono are self-hosted via
//     @fontsource/* (see web/src/main.tsx), bundled into the console's own
//     assets at build time -- no request to fonts.googleapis.com or
//     fonts.gstatic.com, so neither needs a CSP allowance. The token
//     stacks in web/src/styles/tokens.css fall back to system fonts if a
//     font file ever fails to load.
//   - img-src: data: for inlined SVG/icon data URIs; no third-party image
//     host -- nothing in web/src loads one.
//   - connect-src: 'self' only. The console only ever calls this origin's
//     own /api/v1/*.
//   - frame-ancestors 'none' / X-Frame-Options DENY: belt and suspenders --
//     the former is what modern browsers honor, the latter covers any
//     that don't.
const cspDefault = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// securityHeaders sets the fixed set of browser-hardening headers on every
// response this router serves (mounted in NewRouter ahead of both the
// /api/v1 route table and the UI mount, so it wraps the JSON API, the
// docs page, and the embedded admin console alike):
//
//   - X-Content-Type-Options: nosniff -- stops a browser from
//     MIME-sniffing a response into executing as something other than its
//     declared Content-Type.
//   - Referrer-Policy: no-referrer -- an admin-console URL or an
//     analytics deep link can carry a tenant id or request id; don't leak
//     it via the Referer header to whatever a click or a fetch reaches.
//   - X-Frame-Options: DENY -- the console (holding the admin API key)
//     and the docs page are never meant to be framed by another site.
//   - Permissions-Policy -- this origin never needs the camera,
//     microphone, or geolocation; deny all three outright.
//   - Content-Security-Policy: cspDefault -- see its doc comment. Set
//     before next runs, so a handler that needs a different policy for
//     its own response (only handlers.Docs.Page does, today) can still
//     overwrite it before writing a body.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", cspDefault)
		next.ServeHTTP(w, r)
	})
}
