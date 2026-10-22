package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Users implements the console-user management routes (GET/POST /users,
// GET/PATCH/DELETE /users/{id}, POST /users/{id}/reset-password and
// /revoke-sessions) and GET /auth/audit.
//
// Every route is gated on "users.manage" (see router.go), which only the
// admin role's "*" grant matches -- the viewer and agent roles' "*.read"
// does not -- so a read-only user cannot even list users, and an agent key
// gets 403. Admin-role API keys pass, so automation can create users and
// reset passwords; those calls are audited with actor_kind "api_key".
// Everything is scoped to the caller's tenant -- the acting tenant while a
// platform-admin sends X-Gateway-Acting-Tenant -- and another tenant's
// user id is a 404.
//
// The platform-admin role is special: giving it to a user (create, or a
// PATCH of role), and disabling, deleting, editing, resetting or logging
// out a user who holds it, needs the caller to hold platform.admin itself
// (403 otherwise), so a tenant admin can neither mint a platform-admin nor
// touch one that lives in its tenant. The last active platform-admin
// across all tenants cannot be disabled, deleted or demoted (409
// last_platform_admin); `gateway create-user -role platform-admin` is the
// recovery path.
type Users struct{ Deps }

// Error types of the guards (HTTP 409).
const (
	typeLastAdmin         = "last_admin"
	typeLastPlatformAdmin = "last_platform_admin"
	typeSelfAction        = "self_action"
)

func validateUserRole(role string) error {
	switch role {
	case store.UserRoleAdmin, store.UserRoleViewer, store.UserRolePlatformAdmin:
		return nil
	}
	return errors.New("role must be one of: admin, viewer, platform-admin")
}

// requirePlatformAdmin writes a 403 and returns false unless the caller
// holds platform.admin. It is the gate on everything that creates or
// touches a platform-admin user; like APIKeys.checkPlatformRole a nil
// Authorizer denies rather than trusting anyone.
func (h Users) requirePlatformAdmin(w http.ResponseWriter, r *http.Request, what string) bool {
	p, _ := pkgauth.PrincipalFrom(r.Context())
	if h.Authorizer != nil && h.Authorizer.Allow(r.Context(), p, pkgauth.PermPlatformAdmin) {
		return true
	}
	httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, what+" requires platform.admin")
	return false
}

// isPlatformAdmin reports whether u holds the platform-admin role.
func isPlatformAdmin(u *store.User) bool { return u.Role == store.UserRolePlatformAdmin }

func validateDisplayName(s string) error {
	if len([]rune(s)) > 128 {
		return errors.New("display_name must be at most 128 characters")
	}
	return nil
}

// userView is the JSON shape of a user. The password hash never appears.
type userView struct {
	ID                 string `json:"id"`
	Username           string `json:"username"`
	DisplayName        string `json:"display_name"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"must_change_password"`
	Disabled           bool   `json:"disabled"`
	// Locked reports an active per-user login lockout.
	Locked            bool   `json:"locked"`
	LastLoginAt       string `json:"last_login_at,omitempty"`
	PasswordChangedAt string `json:"password_changed_at"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

func newUserView(u *store.User) userView {
	return userView{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role,
		MustChangePassword: u.MustChangePassword, Disabled: u.Disabled,
		Locked:      u.LockedUntil != nil && time.Now().Before(*u.LockedUntil),
		LastLoginAt: formatTimePtr(u.LastLoginAt), PasswordChangedAt: formatTime(u.PasswordChangedAt),
		CreatedAt: formatTime(u.CreatedAt), UpdatedAt: formatTime(u.UpdatedAt),
	}
}

// selfID is the caller's own user id, or "" when the caller is not a
// console user (an API key is nobody's "self").
func selfID(r *http.Request) string {
	if p, ok := pkgauth.PrincipalFrom(r.Context()); ok && p.IsUser() {
		return p.Subject
	}
	return ""
}

// ---------------------------------------------------------------------------

type createUserRequest struct {
	Username    string  `json:"username"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"`
	Password    *string `json:"password"`
}

type createUserResponse struct {
	User              userView `json:"user"`
	TemporaryPassword string   `json:"temporary_password,omitempty"`
}

// Create handles POST /api/v1/users. With no password, a temporary one is
// generated, returned once, and the user must change it at first login.
// With a password (checked against the policy) the user may keep it.
func (h Users) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req createUserRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	username, err := store.ValidateUsername(req.Username)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateUserRole(req.Role); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateDisplayName(req.DisplayName); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if req.Role == store.UserRolePlatformAdmin && !h.requirePlatformAdmin(w, r, "creating a user with role platform-admin") {
		return
	}

	plain, temporary := "", false
	if req.Password != nil {
		plain = *req.Password
		if err := password.ValidatePolicy(plain, username); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	} else {
		if plain, err = password.GenerateTemporary(); err != nil {
			applog.From(r.Context()).Error("create user: generate temporary password failed", "error", err)
			httpx.Internal(w, "internal error")
			return
		}
		temporary = true
	}
	hash, err := password.Hash(plain)
	if err != nil {
		applog.From(r.Context()).Error("create user: hash failed", "error", err)
		httpx.Internal(w, "internal error")
		return
	}

	u := &store.User{
		TenantID: tid, Username: username, DisplayName: strings.TrimSpace(req.DisplayName),
		PasswordHash: hash, Role: req.Role, MustChangePassword: temporary,
	}
	if err := h.Store.Users().Create(r.Context(), u); err != nil {
		writeStoreErr(w, r, "create user", err)
		return
	}
	h.audit(r, store.AuthAuditEntry{TenantID: tid, Action: "user_create", TargetUserID: u.ID, Detail: "role=" + u.Role})

	resp := createUserResponse{User: newUserView(u)}
	if temporary {
		resp.TemporaryPassword = plain
	}
	httpx.WriteJSON(w, http.StatusCreated, resp)
}

// List handles GET /api/v1/users (optional ?role=admin|viewer|platform-admin).
func (h Users) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	opts := store.UserListOptions{Role: r.URL.Query().Get("role")}
	if opts.Role != "" {
		if err := validateUserRole(opts.Role); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	}
	users, err := h.Store.Users().List(r.Context(), tid, opts)
	if err != nil {
		writeStoreErr(w, r, "list users", err)
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, newUserView(u))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, httpx.ParsePagination(r)), Total: len(views)})
}

// Get handles GET /api/v1/users/{id}.
func (h Users) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	u, err := h.Store.Users().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "get user", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newUserView(u))
}

type updateUserRequest struct {
	DisplayName *string `json:"display_name"`
	Role        *string `json:"role"`
	Disabled    *bool   `json:"disabled"`
}

// guardLastAdmin writes the 409 and returns false when u is an enabled
// admin and is the tenant's only one, i.e. the change about to be made
// (disable, delete, demote) would leave the tenant without an admin.
func (h Users) guardLastAdmin(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	if u.Role != store.UserRoleAdmin || u.Disabled {
		return true
	}
	n, err := h.Store.Users().CountActiveAdmins(r.Context(), u.TenantID)
	if err != nil {
		writeStoreErr(w, r, "count active admins", err)
		return false
	}
	if n <= 1 {
		httpx.WriteError(w, http.StatusConflict, typeLastAdmin, "cannot disable, delete or demote the last active admin of the tenant")
		return false
	}
	return true
}

// guardLastPlatformAdmin is guardLastAdmin's platform-wide twin: it writes
// the 409 and returns false when u is an enabled platform-admin and the
// only active one in the whole deployment.
func (h Users) guardLastPlatformAdmin(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	if !isPlatformAdmin(u) || u.Disabled {
		return true
	}
	n, err := h.Store.Users().CountActivePlatformAdmins(r.Context())
	if err != nil {
		writeStoreErr(w, r, "count active platform admins", err)
		return false
	}
	if n <= 1 {
		httpx.WriteError(w, http.StatusConflict, typeLastPlatformAdmin, "cannot disable, delete or demote the last active platform-admin")
		return false
	}
	return true
}

func selfActionConflict(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusConflict, typeSelfAction, "you cannot disable, delete or demote your own account")
}

// Update handles PATCH /api/v1/users/{id}: display_name, role and
// disabled. The username is immutable (an unknown field, so a 400). You
// cannot disable or demote yourself (409 self_action), nobody can disable
// or demote a tenant's last active admin (409 last_admin) or the
// deployment's last active platform-admin (409 last_platform_admin), and
// a patch that gives or touches the platform-admin role needs
// platform.admin (403). Promoting a tenant's only admin to platform-admin
// is allowed: it keeps the admin grants. Disabling revokes the user's
// sessions.
func (h Users) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var req updateUserRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if req.DisplayName == nil && req.Role == nil && req.Disabled == nil {
		httpx.ValidationError(w, "nothing to update: set at least one of display_name, role, disabled")
		return
	}
	if req.Role != nil {
		if err := validateUserRole(*req.Role); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	}
	if req.DisplayName != nil {
		if err := validateDisplayName(*req.DisplayName); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	}

	u, err := h.Store.Users().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "update user", err)
		return
	}
	if isPlatformAdmin(u) && !h.requirePlatformAdmin(w, r, "editing a platform-admin user") {
		return
	}
	if req.Role != nil && *req.Role == store.UserRolePlatformAdmin && !isPlatformAdmin(u) &&
		!h.requirePlatformAdmin(w, r, "assigning role platform-admin") {
		return
	}

	newDisabled := u.Disabled
	if req.Disabled != nil {
		newDisabled = *req.Disabled
	}
	newRole := u.Role
	if req.Role != nil {
		newRole = *req.Role
	}
	disabling := newDisabled && !u.Disabled
	// "demoting" is a role change that takes admin grants away: admin ->
	// viewer, or platform-admin -> anything else. admin -> platform-admin
	// is not one: the user keeps every admin grant, so a tenant whose only
	// admin is promoted still has an admin-capable user, even though
	// CountActiveAdmins (role admin only) then reads zero for it.
	demoting := (u.Role == store.UserRoleAdmin && newRole != store.UserRoleAdmin && newRole != store.UserRolePlatformAdmin) ||
		(isPlatformAdmin(u) && newRole != u.Role)
	if disabling || demoting {
		if id == selfID(r) {
			selfActionConflict(w)
			return
		}
		if !h.guardLastAdmin(w, r, u) || !h.guardLastPlatformAdmin(w, r, u) {
			return
		}
	}

	var changes []string
	if req.DisplayName != nil && strings.TrimSpace(*req.DisplayName) != u.DisplayName {
		u.DisplayName = strings.TrimSpace(*req.DisplayName)
		changes = append(changes, "display_name")
	}
	if newRole != u.Role {
		changes = append(changes, "role:"+u.Role+"->"+newRole)
		u.Role = newRole
	}
	wasDisabled := u.Disabled
	u.Disabled = newDisabled
	if err := h.Store.Users().Update(r.Context(), u); err != nil {
		writeStoreErr(w, r, "update user", err)
		return
	}

	switch {
	case disabling:
		if _, err := h.Store.UserSessions().RevokeAllForUser(r.Context(), u.ID, ""); err != nil {
			applog.From(r.Context()).Error("update user: revoke sessions failed", "user_id", u.ID, "error", err)
		}
		h.audit(r, store.AuthAuditEntry{TenantID: tid, Action: "user_disable", TargetUserID: u.ID})
	case wasDisabled && !newDisabled:
		changes = append(changes, "enabled")
	}
	if len(changes) > 0 {
		h.audit(r, store.AuthAuditEntry{TenantID: tid, Action: "user_update", TargetUserID: u.ID, Detail: strings.Join(changes, ",")})
	}
	httpx.WriteJSON(w, http.StatusOK, newUserView(u))
}

// Delete handles DELETE /api/v1/users/{id} (soft). Same self, last-admin,
// last-platform-admin and platform.admin guards as Update; revokes the
// user's sessions.
func (h Users) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.Store.Users().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "delete user", err)
		return
	}
	if isPlatformAdmin(u) && !h.requirePlatformAdmin(w, r, "deleting a platform-admin user") {
		return
	}
	if id == selfID(r) {
		selfActionConflict(w)
		return
	}
	if !h.guardLastAdmin(w, r, u) || !h.guardLastPlatformAdmin(w, r, u) {
		return
	}
	if err := h.Store.Users().SoftDelete(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "delete user", err)
		return
	}
	if _, err := h.Store.UserSessions().RevokeAllForUser(r.Context(), id, ""); err != nil {
		applog.From(r.Context()).Error("delete user: revoke sessions failed", "user_id", id, "error", err)
	}
	h.audit(r, store.AuthAuditEntry{TenantID: tid, Action: "user_delete", TargetUserID: id, Detail: "username=" + u.Username})
	w.WriteHeader(http.StatusNoContent)
}

type resetPasswordResponse struct {
	TemporaryPassword string `json:"temporary_password"`
}

// ResetPassword handles POST /api/v1/users/{id}/reset-password: sets a new
// random temporary password (returned once), forces a change at next
// login, and revokes all of the user's sessions. There is no email
// reset: this, or `gateway reset-password`, is the only way. Resetting a
// platform-admin needs platform.admin.
func (h Users) ResetPassword(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.Store.Users().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "reset password", err)
		return
	}
	if isPlatformAdmin(u) && !h.requirePlatformAdmin(w, r, "resetting a platform-admin user's password") {
		return
	}
	temp, err := password.GenerateTemporary()
	if err != nil {
		applog.From(r.Context()).Error("reset password: generate failed", "error", err)
		httpx.Internal(w, "internal error")
		return
	}
	hash, err := password.Hash(temp)
	if err != nil {
		applog.From(r.Context()).Error("reset password: hash failed", "error", err)
		httpx.Internal(w, "internal error")
		return
	}
	if err := h.Store.Users().SetPassword(r.Context(), tid, u.ID, hash, true); err != nil {
		writeStoreErr(w, r, "reset password", err)
		return
	}
	if _, err := h.Store.UserSessions().RevokeAllForUser(r.Context(), u.ID, ""); err != nil {
		applog.From(r.Context()).Error("reset password: revoke sessions failed", "user_id", u.ID, "error", err)
		httpx.Internal(w, "internal error")
		return
	}
	h.audit(r, store.AuthAuditEntry{TenantID: tid, Action: "password_reset", TargetUserID: u.ID})
	httpx.WriteJSON(w, http.StatusOK, resetPasswordResponse{TemporaryPassword: temp})
}

// RevokeSessions handles POST /api/v1/users/{id}/revoke-sessions: logs the
// user out everywhere. Revoking a platform-admin's sessions needs
// platform.admin.
func (h Users) RevokeSessions(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.Store.Users().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "revoke sessions", err)
		return
	}
	if isPlatformAdmin(u) && !h.requirePlatformAdmin(w, r, "revoking a platform-admin user's sessions") {
		return
	}
	n, err := h.Store.UserSessions().RevokeAllForUser(r.Context(), id, "")
	if err != nil {
		writeStoreErr(w, r, "revoke sessions", err)
		return
	}
	h.audit(r, store.AuthAuditEntry{TenantID: tid, Action: "session_revoke", TargetUserID: id, Detail: "revoked=" + strconv.Itoa(n)})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------

type auditEntryView struct {
	ID        int64  `json:"id"`
	ActorKind string `json:"actor_kind"`
	ActorID   string `json:"actor_id"`
	// ActorTenantID is set when the actor belonged to another tenant (a
	// platform-admin acting here); empty for a local actor.
	ActorTenantID string `json:"actor_tenant_id,omitempty"`
	Action        string `json:"action"`
	TargetUserID  string `json:"target_user_id,omitempty"`
	IP            string `json:"ip,omitempty"`
	Detail        string `json:"detail,omitempty"`
	At            string `json:"at"`
}

// Audit handles GET /api/v1/auth/audit: the tenant's authentication audit
// trail, newest first. Optional ?action= and ?user_id= filters plus the
// usual limit/offset.
func (h Users) Audit(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	page := httpx.ParsePagination(r)
	opts := store.AuthAuditListOptions{
		Limit: page.Limit, Offset: page.Offset,
		Action: r.URL.Query().Get("action"), TargetUserID: r.URL.Query().Get("user_id"),
	}
	if opts.TargetUserID != "" {
		if _, err := uuid.Parse(opts.TargetUserID); err != nil {
			httpx.ValidationError(w, "user_id must be a UUID")
			return
		}
	}
	entries, total, err := h.Store.AuthAudit().List(r.Context(), tid, opts)
	if err != nil {
		writeStoreErr(w, r, "list auth audit", err)
		return
	}
	views := make([]auditEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, auditEntryView{
			ID: e.ID, ActorKind: e.ActorKind, ActorID: e.ActorID, ActorTenantID: e.ActorTenantID, Action: e.Action,
			TargetUserID: e.TargetUserID, IP: e.IP, Detail: e.Detail, At: formatTime(e.At),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: views, Total: total})
}
