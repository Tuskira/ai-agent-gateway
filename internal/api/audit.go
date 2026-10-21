package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/handlers"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// platformAudit wraps every platform.admin route (tenant creation and
// listing and the per-tenant API-key routes). It sits OUTSIDE the
// permission check, so a refused caller is logged too.
//
// Each request writes two INFO lines on the router's logger:
//
//	"platform request"  before the handler runs: who, from where, what
//	"platform result"   after it: the HTTP status, an outcome, and whatever
//	                    the handler recorded with handlers.AuditNote
//	                    (new_key_id, old_key_id/old_key_revoked for a rotate,
//	                    created_tenant_id, ...)
//
// so an action that dies half way (a rotate that revoked the old key but
// failed to issue the new one) still leaves both lines. Fields: method,
// route (the pattern), tenant_id (the {tenant_id} path parameter when the
// route has one), key_id (the {id} path parameter), actor (the
// authenticated principal's subject), actor_auth_method, actor_tenant_id,
// client_ip (as the rate limiter resolves it, honouring trusted proxies),
// request_id (the request's X-Request-Id header when the client or a proxy
// sent one; the gateway does not generate one), and on the result line
// status and outcome (ok, denied for 401/403, not_found, invalid for other
// 4xx, error for 5xx). Plaintext keys, hashes and request bodies are never
// logged.
func platformAudit(logger *slog.Logger, rl *internalauth.RateLimiter) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, trail := handlers.WithAuditTrail(r.Context())
			r = r.WithContext(ctx)

			base := []any{"method", r.Method, "client_ip", rl.ClientIP(r), "request_id", r.Header.Get("X-Request-Id")}
			if p, ok := pkgauth.PrincipalFrom(ctx); ok {
				base = append(base, "actor", p.Subject, "actor_auth_method", p.AuthMethod, "actor_tenant_id", p.TenantID)
			}
			rec := &statusWriter{ResponseWriter: w}
			// This runs inside the matched route's handler chain, so the
			// route pattern and path parameters are already resolved.
			rc := chi.RouteContext(ctx)
			base = append(base, "route", rc.RoutePattern())
			if v := chi.URLParam(r, "tenant_id"); v != "" {
				base = append(base, "tenant_id", v)
			}
			if v := chi.URLParam(r, "id"); v != "" {
				base = append(base, "key_id", v)
			}
			logger.InfoContext(ctx, "platform request", base...)

			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			out := append(append([]any{}, base...), "status", status, "outcome", outcome(status))
			out = append(out, trail.Fields()...)
			logger.InfoContext(ctx, "platform result", out...)
		})
	}
}

func outcome(status int) string {
	switch {
	case status < 400:
		return "ok"
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "denied"
	case status == http.StatusNotFound:
		return "not_found"
	case status < 500:
		return "invalid"
	default:
		return "error"
	}
}

// statusWriter records the response status. It forwards Flush so a
// streaming handler would still work; platform routes do not stream.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
