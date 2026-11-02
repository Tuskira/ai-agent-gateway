package agent

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireInboundToken wraps h so that, with Config.InboundToken set, a
// request without "Authorization: Bearer <InboundToken>" is refused with 401
// before it reaches a handler. GET /healthz stays open: it serves counts
// only, and probes cannot easily send a header. With no token configured h
// is returned as it is.
func (a *Agent) requireInboundToken(h http.Handler) http.Handler {
	if a.cfg.InboundToken == "" {
		return h
	}
	want := sha256.Sum256([]byte(a.cfg.InboundToken))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		// Hash both sides so the comparison takes the same time whatever
		// the length of what was sent.
		got := sha256.Sum256([]byte(bearerOf(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			answer(w, http.StatusUnauthorized, nil)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// bearerOf is the token of r's "Authorization: Bearer <token>" header, or ""
// when there is none.
func bearerOf(r *http.Request) string {
	const prefix = "bearer "
	v := r.Header.Get("Authorization")
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(v[len(prefix):])
}
