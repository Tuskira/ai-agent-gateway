package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type usersEnv struct {
	*authEnv
	h     Users
	admin *store.User // alice, admin in tenant A
	sess  *store.UserSession
}

func newUsersEnv(t *testing.T) *usersEnv {
	t.Helper()
	env := newAuthEnv(t)
	alice := env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	_, s := env.session(t, alice)
	return &usersEnv{authEnv: env, h: Users{Deps: env.deps}, admin: alice, sess: s}
}

// asAdmin performs the request as alice, the logged-in admin.
func (e *usersEnv) asAdmin(r *http.Request) *http.Request { return asUser(r, e.admin, e.sess) }

func (e *usersEnv) do(method, pattern, target, body string, h http.HandlerFunc, wrap func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = jsonReq(method, target, body)
	}
	return serve(method, pattern, h, wrap(r))
}

// ---------------------------------------------------------------------------
// POST /users
// ---------------------------------------------------------------------------

func TestUsers_Create_GeneratesTemporaryPassword(t *testing.T) {
	e := newUsersEnv(t)
	w := e.do(http.MethodPost, "/users", "/users", `{"username":"Bob@Example.com","display_name":"Bob","role":"viewer"}`, e.h.Create, e.asAdmin)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got createUserResponse
	decodeInto(t, w, &got)
	if got.User.Username != "bob@example.com" || got.User.Role != "viewer" || got.User.DisplayName != "Bob" ||
		!got.User.MustChangePassword || got.User.Disabled || got.User.ID == "" {
		t.Errorf("user = %+v", got.User)
	}
	if len(got.TemporaryPassword) != password.TempLength {
		t.Fatalf("temporary_password = %q, want %d characters", got.TemporaryPassword, password.TempLength)
	}
	// The hash never leaves the server, and the temp password works.
	if strings.Contains(w.Body.String(), "argon2") || strings.Contains(w.Body.String(), "password_hash") {
		t.Errorf("response leaks the hash: %s", w.Body.String())
	}
	stored, _ := e.fs.Users().GetByUsername(context.Background(), e.tenantA.ID, "bob@example.com")
	if ok, _ := password.Verify(got.TemporaryPassword, stored.PasswordHash); !ok || !stored.MustChangePassword || stored.TenantID != e.tenantA.ID {
		t.Errorf("stored user = %+v; want the temp password, must-change, tenant A", stored)
	}
	if a := hasAudit(e.audit(t, e.tenantA.ID), "user_create"); a == nil || a.ActorKind != "user" || a.ActorID != e.admin.ID || a.TargetUserID != stored.ID {
		t.Errorf("user_create audit = %+v", a)
	}
}

func TestUsers_Create_WithExplicitPassword(t *testing.T) {
	e := newUsersEnv(t)
	w := e.do(http.MethodPost, "/users", "/users", `{"username":"carol","role":"admin","password":"carols-own-password"}`, e.h.Create, e.asAdmin)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got createUserResponse
	decodeInto(t, w, &got)
	if got.TemporaryPassword != "" || got.User.MustChangePassword {
		t.Errorf("response = %+v; an admin-chosen password is neither echoed nor forced to change", got)
	}
	stored, _ := e.fs.Users().GetByUsername(context.Background(), e.tenantA.ID, "carol")
	if ok, _ := password.Verify("carols-own-password", stored.PasswordHash); !ok {
		t.Error("stored hash does not match the chosen password")
	}
}

func TestUsers_Create_Validation(t *testing.T) {
	e := newUsersEnv(t)
	for name, body := range map[string]string{
		"role missing":          `{"username":"dave"}`,
		"role not allowed":      `{"username":"dave","role":"agent"}`,
		"role platform admin":   `{"username":"dave","role":"platform-admin"}`,
		"username too short":    `{"username":"ab","role":"viewer"}`,
		"username with spaces":  `{"username":"da ve","role":"viewer"}`,
		"username leading dash": `{"username":"-dave","role":"viewer"}`,
		"username empty":        `{"username":"","role":"viewer"}`,
		"password too short":    `{"username":"dave","role":"viewer","password":"short"}`,
		"password is username":  `{"username":"dave-the-user","role":"viewer","password":"Dave-The-User"}`,
		"display name too long": `{"username":"dave","role":"viewer","display_name":"` + strings.Repeat("x", 129) + `"}`,
		"unknown field":         `{"username":"dave","role":"viewer","tenant_id":"other"}`,
	} {
		if w := e.do(http.MethodPost, "/users", "/users", body, e.h.Create, e.asAdmin); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body = %s", name, w.Code, w.Body.String())
		}
	}
}

func TestUsers_Create_DuplicateUsernameConflicts(t *testing.T) {
	e := newUsersEnv(t)
	w := e.do(http.MethodPost, "/users", "/users", `{"username":"ALICE","role":"viewer"}`, e.h.Create, e.asAdmin)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "conflict")
	// The same name in another tenant is fine.
	e.user(t, e.tenantB.ID, "zed", store.UserRoleViewer, pwBob, false)
	if w := e.do(http.MethodPost, "/users", "/users", `{"username":"zed","role":"viewer"}`, e.h.Create, e.asAdmin); w.Code != http.StatusCreated {
		t.Errorf("same username in another tenant: status = %d, want 201", w.Code)
	}
}

func TestUsers_Create_ByAdminAPIKeyIsAuditedAsAPIKey(t *testing.T) {
	e := newUsersEnv(t)
	asKey := func(r *http.Request) *http.Request { return asAPIKey(r, e.tenantA.ID, "admin") }
	w := e.do(http.MethodPost, "/users", "/users", `{"username":"erin","role":"viewer"}`, e.h.Create, asKey)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if a := hasAudit(e.audit(t, e.tenantA.ID), "user_create"); a == nil || a.ActorKind != "api_key" || a.ActorID != "key-1" {
		t.Errorf("audit = %+v, want actor api_key/key-1", a)
	}
}

// ---------------------------------------------------------------------------
// GET /users, GET /users/{id}
// ---------------------------------------------------------------------------

func TestUsers_List_IsTenantScoped(t *testing.T) {
	e := newUsersEnv(t)
	e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)
	e.user(t, e.tenantB.ID, "gary", store.UserRoleAdmin, pwBob, false)

	w := e.do(http.MethodGet, "/users", "/users", "", e.h.List, e.asAdmin)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page struct {
		Items []userView `json:"items"`
		Total int        `json:"total"`
	}
	decodeInto(t, w, &page)
	if page.Total != 2 || len(page.Items) != 2 || page.Items[0].Username != "alice" || page.Items[1].Username != "bob" {
		t.Errorf("list = %+v, want alice and bob only (no tenant B users)", page)
	}
	if strings.Contains(w.Body.String(), "argon2") {
		t.Error("list leaks a password hash")
	}

	w = e.do(http.MethodGet, "/users", "/users?role=viewer", "", e.h.List, e.asAdmin)
	decodeInto(t, w, &page)
	if page.Total != 1 || page.Items[0].Username != "bob" {
		t.Errorf("list ?role=viewer = %+v, want only bob", page)
	}
	if w := e.do(http.MethodGet, "/users", "/users?role=root", "", e.h.List, e.asAdmin); w.Code != http.StatusBadRequest {
		t.Errorf("?role=root status = %d, want 400", w.Code)
	}
}

func TestUsers_Get_CrossTenantIs404(t *testing.T) {
	e := newUsersEnv(t)
	gary := e.user(t, e.tenantB.ID, "gary", store.UserRoleAdmin, pwBob, false)
	bob := e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)

	if w := e.do(http.MethodGet, "/users/{id}", "/users/"+bob.ID, "", e.h.Get, e.asAdmin); w.Code != http.StatusOK {
		t.Errorf("own-tenant get: status = %d", w.Code)
	}
	for name, run := range map[string]func() *httptest.ResponseRecorder{
		"get": func() *httptest.ResponseRecorder {
			return e.do(http.MethodGet, "/users/{id}", "/users/"+gary.ID, "", e.h.Get, e.asAdmin)
		},
		"patch": func() *httptest.ResponseRecorder {
			return e.do(http.MethodPatch, "/users/{id}", "/users/"+gary.ID, `{"disabled":true}`, e.h.Update, e.asAdmin)
		},
		"delete": func() *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, "/users/{id}", "/users/"+gary.ID, "", e.h.Delete, e.asAdmin)
		},
		"reset-password": func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/users/{id}/reset-password", "/users/"+gary.ID+"/reset-password", "", e.h.ResetPassword, e.asAdmin)
		},
		"revoke-sessions": func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/users/{id}/revoke-sessions", "/users/"+gary.ID+"/revoke-sessions", "", e.h.RevokeSessions, e.asAdmin)
		},
	} {
		w := run()
		if w.Code != http.StatusNotFound {
			t.Errorf("%s of another tenant's user: status = %d, want 404, body = %s", name, w.Code, w.Body.String())
		}
	}
	g, _ := e.fs.Users().Get(context.Background(), e.tenantB.ID, gary.ID)
	if g.Disabled || g.DeletedAt != nil {
		t.Error("a cross-tenant request modified the other tenant's user")
	}
}

// ---------------------------------------------------------------------------
// PATCH /users/{id}
// ---------------------------------------------------------------------------

func TestUsers_Update_Fields(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)

	w := e.do(http.MethodPatch, "/users/{id}", "/users/"+bob.ID, `{"display_name":"Robert","role":"admin"}`, e.h.Update, e.asAdmin)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got userView
	decodeInto(t, w, &got)
	if got.DisplayName != "Robert" || got.Role != "admin" || got.Username != "bob" || got.Disabled {
		t.Errorf("user = %+v", got)
	}
	if a := hasAudit(e.audit(t, e.tenantA.ID), "user_update"); a == nil || !strings.Contains(a.Detail, "role:viewer->admin") || !strings.Contains(a.Detail, "display_name") {
		t.Errorf("user_update audit = %+v", a)
	}
}

func TestUsers_Update_Validation(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)
	for name, body := range map[string]string{
		"empty patch":        `{}`,
		"bad role":           `{"role":"root"}`,
		"username immutable": `{"username":"robert"}`,
		"password via patch": `{"password":"some-new-password"}`,
		"tenant via patch":   `{"tenant_id":"x"}`,
		"display name long":  `{"display_name":"` + strings.Repeat("y", 129) + `"}`,
	} {
		if w := e.do(http.MethodPatch, "/users/{id}", "/users/"+bob.ID, body, e.h.Update, e.asAdmin); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body = %s", name, w.Code, w.Body.String())
		}
	}
}

func TestUsers_Update_DisableRevokesSessionsAndEnableRestores(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)
	_, bobSess := e.session(t, bob)

	w := e.do(http.MethodPatch, "/users/{id}", "/users/"+bob.ID, `{"disabled":true}`, e.h.Update, e.asAdmin)
	if w.Code != http.StatusOK {
		t.Fatalf("disable: status = %d, body = %s", w.Code, w.Body.String())
	}
	if s, _ := e.fs.UserSessions().Get(context.Background(), bobSess.ID); s.RevokedAt == nil {
		t.Error("disabling a user left their session alive")
	}
	if hasAudit(e.audit(t, e.tenantA.ID), "user_disable") == nil {
		t.Error("no user_disable audit row")
	}
	w = e.do(http.MethodPatch, "/users/{id}", "/users/"+bob.ID, `{"disabled":false}`, e.h.Update, e.asAdmin)
	var got userView
	decodeInto(t, w, &got)
	if w.Code != http.StatusOK || got.Disabled {
		t.Errorf("enable: status = %d, user = %+v", w.Code, got)
	}
}

func TestUsers_Update_SelfGuard(t *testing.T) {
	e := newUsersEnv(t)
	// A second admin, so the last-admin guard is not what stops alice.
	e.user(t, e.tenantA.ID, "root2", store.UserRoleAdmin, pwBob, false)
	for name, body := range map[string]string{
		"disable self": `{"disabled":true}`,
		"demote self":  `{"role":"viewer"}`,
	} {
		w := e.do(http.MethodPatch, "/users/{id}", "/users/"+e.admin.ID, body, e.h.Update, e.asAdmin)
		if w.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409, body = %s", name, w.Code, w.Body.String())
			continue
		}
		assertErrorType(t, w, "self_action")
	}
	// Harmless self edits are fine.
	if w := e.do(http.MethodPatch, "/users/{id}", "/users/"+e.admin.ID, `{"display_name":"Alice A.","role":"admin","disabled":false}`, e.h.Update, e.asAdmin); w.Code != http.StatusOK {
		t.Errorf("harmless self edit: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestUsers_Update_LastAdminGuard(t *testing.T) {
	e := newUsersEnv(t)
	asKey := func(r *http.Request) *http.Request { return asAPIKey(r, e.tenantA.ID, "admin") }

	// alice is the only admin; even an admin API key cannot remove her.
	for name, body := range map[string]string{"disable": `{"disabled":true}`, "demote": `{"role":"viewer"}`} {
		w := e.do(http.MethodPatch, "/users/{id}", "/users/"+e.admin.ID, body, e.h.Update, asKey)
		if w.Code != http.StatusConflict {
			t.Errorf("%s last admin: status = %d, want 409, body = %s", name, w.Code, w.Body.String())
			continue
		}
		assertErrorType(t, w, "last_admin")
	}
	// With a second admin, the first may be demoted; then the second is last.
	root2 := e.user(t, e.tenantA.ID, "root2", store.UserRoleAdmin, pwBob, false)
	if w := e.do(http.MethodPatch, "/users/{id}", "/users/"+e.admin.ID, `{"role":"viewer"}`, e.h.Update, asKey); w.Code != http.StatusOK {
		t.Fatalf("demote with a second admin present: status = %d, body = %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodPatch, "/users/{id}", "/users/"+root2.ID, `{"disabled":true}`, e.h.Update, asKey); w.Code != http.StatusConflict {
		t.Errorf("disabling the now-last admin: status = %d, want 409", w.Code)
	}
	// A viewer or an already-disabled admin never counts as "the last admin".
	if w := e.do(http.MethodPatch, "/users/{id}", "/users/"+e.admin.ID, `{"disabled":true}`, e.h.Update, asKey); w.Code != http.StatusOK {
		t.Errorf("disabling a viewer: status = %d, want 200", w.Code)
	}
}

// ---------------------------------------------------------------------------
// DELETE /users/{id}
// ---------------------------------------------------------------------------

func TestUsers_Delete(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)
	_, bobSess := e.session(t, bob)

	w := e.do(http.MethodDelete, "/users/{id}", "/users/"+bob.ID, "", e.h.Delete, e.asAdmin)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if _, err := e.fs.Users().Get(context.Background(), e.tenantA.ID, bob.ID); err != store.ErrNotFound {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	if s, _ := e.fs.UserSessions().Get(context.Background(), bobSess.ID); s.RevokedAt == nil {
		t.Error("deleting a user left their session alive")
	}
	if a := hasAudit(e.audit(t, e.tenantA.ID), "user_delete"); a == nil || a.TargetUserID != bob.ID {
		t.Errorf("user_delete audit = %+v", a)
	}
	if w := e.do(http.MethodDelete, "/users/{id}", "/users/"+bob.ID, "", e.h.Delete, e.asAdmin); w.Code != http.StatusNotFound {
		t.Errorf("deleting twice: status = %d, want 404", w.Code)
	}
	// The username is free again.
	if w := e.do(http.MethodPost, "/users", "/users", `{"username":"bob","role":"viewer"}`, e.h.Create, e.asAdmin); w.Code != http.StatusCreated {
		t.Errorf("re-create after delete: status = %d, want 201", w.Code)
	}
}

func TestUsers_Delete_Guards(t *testing.T) {
	e := newUsersEnv(t)
	root2 := e.user(t, e.tenantA.ID, "root2", store.UserRoleAdmin, pwBob, false)

	// Self: refused even though another admin exists.
	w := e.do(http.MethodDelete, "/users/{id}", "/users/"+e.admin.ID, "", e.h.Delete, e.asAdmin)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete self: status = %d, want 409", w.Code)
	}
	assertErrorType(t, w, "self_action")

	// Last admin: alice (as a user) may delete root2 now, but then root2 is gone and
	// an API key trying to delete alice hits the guard.
	if w := e.do(http.MethodDelete, "/users/{id}", "/users/"+root2.ID, "", e.h.Delete, e.asAdmin); w.Code != http.StatusNoContent {
		t.Fatalf("delete the other admin: status = %d", w.Code)
	}
	asKey := func(r *http.Request) *http.Request { return asAPIKey(r, e.tenantA.ID, "admin") }
	w = e.do(http.MethodDelete, "/users/{id}", "/users/"+e.admin.ID, "", e.h.Delete, asKey)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete last admin: status = %d, want 409", w.Code)
	}
	assertErrorType(t, w, "last_admin")
}

// ---------------------------------------------------------------------------
// POST /users/{id}/reset-password, /revoke-sessions
// ---------------------------------------------------------------------------

func TestUsers_ResetPassword(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)
	_, s1 := e.session(t, bob)
	_, s2 := e.session(t, bob)
	_, aliceSess := e.session(t, e.admin)

	w := e.do(http.MethodPost, "/users/{id}/reset-password", "/users/"+bob.ID+"/reset-password", "", e.h.ResetPassword, e.asAdmin)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got resetPasswordResponse
	decodeInto(t, w, &got)
	if len(got.TemporaryPassword) != password.TempLength {
		t.Fatalf("temporary_password = %q", got.TemporaryPassword)
	}
	u, _ := e.fs.Users().Get(context.Background(), e.tenantA.ID, bob.ID)
	if ok, _ := password.Verify(got.TemporaryPassword, u.PasswordHash); !ok || !u.MustChangePassword {
		t.Errorf("after reset: hash matches temp = %v, must_change = %v", ok, u.MustChangePassword)
	}
	if ok, _ := password.Verify(pwBob, u.PasswordHash); ok {
		t.Error("the old password still works after a reset")
	}
	for _, id := range []string{s1.ID, s2.ID} {
		if s, _ := e.fs.UserSessions().Get(context.Background(), id); s.RevokedAt == nil {
			t.Error("a reset left one of the user's sessions alive")
		}
	}
	if s, _ := e.fs.UserSessions().Get(context.Background(), aliceSess.ID); s.RevokedAt != nil {
		t.Error("resetting bob revoked alice's session")
	}
	if a := hasAudit(e.audit(t, e.tenantA.ID), "password_reset"); a == nil || a.TargetUserID != bob.ID || a.ActorID != e.admin.ID {
		t.Errorf("password_reset audit = %+v", a)
	}
	if strings.Contains(w.Body.String(), "argon2") {
		t.Error("reset response leaks a hash")
	}
}

func TestUsers_ResetPassword_ByAdminAPIKey(t *testing.T) {
	e := newUsersEnv(t)
	asKey := func(r *http.Request) *http.Request { return asAPIKey(r, e.tenantA.ID, "admin") }
	w := e.do(http.MethodPost, "/users/{id}/reset-password", "/users/"+e.admin.ID+"/reset-password", "", e.h.ResetPassword, asKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if a := hasAudit(e.audit(t, e.tenantA.ID), "password_reset"); a == nil || a.ActorKind != "api_key" || a.ActorID != "key-1" {
		t.Errorf("audit = %+v, want actor api_key/key-1", a)
	}
}

func TestUsers_RevokeSessions(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.user(t, e.tenantA.ID, "bob", store.UserRoleViewer, pwBob, false)
	_, s1 := e.session(t, bob)
	w := e.do(http.MethodPost, "/users/{id}/revoke-sessions", "/users/"+bob.ID+"/revoke-sessions", "", e.h.RevokeSessions, e.asAdmin)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if s, _ := e.fs.UserSessions().Get(context.Background(), s1.ID); s.RevokedAt == nil {
		t.Error("session not revoked")
	}
	if hasAudit(e.audit(t, e.tenantA.ID), "session_revoke") == nil {
		t.Error("no session_revoke audit row")
	}
}

// ---------------------------------------------------------------------------
// GET /auth/audit
// ---------------------------------------------------------------------------

func TestUsers_Audit(t *testing.T) {
	e := newUsersEnv(t)
	e.do(http.MethodPost, "/users", "/users", `{"username":"bob","role":"viewer"}`, e.h.Create, e.asAdmin)
	e.do(http.MethodPost, "/users", "/users", `{"username":"carol","role":"viewer"}`, e.h.Create, e.asAdmin)
	// An event in another tenant must not show up.
	_ = e.fs.AuthAudit().Append(context.Background(), &store.AuthAuditEntry{TenantID: e.tenantB.ID, ActorKind: "cli", Action: "user_create"})

	w := e.do(http.MethodGet, "/auth/audit", "/auth/audit", "", e.h.Audit, e.asAdmin)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var page struct {
		Items []auditEntryView `json:"items"`
		Total int              `json:"total"`
	}
	decodeInto(t, w, &page)
	if page.Total != 2 || len(page.Items) != 2 || page.Items[0].Action != "user_create" || page.Items[0].ActorKind != "user" || page.Items[0].ActorID != e.admin.ID || page.Items[0].At == "" {
		t.Errorf("audit page = %+v", page)
	}

	w = e.do(http.MethodGet, "/auth/audit", "/auth/audit?limit=1&offset=1&action=user_create", "", e.h.Audit, e.asAdmin)
	decodeInto(t, w, &page)
	if page.Total != 2 || len(page.Items) != 1 {
		t.Errorf("paged audit = %+v, want 1 of 2", page)
	}
	if w := e.do(http.MethodGet, "/auth/audit", "/auth/audit?user_id=not-a-uuid", "", e.h.Audit, e.asAdmin); w.Code != http.StatusBadRequest {
		t.Errorf("bad user_id: status = %d, want 400", w.Code)
	}
}
