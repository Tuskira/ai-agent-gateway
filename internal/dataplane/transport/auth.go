package transport

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// AuthMiddleware authenticates every MCP request through a, attaching
// the resolved Principal and a request-scoped logger to the context.
//
// It does the same work as internal/auth.Middleware but answers a
// failure in the caller's own protocol: HTTP 401 carrying a JSON-RPC
// error object with code -32001, rather than the REST plane's
// {"error":{"type":...}} body. An MCP client parses the body as JSON-RPC
// whatever the status code, so the REST shape reaches it as an
// unparseable response instead of an authentication failure it can
// report.
//
// The 401 body never says WHY the credential failed -- unknown, revoked
// and expired are indistinguishable to the caller -- so a caller cannot
// use responses to enumerate which keys exist. The reason is logged
// server-side.
// PermissionMCPAccess is the single permission the MCP plane requires;
// the built-in agent role holds it via mcp.*, admin via *. A custom role
// with only *.read cannot reach the plane.
const PermissionMCPAccess = "mcp.access"

func AuthMiddleware(a pkgauth.Authenticator, z pkgauth.Authorizer, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := a.Authenticate(r.Context(), r)
			if err != nil {
				var rle *pkgauth.RateLimitedError
				if errors.As(err, &rle) {
					logger.Warn("mcp authentication rate limited", "path", r.URL.Path, "method", r.Method)
					writeRateLimited(w, rle.RetryAfter)
					return
				}
				logger.Warn("mcp authentication failed", "error", err, "path", r.URL.Path, "method", r.Method)
				writeUnauthorized(w)
				return
			}

			if z != nil && !z.Allow(r.Context(), principal, PermissionMCPAccess) {
				logger.Warn("mcp authorization denied", "principal", principal.Subject, "roles", principal.Roles)
				writeForbidden(w)
				return
			}

			// Also record it on the request-scoped log record, which
			// the access-log middleware outside this one reads back.
			reqctx.From(r.Context()).SetPrincipal(principal)

			ctx := pkgauth.WithPrincipal(r.Context(), principal)
			ctx = applog.With(ctx, logger.With(
				"principal", principal.Subject,
				"tenant_id", principal.TenantID,
				"key_id", principal.KeyID,
			))

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeUnauthorized emits the one response the MCP plane sends with a
// non-200 status.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(mcp.NewErrorResponse(nil,
		mcp.NewError(mcp.ErrorCodeUnauthorized, "authentication required", nil)))
}

// writeRateLimited is the 429 for a caller locked out by the auth-failure
// limiter: a JSON-RPC error body, like every other response of this plane.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	auth.SetRetryAfter(w, retryAfter)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(mcp.NewErrorResponse(nil,
		mcp.NewError(mcp.ErrorCodeRateLimited, "too many failed authentication attempts", nil)))
}

// writeForbidden is the 403 for an authenticated caller whose role lacks
// mcp.access.
func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(mcp.NewErrorResponse(nil,
		mcp.NewError(mcp.ErrorCodeForbidden, "role does not permit MCP access", nil)))
}
