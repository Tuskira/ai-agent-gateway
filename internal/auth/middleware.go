package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// errorBody is the JSON shape of every auth-related error response:
// {"error":{"type":"...","message":"..."}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Middleware authenticates every request through a, rejecting with 401 on
// failure (whether a returned auth.ErrNoCredential or auth.ErrInvalid --
// both are handled identically here) and, on success, attaching the
// resolved Principal to the request context (pkgauth.WithPrincipal) and
// building a request-scoped slog logger, enriched with principal/
// tenant_id/key_id, stashed via internal/log.With so downstream handlers'
// logs are already attributed.
//
// The 401 response body never includes the specific failure reason (e.g.
// unknown vs. revoked vs. expired key) -- that distinction is logged
// server-side only, via logger, so a caller can't use response contents to
// enumerate which keys exist.
func Middleware(a pkgauth.Authenticator, logger *slog.Logger) func(http.Handler) http.Handler {
	return MiddlewareWith(a, logger, nil)
}

// RateLimitedResponder writes a plane's own 429 for a caller locked out by
// the auth-failure limiter (see pkgauth.ErrRateLimited). It must set the
// Retry-After header itself or call SetRetryAfter.
type RateLimitedResponder func(w http.ResponseWriter, r *http.Request, retryAfter time.Duration)

// SetRetryAfter sets Retry-After (seconds, rounded up, minimum 1).
func SetRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

// MiddlewareWith is Middleware with a plane-specific 429 writer; nil uses
// the REST envelope. The LLM plane passes a provider-shaped one.
func MiddlewareWith(a pkgauth.Authenticator, logger *slog.Logger, onRateLimited RateLimitedResponder) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.Authenticate(r.Context(), r)
			if err != nil {
				var rle *pkgauth.RateLimitedError
				if errors.As(err, &rle) {
					logger.Warn("authentication rate limited", "path", r.URL.Path, "method", r.Method)
					if onRateLimited != nil {
						onRateLimited(w, r, rle.RetryAfter)
					} else {
						writeRateLimited(w, rle.RetryAfter)
					}
					return
				}
				logger.Warn("authentication failed", "error", err, "path", r.URL.Path, "method", r.Method)
				writeError(w, http.StatusUnauthorized, "authentication_error", "authentication required")
				return
			}

			ctx := pkgauth.WithPrincipal(r.Context(), p)
			reqLogger := logger.With("principal", p.Subject, "tenant_id", p.TenantID, "key_id", p.KeyID)
			ctx = applog.With(ctx, reqLogger)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequirePermission rejects with 403 any request whose Principal (attached
// upstream by Middleware) is not granted perm by z. A request reaching
// this middleware with no Principal at all (e.g. Middleware was skipped)
// is treated as denied, not a panic: PrincipalFrom's zero value is nil,
// and every Authorizer must handle a nil Principal by denying it.
func RequirePermission(z pkgauth.Authorizer, perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := pkgauth.PrincipalFrom(r.Context())
			if !z.Allow(r.Context(), p, perm) {
				writeError(w, http.StatusForbidden, "permission_error", "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Type: errType, Message: message}})
}

// RateLimitMiddleware enforces rl's two policies on every request that
// reaches it: the general per-IP requests-per-minute cap (always, on
// every request) and the auth-failure lockout (only when exempt returns
// false for the request). It runs before the authenticator -- mount it
// ahead of Middleware in the chain -- so a locked-out IP never even
// reaches Authenticate.
//
// exempt identifies routes that must stay reachable during a lockout
// (health checks, docs, the embedded UI's static files, ...) without
// exempting them from the general cap; pass nil to apply the lockout to
// every request. generalCapExempt identifies routes that should skip the
// general per-IP requests-per-minute cap entirely -- today only POST
// /api/v1/ingest (internal/api/router.go's isIngestPath): many interceptor
// instances behind one office NAT share an IP, and that route has its own
// per-key limiter instead (internal/auth.KeyRateLimiter, applied inside
// the handler); pass nil to apply the general cap to every request, which
// is every other exempt route's existing behavior (see isRateLimitExempt's
// doc comment -- health etc. stay subject to the general cap on purpose).
// A nil rl (or one built with Enabled: false) makes this a no-op
// passthrough.
func RateLimitMiddleware(rl *RateLimiter, exempt, generalCapExempt func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.Enabled() {
				next.ServeHTTP(w, r)
				return
			}

			ip := rl.ClientIP(r)

			if generalCapExempt == nil || !generalCapExempt(r) {
				if ok, retryAfter := rl.AllowGeneral(ip); !ok {
					writeRateLimited(w, retryAfter)
					return
				}
			}

			if exempt == nil || !exempt(r) {
				if locked, retryAfter := rl.Locked(ip); locked {
					writeRateLimited(w, retryAfter)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeRateLimited writes a 429 with a Retry-After header (seconds,
// rounded up, minimum 1) using the gateway's standard error envelope.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	SetRetryAfter(w, retryAfter)
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
}

// rateLimitedAuthenticator wraps an Authenticator to report every outcome
// to a RateLimiter: a rejected credential (pkgauth.ErrInvalid or an
// unexpected error) counts toward that IP's lockout, a request with no
// credential at all does not (nothing was guessed), and a success resets
// it. A 429 from the per-credential limiter (pkgauth.ErrRateLimited) is
// passed through and does NOT count against the caller's IP: that lockout
// is about a key being guessed, not about who is asking.
type rateLimitedAuthenticator struct {
	inner pkgauth.Authenticator
	rl    *RateLimiter
	// enforce makes the wrapper itself refuse a locked-out IP (returning
	// *pkgauth.RateLimitedError) before authenticating. The API plane
	// leaves it off: RateLimitMiddleware already gates it, with route
	// exemptions. The MCP and LLM planes have no such middleware.
	enforce bool
}

// RateLimitedAuthenticator wraps inner so every Authenticate call reports
// its outcome to rl (see rateLimitedAuthenticator). When rl is nil or
// disabled, inner is returned unwrapped -- zero overhead, zero behavior
// change.
func RateLimitedAuthenticator(inner pkgauth.Authenticator, rl *RateLimiter) pkgauth.Authenticator {
	if !rl.Enabled() {
		return inner
	}
	return &rateLimitedAuthenticator{inner: inner, rl: rl}
}

// PlaneRateLimitedAuthenticator is RateLimitedAuthenticator for the MCP and
// LLM planes: it additionally refuses (429, via *pkgauth.RateLimitedError)
// any request from an IP currently locked out for failed authentication,
// before any credential lookup.
func PlaneRateLimitedAuthenticator(inner pkgauth.Authenticator, rl *RateLimiter) pkgauth.Authenticator {
	if !rl.Enabled() {
		return inner
	}
	return &rateLimitedAuthenticator{inner: inner, rl: rl, enforce: true}
}

func (a *rateLimitedAuthenticator) Name() string { return a.inner.Name() }

func (a *rateLimitedAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*pkgauth.Principal, error) {
	ip := a.rl.ClientIP(r)
	// This plane's own limiter also owns the per-credential-prefix counters
	// of the (shared) API-key authenticator for this call.
	ctx = apikey.WithGuard(ctx, a.rl)

	if a.enforce {
		if locked, retry := a.rl.Locked(ip); locked {
			return nil, &pkgauth.RateLimitedError{RetryAfter: retry}
		}
	}

	p, err := a.inner.Authenticate(ctx, r)
	if err != nil {
		// A request that presented no credential at all guessed nothing,
		// so it does not count toward the lockout (the general per-IP cap
		// still bounds it). This matters for the console, which probes
		// GET /auth/me on load to find out whether a session cookie is
		// still good.
		if !errors.Is(err, pkgauth.ErrNoCredential) && !errors.Is(err, pkgauth.ErrRateLimited) {
			a.rl.ReportFailure(ip)
		}
		return nil, err
	}
	a.rl.ReportSuccess(ip)
	return p, nil
}
