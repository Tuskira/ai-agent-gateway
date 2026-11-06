package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	oidcauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/oidc"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// OIDC implements single sign-on for console users:
// GET /auth/oidc/{provider}/start and GET /auth/oidc/{provider}/callback.
// Both are public browser navigations, behind the same per-IP lockout as
// POST /auth/login. The protocol (state, nonce, PKCE, the ID-token checks
// and email rules) lives in internal/auth/oidc; this handler maps the
// verified identity to a console user of the provider's tenant and starts
// the same session a password login does (user_sessions row, gw_session
// cookie, CSRF token), with auth_method "oidc".
//
// Every failure redirects to the console login page with
// ?sso_error=<code> (a fixed oidcauth.Code* value, never free text), is
// audited as login_fail with detail "oidc:<provider>:<code>", and counts
// as a failure against the caller's IP.
type OIDC struct{ Deps }

// Start handles GET /api/v1/auth/oidc/{provider}/start?return_to=<path>:
// sets the sealed login-state cookie and redirects to the issuer's
// authorize endpoint. A return_to that is not a same-origin relative path
// is ignored ("/" is used).
func (h OIDC) Start(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "provider")
	p, ok := h.OIDC.Provider(name)
	if !ok {
		h.redirectError(w, r, oidcauth.CodeProvider)
		return
	}
	returnTo, ok := oidcauth.CleanReturnTo(r.URL.Query().Get("return_to"))
	if !ok {
		returnTo = "/"
	}
	authURL, cookie, err := h.OIDC.Begin(p, r, returnTo)
	if err != nil {
		applog.From(r.Context()).Warn("sso start failed", "provider", p.Name, "error", err)
		h.redirectError(w, r, oidcauth.CodeProvider)
		return
	}
	http.SetCookie(w, cookie)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback handles GET /api/v1/auth/oidc/{provider}/callback: the IdP's
// redirect back. On success it answers a small page that navigates to
// return_to from the gateway's own origin (see oidcauth.WriteCompletePage
// for why not a 302).
func (h OIDC) Callback(w http.ResponseWriter, r *http.Request) {
	// The login state is single use: cleared on every answer.
	for _, c := range oidcauth.ClearStateCookies() {
		http.SetCookie(w, c)
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()

	name := chi.URLParam(r, "provider")
	p, ok := h.OIDC.Provider(name)
	if !ok {
		// The path segment is caller-controlled: it goes to the log (bounded),
		// never into the audit detail.
		h.fail(w, r, "", "unknown", "", "", &oidcauth.Error{Code: oidcauth.CodeProvider,
			Err: fmt.Errorf("unknown provider %q", truncate(name, 64))})
		return
	}
	tenant, err := h.oidcTenant(ctx, p.TenantRef)
	if err != nil {
		h.fail(w, r, "", p.Name, "", "", &oidcauth.Error{Code: oidcauth.CodeProvider, Err: err})
		return
	}

	res, err := h.OIDC.Finish(p, r)
	if err != nil {
		var oe *oidcauth.Error
		if !errors.As(err, &oe) {
			oe = &oidcauth.Error{Code: oidcauth.CodeProvider, Err: err}
		}
		h.fail(w, r, tenant.ID, p.Name, "", "", oe)
		return
	}
	ident := res.Identity

	user, err := h.resolveUser(r, p, tenant.ID, ident)
	if err != nil {
		var oe *oidcauth.Error
		targetID := ""
		if user != nil {
			targetID = user.ID
		}
		if !errors.As(err, &oe) {
			applog.From(ctx).Error("sso callback: resolve user failed", "provider", p.Name, "error", err)
			oe = &oidcauth.Error{Code: oidcauth.CodeProvider, Err: err}
		}
		h.fail(w, r, tenant.ID, p.Name, ident.Email, targetID, oe)
		return
	}

	lifetime := h.sessionMax()
	if m := p.SessionMax(); m > 0 && m < lifetime {
		lifetime = m
	}
	_, raw, err := h.newSession(r, user, store.SessionAuthOIDC, lifetime)
	if err != nil {
		applog.From(ctx).Error("sso callback: create session failed", "user_id", user.ID, "error", err)
		h.redirectError(w, r, oidcauth.CodeProvider)
		return
	}
	now := time.Now()
	if err := h.Store.Users().RecordLoginSuccess(ctx, tenant.ID, user.ID, now); err != nil {
		applog.From(ctx).Error("sso callback: record success failed", "user_id", user.ID, "error", err)
	}
	if h.LoginThrottle != nil {
		h.LoginThrottle.ReportSuccess(h.clientIP(r))
	}
	h.audit(r, store.AuthAuditEntry{
		TenantID: tenant.ID, ActorKind: "user", ActorID: user.ID, Action: "login_ok", TargetUserID: user.ID,
		Detail: "oidc:" + p.Name,
	})

	session.SetCookie(w, r, raw, h.Console.CookieSecure, lifetime)
	oidcauth.WriteCompletePage(w, res.ReturnTo)
}

func (h OIDC) sessionMax() time.Duration {
	if h.Console.SessionMax > 0 {
		return h.Console.SessionMax
	}
	return session.DefaultMax
}

// redirectError sends the browser to the console login page with a fixed
// failure code.
func (h OIDC) redirectError(w http.ResponseWriter, r *http.Request, code string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, oidcauth.LoginErrorPath(code), http.StatusFound)
}

// fail logs, audits and counts a failed SSO login, then redirects with its
// code. The error detail goes to the server log only.
func (h OIDC) fail(w http.ResponseWriter, r *http.Request, tenantID, provider, email, targetUserID string, e *oidcauth.Error) {
	applog.From(r.Context()).Warn("sso login refused", "provider", provider, "code", e.Code, "error", e.Err)
	if h.LoginThrottle != nil {
		h.LoginThrottle.ReportFailure(h.clientIP(r))
	}
	h.audit(r, store.AuthAuditEntry{
		TenantID: tenantID, ActorKind: "anonymous", ActorID: truncate(email, 80),
		Action: "login_fail", TargetUserID: targetUserID, Detail: truncate("oidc:"+provider+":"+e.Code, 120),
	})
	h.redirectError(w, r, e.Code)
}

// oidcTenant resolves a provider's configured tenant, by id when it is a
// UUID and by slug otherwise.
func (h OIDC) oidcTenant(ctx context.Context, ref string) (*store.Tenant, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return h.Store.Tenants().GetByID(ctx, ref)
	}
	return h.Store.Tenants().GetBySlug(ctx, ref)
}

func refuse(code, msg string) *oidcauth.Error {
	return &oidcauth.Error{Code: code, Err: errors.New(msg)}
}

// resolveUser maps a verified identity to a console user of tenantID.
//
//  1. A link for (provider, issuer, subject) names the user. A link whose
//     user no longer exists (deleted) is removed and step 2 runs.
//  2. Otherwise the live user whose username is the email (lowercase) is
//     the candidate. None: refused (no_user), or created when the provider
//     provisions just in time. A candidate already linked to another
//     subject at this provider and issuer is refused (linked): a second
//     account at the issuer claiming the same address must not take the
//     user over.
//  3. A disabled user is refused (disabled); a platform-admin is refused
//     (role) unless the provider allows it.
//  4. A first login creates the link; later ones record the login on it.
//
// An *oidcauth.Error is a refusal; any other error is a store failure. The
// returned user is set on a refusal when one was found (for the audit).
func (h OIDC) resolveUser(r *http.Request, p *oidcauth.Provider, tenantID string, ident *oidcauth.Identity) (*store.User, error) {
	ctx := r.Context()
	ids := h.Store.UserIdentities()
	users := h.Store.Users()

	var user *store.User
	link, err := ids.GetBySubject(ctx, tenantID, p.Name, ident.Issuer, ident.Subject)
	switch {
	case err == nil:
		u, err := users.Get(ctx, tenantID, link.UserID)
		switch {
		case err == nil:
			user = u
		case errors.Is(err, store.ErrNotFound):
			if err := ids.Delete(ctx, tenantID, link.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			link = nil
		default:
			return nil, err
		}
	case errors.Is(err, store.ErrNotFound):
		link = nil
	default:
		return nil, err
	}

	if user == nil {
		u, err := users.GetByUsername(ctx, tenantID, ident.Email)
		switch {
		case err == nil:
			user = u
		case errors.Is(err, store.ErrNotFound):
			if p.Config().Provision != config.OIDCProvisionJIT {
				return nil, refuse(oidcauth.CodeNoUser, "no user with this email in the tenant")
			}
			if user, err = h.provisionUser(r, p, tenantID, ident); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
		other, err := ids.GetByUser(ctx, tenantID, p.Name, ident.Issuer, user.ID)
		switch {
		case err == nil && other.Subject != ident.Subject:
			return user, refuse(oidcauth.CodeLinked, "the user is linked to another subject at this issuer")
		case err == nil:
			link = other
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}

	if user.Disabled {
		return user, refuse(oidcauth.CodeDisabled, "the user is disabled")
	}
	if user.Role == store.UserRolePlatformAdmin && !p.Config().AllowPlatformAdmin {
		return user, refuse(oidcauth.CodeRole, "platform-admin users may not sign in through this provider")
	}

	now := time.Now()
	if link == nil {
		nl := &store.UserIdentity{
			TenantID: tenantID, Provider: p.Name, Issuer: ident.Issuer, Subject: ident.Subject,
			UserID: user.ID, Email: ident.Email,
		}
		err := ids.Create(ctx, nl)
		if errors.Is(err, store.ErrConflict) {
			// A concurrent first login may have linked it already: fine if
			// it linked this subject to this user, refused otherwise.
			got, gerr := ids.GetBySubject(ctx, tenantID, p.Name, ident.Issuer, ident.Subject)
			if gerr != nil || got.UserID != user.ID {
				return user, refuse(oidcauth.CodeLinked, "the subject or the user was linked concurrently")
			}
			err = nil
		}
		if err != nil {
			return nil, err
		}
		return user, nil
	}
	if err := ids.RecordLogin(ctx, tenantID, link.ID, ident.Email, now); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return user, nil
}

// provisionUser creates the user for a just-in-time login: username = the
// email, role = jit_role (default viewer), no usable password. An email
// that is not a valid username cannot be provisioned (no_user).
func (h OIDC) provisionUser(r *http.Request, p *oidcauth.Provider, tenantID string, ident *oidcauth.Identity) (*store.User, error) {
	ctx := r.Context()
	username, err := store.ValidateUsername(ident.Email)
	if err != nil {
		return nil, refuse(oidcauth.CodeNoUser, "the email is not a valid username")
	}
	role := p.Config().JITRole
	if role == "" {
		role = store.UserRoleViewer
	}
	display := strings.TrimSpace(ident.Name)
	if validateDisplayName(display) != nil {
		display = ""
	}
	u := &store.User{TenantID: tenantID, Username: username, DisplayName: display, PasswordHash: password.Unusable, Role: role}
	if err := h.Store.Users().Create(ctx, u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Created by a concurrent login.
			return h.Store.Users().GetByUsername(ctx, tenantID, username)
		}
		return nil, err
	}
	h.audit(r, store.AuthAuditEntry{
		TenantID: tenantID, ActorKind: "anonymous", ActorID: truncate(ident.Email, 80), Action: "user_create",
		TargetUserID: u.ID, Detail: "role=" + u.Role + " oidc:" + p.Name,
	})
	return u, nil
}
