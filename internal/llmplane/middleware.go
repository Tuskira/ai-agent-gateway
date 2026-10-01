package llmplane

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

type ctxKey int

const (
	ctxSession ctxKey = iota
	ctxRequestID
)

// Caller identity comes from the Principal the gateway's auth middleware
// (internal/auth) resolves and stores on the context AHEAD of this plane. The
// LLM plane only READS it for capture and the per-tenant limiter; a missing
// Principal (e.g. an unauthenticated /health) yields empty strings.
func tenantOf(ctx context.Context) string {
	if p, ok := pkgauth.PrincipalFrom(ctx); ok {
		return p.TenantID
	}
	return ""
}
func principalOf(ctx context.Context) string {
	if p, ok := pkgauth.PrincipalFrom(ctx); ok {
		return p.Subject
	}
	return ""
}
func keyIDOf(ctx context.Context) string {
	if p, ok := pkgauth.PrincipalFrom(ctx); ok {
		return p.KeyID
	}
	return ""
}
func sessionOf(ctx context.Context) string   { s, _ := ctx.Value(ctxSession).(string); return s }
func requestIDOf(ctx context.Context) string { s, _ := ctx.Value(ctxRequestID).(string); return s }

// peerIP is the default client-IP function: the TCP peer's address.
// X-Forwarded-For is deliberately NOT consulted here; the process-wide
// clientip.Resolver (Config.ClientIP) decides when it may be believed.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// withTags reads the session tag for capture correlation and mints a
// per-request id. It does NOT authenticate. The tag is optional and read from
// X-Session-Id, falling back to X-Claude-Code-Session-Id (what Claude Code
// sends); both are stripped so they never reach the provider. Absent → the
// session id is empty and capture proceeds normally.
func withTags(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.Header.Get("X-Session-Id")
		if session == "" {
			session = r.Header.Get("X-Claude-Code-Session-Id")
		}
		r.Header.Del("X-Session-Id")
		r.Header.Del("X-Claude-Code-Session-Id")
		ctx := context.WithValue(r.Context(), ctxSession, session)
		ctx = context.WithValue(ctx, ctxRequestID, uuid.NewString())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requirePermission answers 403 unless z grants PermissionLLMAccess to the
// request's Principal. A missing Principal is denied, and so is a nil z: the
// plane must never run without an authorizer, so the degenerate case fails
// CLOSED rather than waving every caller through. Handler refuses to build
// with Config.Authorizer unset, so nil here means the chain was assembled by
// hand; it still denies.
func requirePermission(z pkgauth.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := pkgauth.PrincipalFrom(r.Context())
		if z == nil || !z.Allow(r.Context(), p, PermissionLLMAccess) {
			writeAnthropicError(w, http.StatusForbidden, requestIDOf(r.Context()), "insufficient permissions: "+PermissionLLMAccess+" required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody caps the request body; the router translates the resulting
// MaxBytesError into 413 on read, before any upstream dial.
func limitBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if max > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// streamDeadline bounds total request time (slow-loris / runaway stream guard).
func streamDeadline(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tenantLimiter caps concurrent in-flight requests per tenant; over the cap → 429.
// Tenant comes from context (set by the auth layer); unset → a single shared bucket.
type tenantLimiter struct {
	mu    sync.Mutex
	sem   map[string]chan struct{}
	limit int
}

func newTenantLimiter(limit int) *tenantLimiter {
	return &tenantLimiter{sem: make(map[string]chan struct{}), limit: limit}
}

func (tl *tenantLimiter) chanFor(tenant string) chan struct{} {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	ch := tl.sem[tenant]
	if ch == nil {
		ch = make(chan struct{}, tl.limit)
		tl.sem[tenant] = ch
	}
	return ch
}

func (tl *tenantLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tl.limit <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		ch := tl.chanFor(tenantOf(r.Context()))
		select {
		case ch <- struct{}{}:
			defer func() { <-ch }()
			next.ServeHTTP(w, r)
		default:
			writeAnthropicError(w, http.StatusTooManyRequests, requestIDOf(r.Context()),
				"too many concurrent requests for this tenant")
		}
	})
}
