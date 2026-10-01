// Package session implements console-user sessions: the opaque cookie
// token, the Authenticator that turns the cookie into a Principal, the
// CSRF double-submit check for cookie-authenticated requests, and the
// must-change-password gate.
//
// Sessions are method-agnostic: a session row records WHO is logged in,
// not how they proved it, so a later SSO login can create the same rows.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const (
	// CookieName is the session cookie.
	CookieName = "gw_session"
	// CSRFHeader carries the session's CSRF token on every state-changing
	// request made with the session cookie.
	CSRFHeader = "X-CSRF-Token"

	// DefaultIdle and DefaultMax mirror config.Auth.SessionIdle/SessionMax.
	DefaultIdle = 8 * time.Hour
	DefaultMax  = 24 * time.Hour

	// TouchInterval is the most often a session's last_seen_at is written.
	TouchInterval = time.Minute

	tokenBytes = 32
)

// Reasons a stored session is unusable (see CheckUsable).
var (
	ErrRevoked = errors.New("session revoked")
	ErrExpired = errors.New("session expired")
	ErrIdle    = errors.New("session idle timeout")
)

// NewToken returns a fresh session token: the raw value that goes into the
// cookie (32 random bytes, base64url) and its storage id, the hex SHA-256
// of the raw value. Only the id is persisted.
func NewToken() (raw, id string, err error) {
	raw, err = randomToken()
	if err != nil {
		return "", "", err
	}
	return raw, HashToken(raw), nil
}

// NewCSRFToken returns a fresh CSRF token (32 random bytes, base64url).
func NewCSRFToken() (string, error) { return randomToken() }

func randomToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the storage id of a raw session token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CheckUsable applies the lifetime rules to a stored session at now:
// revoked, past its absolute expiry, or unused for idle or longer. It
// returns nil when the session is still good. The idle window is measured
// from LastSeenAt, which the Authenticator refreshes at most once per
// TouchInterval, so the effective idle timeout is idle plus up to
// TouchInterval.
func CheckUsable(s *store.UserSession, now time.Time, idle time.Duration) error {
	switch {
	case s.RevokedAt != nil:
		return ErrRevoked
	case !now.Before(s.ExpiresAt):
		return ErrExpired
	case idle > 0 && !now.Before(s.LastSeenAt.Add(idle)):
		return ErrIdle
	}
	return nil
}

// Options configures an Authenticator. Zero values take the defaults.
type Options struct {
	Idle   time.Duration
	Logger *slog.Logger
	// Now is overridable for tests.
	Now func() time.Time
}

// Authenticator resolves the session cookie to a user Principal. It
// implements auth.Authenticator.
type Authenticator struct {
	users    store.UserStore
	sessions store.UserSessionStore
	idle     time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

var _ auth.Authenticator = (*Authenticator)(nil)

// New returns an Authenticator over the given stores.
func New(users store.UserStore, sessions store.UserSessionStore, opts Options) *Authenticator {
	if opts.Idle <= 0 {
		opts.Idle = DefaultIdle
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Authenticator{users: users, sessions: sessions, idle: opts.Idle, logger: opts.Logger, now: opts.Now}
}

// Name identifies this Authenticator within an auth.Chain.
func (a *Authenticator) Name() string { return "session" }

// Authenticate returns auth.ErrNoCredential when r carries no session
// cookie, and an error wrapping auth.ErrInvalid when the cookie names no
// usable session (unknown, revoked, expired, idle) or its user is gone or
// disabled. The reason is for server logs only.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (*auth.Principal, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, auth.ErrNoCredential
	}

	sess, err := a.sessions.Get(ctx, HashToken(c.Value))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: unknown session", auth.ErrInvalid)
		}
		return nil, fmt.Errorf("session: lookup: %w", err)
	}

	now := a.now()
	if err := CheckUsable(sess, now, a.idle); err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalid, err)
	}

	user, err := a.users.Get(ctx, sess.TenantID, sess.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: session user gone", auth.ErrInvalid)
		}
		return nil, fmt.Errorf("session: load user: %w", err)
	}
	if user.Disabled {
		return nil, fmt.Errorf("%w: user disabled", auth.ErrInvalid)
	}

	if now.Sub(sess.LastSeenAt) >= TouchInterval {
		if err := a.sessions.Touch(ctx, sess.ID, now); err != nil {
			// Best effort: a failed touch must not log the user out.
			a.logger.Warn("session: touch last_seen_at failed", "error", err)
		}
	}

	p := &auth.Principal{
		Subject:            user.ID,
		TenantID:           user.TenantID,
		Roles:              []string{user.Role},
		AuthMethod:         auth.AuthMethodSession,
		Username:           user.Username,
		SessionID:          sess.ID,
		CSRFToken:          sess.CSRFToken,
		MustChangePassword: user.MustChangePassword,
	}
	if strings.Contains(user.Username, "@") {
		p.Email = user.Username
	}
	return p, nil
}

// SecureCookie reports whether the session cookie must carry the Secure
// attribute for r under mode ("auto" | "true" | "false").
func SecureCookie(r *http.Request, mode string) bool {
	switch mode {
	case "true":
		return true
	case "false":
		return false
	}
	if r.TLS != nil {
		return true
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// SetCookie writes the session cookie: HttpOnly, SameSite=Strict, Path=/,
// living at most maxAge (the session's absolute lifetime).
func SetCookie(w http.ResponseWriter, r *http.Request, rawToken, secureMode string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    rawToken,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   SecureCookie(r, secureMode),
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie expires the session cookie in the browser.
func ClearCookie(w http.ResponseWriter, r *http.Request, secureMode string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   SecureCookie(r, secureMode),
		SameSite: http.SameSiteStrictMode,
	})
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeForbidden(w http.ResponseWriter, errType, message string) {
	var b errorBody
	b.Error.Type, b.Error.Message = errType, message
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(b)
}

// Error types of the two 403s this package emits.
const (
	TypeCSRF                   = "csrf_error"
	TypePasswordChangeRequired = "password_change_required"
)

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// CSRF enforces the double-submit check: a state-changing request
// (anything but GET/HEAD/OPTIONS) authenticated by the session cookie
// must carry an X-CSRF-Token header equal to the session's csrf token.
// Requests authenticated any other way (an API key in the Authorization
// header, dev mode) are exempt: a cross-site attacker cannot make a
// browser attach those. Mount it after the authentication middleware.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if ok && p.IsUser() && !safeMethod(r.Method) {
			got := r.Header.Get(CSRFHeader)
			if got == "" || p.CSRFToken == "" || subtle.ConstantTimeCompare([]byte(got), []byte(p.CSRFToken)) != 1 {
				writeForbidden(w, TypeCSRF, "missing or invalid CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// MustChangeGate rejects with 403 password_change_required any request
// from a session whose user must change their password, unless allowed
// reports the request as one of the few a user in that state may make
// (read own identity, change password, log out). Non-session principals
// are never gated. Mount it after the authentication middleware.
func MustChangeGate(allowed func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFrom(r.Context())
			if ok && p.IsUser() && p.MustChangePassword && !allowed(r) {
				writeForbidden(w, TypePasswordChangeRequired, "password change required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
