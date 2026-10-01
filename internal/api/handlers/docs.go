package handlers

import (
	"fmt"
	"net/http"
)

// Docs serves the generated OpenAPI document and a minimal API reference
// page that renders it. Spec is set once, after the full route table
// (including these two routes) is assembled -- see api.buildRoutes -- so
// Docs uses pointer-receiver methods: the *handlers.Docs bound into the
// route table before Spec is known still sees it once it's set, since the
// bound method value holds the pointer, not a snapshot of the struct.
type Docs struct {
	Spec []byte
}

// scalarVersion and scalarIntegrity pin the exact @scalar/api-reference
// build /api/v1/docs loads from jsDelivr, instead of the unversioned
// "@latest" URL: whatever jsDelivr resolves that to changes without
// notice, out from under this binary, in every admin's browser. Bump
// both together when a newer Scalar release is wanted:
//
//	npm view @scalar/api-reference version --registry=https://registry.npmjs.org/
//	curl -s -o /tmp/scalar.js \
//	  https://cdn.jsdelivr.net/npm/@scalar/api-reference@<new-version>/dist/browser/standalone.min.js \
//	  && openssl dgst -sha384 -binary /tmp/scalar.js | openssl base64 -A
//
// dist/browser/standalone.min.js is jsDelivr's own resolved default entry
// for this package's "browser" field today (confirmed via
// https://data.jsdelivr.com/v1/packages/npm/@scalar/api-reference@<version>/entrypoints)
// -- re-check that response's "js" entrypoint hasn't moved before
// bumping, since a stale path here would silently 404 the docs page.
const (
	scalarVersion   = "1.72.1"
	scalarIntegrity = "sha384-JezfTaoGe2t8F2YRYUQosjM0S21blpE8j3yOUgTEiTKCyLWx9K4lfwjKPd8Dp7WY"
)

// cspDocs is the docs page's own Content-Security-Policy, overriding the
// control plane's default (api.cspDefault): it needs jsDelivr as a
// script/style/font source for Scalar's CDN build, which nothing else on
// this origin does. Page sets this itself rather than the security
// headers middleware special-casing /api/v1/docs, so the one route that
// needs a wider policy is also the one place that has to justify it.
//
//   - script-src: 'self' (unused here, but keeps the directive from
//     silently falling back to default-src) plus jsDelivr, for Scalar's
//     bundle -- no 'unsafe-inline' and no script hash needed since the
//     page carries no inline script (see docsHTML: the Scalar
//     configuration lives in data-url on the same tag that loads the
//     bundle, not in an executed inline script).
//   - style-src: 'unsafe-inline' because Scalar (a Vue app) injects
//     <style> tags at runtime for component-scoped CSS; jsDelivr because
//     Scalar also ships its own stylesheet chunks from there.
//   - img-src: https: (not just jsDelivr) because an OpenAPI document can
//     point Scalar's viewer at operation/schema examples hosted anywhere.
//   - connect-src: 'self' only -- the OpenAPI document this page loads is
//     always same-origin (/api/v1/openapi.json), and this deliberately
//     does NOT allow Scalar's "Try it" panel to reach arbitrary hosts a
//     spec's `servers` list might name, or Scalar's own telemetry
//     endpoints.
const cspDocs = "default-src 'self'; " +
	"script-src 'self' https://cdn.jsdelivr.net; " +
	"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; " +
	"img-src 'self' data: https:; " +
	"font-src 'self' data: https://cdn.jsdelivr.net; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// OpenAPI handles GET /api/v1/openapi.json.
func (d *Docs) OpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(d.Spec)
}

// Page handles GET /api/v1/docs: a single self-contained HTML page that
// loads the OpenAPI document from /api/v1/openapi.json into Scalar's API
// reference viewer (via CDN -- no build step, no vendored assets).
func (d *Docs) Page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Overrides the default CSP the security headers middleware already
	// set on this response -- see cspDocs' doc comment.
	w.Header().Set("Content-Security-Policy", cspDocs)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsHTML))
}

// docsHTML is built once at package init from scalarVersion/
// scalarIntegrity, not a plain const, since it interpolates them.
var docsHTML = fmt.Sprintf(`<!doctype html>
<html>
  <head>
    <title>Tusk AI Secured Gateway - API Reference</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script
      id="api-reference"
      data-url="/api/v1/openapi.json"
      src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@%s/dist/browser/standalone.min.js"
      integrity="%s"
      crossorigin="anonymous"
    ></script>
  </body>
</html>
`, scalarVersion, scalarIntegrity)
