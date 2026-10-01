// Package ui embeds and serves the React admin console (web/) as static
// assets baked into the gateway binary.
//
// The embedded tree lives at dist/ inside this package. It is populated by
// `make ui-build` (Vite configured to build into internal/api/ui/dist, see
// web/vite.config.ts). A dist/.gitkeep placeholder keeps the directory
// present -- and this package compiling -- even when the UI has never been
// built; Handler serves 503s in that case instead of panicking.
package ui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed dist/*
var embedded embed.FS

// distDirName is the top-level directory inside the embedded FS that Vite's
// build output (and the dist/.gitkeep placeholder) lives under.
const distDirName = "dist"

// Handler returns an http.Handler that serves the embedded React admin
// console: static files under /assets/* (and any other built file) with
// content-type detection, long-lived immutable caching for hashed asset
// filenames, no-cache for index.html, and an SPA fallback so client-side
// routes (e.g. /login, /api-keys) resolve to index.html on a fresh GET.
//
// If the UI was never built (dist/ contains only the .gitkeep placeholder,
// no index.html), the returned handler responds 503 to every request with a
// plain-text message telling the operator to run `make ui-build`.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, distDirName)
	if err != nil {
		// embedded is compiled in via go:embed; Sub only fails on a
		// malformed dir name, which is a build-time constant here.
		panic("ui: fs.Sub(" + distDirName + "): " + err.Error())
	}

	if !hasIndex(sub) {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("UI not built; run make ui-build\n"))
		})
	}

	return &spaHandler{fs: sub}
}

// hasIndex reports whether index.html exists at the root of fsys, i.e.
// whether the UI has actually been built into the embedded tree (as
// opposed to just the dist/.gitkeep placeholder).
func hasIndex(fsys fs.FS) bool {
	f, err := fsys.Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// spaHandler serves an embedded single-page-app build: static files as-is,
// with a fallback to index.html for any GET that doesn't match a real file,
// so client-side routes resolve on a hard refresh or deep link. The
// gateway's router only mounts this handler at "/" -- /api/v1/* is routed
// separately and never reaches here -- but ServeHTTP still excludes /api/
// paths from the fallback defensively.
type spaHandler struct {
	fs fs.FS
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	if name != "index.html" {
		if fi, err := fs.Stat(h.fs, name); err == nil && !fi.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.FileServer(http.FS(h.fs)).ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Defense in depth: the gateway's router never routes
			// /api/v1/* here, but don't SPA-fallback an API-shaped
			// path if it somehow arrives.
			http.NotFound(w, r)
			return
		}
	}

	// Either the request is for "/" (or "/index.html" itself), or it's a
	// GET for a path that isn't a real built file -- SPA fallback so
	// client-side routes (e.g. /login, /api-keys) resolve on a fresh GET.
	serveIndex(w, r, h.fs)
}

// serveIndex serves dist/index.html with no-cache headers. It deliberately
// uses http.ServeContent (not http.FileServer/http.ServeFile) because
// ServeFile special-cases any request whose path ends in "index.html" and
// redirects it to "./" -- which would turn every "/" and SPA-fallback
// request into a 301 instead of serving the page.
func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	f, err := fsys.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// embed.FS (and fs.Sub over it) files always implement Seek;
		// this is defensive against a future fs.FS swap.
		http.Error(w, "index.html: not seekable", http.StatusInternalServerError)
		return
	}

	var modTime time.Time
	if fi, statErr := f.Stat(); statErr == nil {
		modTime = fi.ModTime()
	}

	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", modTime, rs)
}
