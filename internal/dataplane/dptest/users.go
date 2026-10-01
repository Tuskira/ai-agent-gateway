package dptest

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type userStore Store

func (u *userStore) Create(_ context.Context, nu *store.User) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.users {
		if e.DeletedAt == nil && e.TenantID == nu.TenantID && e.Username == nu.Username {
			return store.ErrConflict
		}
	}
	now := time.Now()
	nu.ID = uuid.NewString() // real UUIDs: the router rejects a non-UUID {id} before any handler runs
	nu.PasswordChangedAt, nu.CreatedAt, nu.UpdatedAt = now, now, now
	cp := *nu
	s.users[nu.ID] = &cp
	return nil
}

// live returns the stored, non-deleted user id in tenantID, or nil.
func (u *userStore) live(tenantID, id string) *store.User {
	s := (*Store)(u)
	e, ok := s.users[id]
	if !ok || e.DeletedAt != nil || e.TenantID != tenantID {
		return nil
	}
	return e
}

func (u *userStore) Get(_ context.Context, tenantID, id string) (*store.User, error) {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := u.live(tenantID, id)
	if e == nil {
		return nil, store.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (u *userStore) GetByUsername(_ context.Context, tenantID, username string) (*store.User, error) {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.users {
		if e.DeletedAt == nil && e.TenantID == tenantID && e.Username == username {
			cp := *e
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (u *userStore) List(_ context.Context, tenantID string, opts store.UserListOptions) ([]*store.User, error) {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*store.User{}
	for _, e := range s.users {
		if e.DeletedAt != nil || e.TenantID != tenantID || (opts.Role != "" && e.Role != opts.Role) {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (u *userStore) Update(_ context.Context, nu *store.User) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := u.live(nu.TenantID, nu.ID)
	if e == nil {
		return store.ErrNotFound
	}
	e.DisplayName, e.Role, e.Disabled = nu.DisplayName, nu.Role, nu.Disabled
	e.UpdatedAt = time.Now()
	nu.UpdatedAt = e.UpdatedAt
	return nil
}

func (u *userStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := u.live(tenantID, id)
	if e == nil {
		return store.ErrNotFound
	}
	now := time.Now()
	e.DeletedAt, e.UpdatedAt = &now, now
	return nil
}

func (u *userStore) SetPassword(_ context.Context, tenantID, id, hash string, mustChange bool) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := u.live(tenantID, id)
	if e == nil {
		return store.ErrNotFound
	}
	now := time.Now()
	e.PasswordHash, e.MustChangePassword = hash, mustChange
	e.PasswordChangedAt, e.UpdatedAt = now, now
	e.FailedLogins, e.LockedUntil = 0, nil
	return nil
}

func (u *userStore) RecordLoginFailure(_ context.Context, tenantID, id string, maxFailures int, lockout time.Duration, at time.Time) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := u.live(tenantID, id)
	if e == nil {
		return store.ErrNotFound
	}
	if e.FailedLogins+1 >= maxFailures {
		until := at.Add(lockout)
		e.LockedUntil, e.FailedLogins = &until, 0
		return nil
	}
	e.FailedLogins++
	return nil
}

func (u *userStore) RecordLoginSuccess(_ context.Context, tenantID, id string, at time.Time) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := u.live(tenantID, id)
	if e == nil {
		return store.ErrNotFound
	}
	e.FailedLogins, e.LockedUntil = 0, nil
	e.LastLoginAt = &at
	return nil
}

func (u *userStore) CountActiveAdmins(_ context.Context, tenantID string) (int, error) {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.users {
		if e.DeletedAt == nil && e.TenantID == tenantID && e.Role == store.UserRoleAdmin && !e.Disabled {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------

type userSessionStore Store

func (us *userSessionStore) Create(_ context.Context, ns *store.UserSession) error {
	s := (*Store)(us)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[ns.ID]; ok {
		return store.ErrConflict
	}
	now := time.Now()
	ns.CreatedAt, ns.LastSeenAt = now, now
	cp := *ns
	s.sessions[ns.ID] = &cp
	return nil
}

func (us *userSessionStore) Get(_ context.Context, id string) (*store.UserSession, error) {
	s := (*Store)(us)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (us *userSessionStore) Touch(_ context.Context, id string, lastSeen time.Time) error {
	s := (*Store)(us)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return store.ErrNotFound
	}
	e.LastSeenAt = lastSeen
	return nil
}

func (us *userSessionStore) Revoke(_ context.Context, id string) error {
	s := (*Store)(us)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return store.ErrNotFound
	}
	if e.RevokedAt == nil {
		now := time.Now()
		e.RevokedAt = &now
	}
	return nil
}

func (us *userSessionStore) RevokeAllForUser(_ context.Context, userID, exceptID string) (int, error) {
	s := (*Store)(us)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	now := time.Now()
	for _, e := range s.sessions {
		if e.UserID == userID && e.RevokedAt == nil && e.ID != exceptID {
			t := now
			e.RevokedAt = &t
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------

type authAuditStore Store

func (a *authAuditStore) Append(_ context.Context, e *store.AuthAuditEntry) error {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.ID = int64(len(s.audit) + 1)
	cp := *e
	s.audit = append(s.audit, &cp)
	return nil
}

func (a *authAuditStore) List(_ context.Context, tenantID string, opts store.AuthAuditListOptions) ([]*store.AuthAuditEntry, int, error) {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []*store.AuthAuditEntry
	for _, e := range s.audit {
		if e.TenantID != tenantID || (opts.Action != "" && e.Action != opts.Action) ||
			(opts.TargetUserID != "" && e.TargetUserID != opts.TargetUserID) {
			continue
		}
		cp := *e
		matched = append(matched, &cp)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].At.Equal(matched[j].At) {
			return matched[i].At.After(matched[j].At)
		}
		return matched[i].ID > matched[j].ID
	})
	total := len(matched)
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	off := max(opts.Offset, 0)
	if off > total {
		off = total
	}
	end := min(off+limit, total)
	out := matched[off:end]
	if out == nil {
		out = []*store.AuthAuditEntry{}
	}
	return out, total, nil
}
