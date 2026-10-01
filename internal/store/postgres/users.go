package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type userStore struct{ db *sql.DB }

var _ store.UserStore = (*userStore)(nil)

const userColumns = `id, tenant_id, username, display_name, password_hash, role, must_change_password, disabled,
	failed_logins, locked_until, last_login_at, password_changed_at, created_at, updated_at, deleted_at`

func scanUserRow(row rowScanner) (*store.User, error) {
	var u store.User
	if err := row.Scan(
		&u.ID, &u.TenantID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.MustChangePassword, &u.Disabled,
		&u.FailedLogins, &u.LockedUntil, &u.LastLoginAt, &u.PasswordChangedAt, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt,
	); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *userStore) Create(ctx context.Context, u *store.User) error {
	const q = `
		INSERT INTO users (tenant_id, username, display_name, password_hash, role, must_change_password, disabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, password_changed_at, created_at, updated_at`
	err := s.db.QueryRowContext(ctx, q, u.TenantID, u.Username, u.DisplayName, u.PasswordHash, u.Role, u.MustChangePassword, u.Disabled).
		Scan(&u.ID, &u.PasswordChangedAt, &u.CreatedAt, &u.UpdatedAt)
	return mapWriteErr(err)
}

func (s *userStore) Get(ctx context.Context, tenantID, id string) (*store.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	u, err := scanUserRow(s.db.QueryRowContext(ctx, q, tenantID, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return u, nil
}

func (s *userStore) GetByUsername(ctx context.Context, tenantID, username string) (*store.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE tenant_id = $1 AND username = $2 AND deleted_at IS NULL`
	u, err := scanUserRow(s.db.QueryRowContext(ctx, q, tenantID, username))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return u, nil
}

func (s *userStore) List(ctx context.Context, tenantID string, opts store.UserListOptions) ([]*store.User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE tenant_id = $1 AND deleted_at IS NULL`
	args := []any{tenantID}
	if opts.Role != "" {
		q += ` AND role = $2`
		args = append(args, opts.Role)
	}
	q += ` ORDER BY username`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list users: %w", err)
	}
	defer rows.Close()

	out := []*store.User{}
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *userStore) Update(ctx context.Context, u *store.User) error {
	const q = `
		UPDATE users SET display_name = $3, role = $4, disabled = $5, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	err := s.db.QueryRowContext(ctx, q, u.TenantID, u.ID, u.DisplayName, u.Role, u.Disabled).Scan(&u.UpdatedAt)
	return mapReadErr(err)
}

func (s *userStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE users SET deleted_at = now(), updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id)
}

func (s *userStore) SetPassword(ctx context.Context, tenantID, id, hash string, mustChange bool) error {
	const q = `
		UPDATE users SET password_hash = $3, must_change_password = $4, password_changed_at = now(),
			failed_logins = 0, locked_until = NULL, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id, hash, mustChange)
}

func (s *userStore) RecordLoginFailure(ctx context.Context, tenantID, id string, maxFailures int, lockout time.Duration, at time.Time) error {
	// A single statement, so concurrent failures can't lose an increment.
	// SET expressions all read the pre-update row, hence failed_logins + 1.
	const q = `
		UPDATE users SET
			locked_until  = CASE WHEN failed_logins + 1 >= $3 THEN $5::timestamptz + make_interval(secs => $4) ELSE locked_until END,
			failed_logins = CASE WHEN failed_logins + 1 >= $3 THEN 0 ELSE failed_logins + 1 END
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id, maxFailures, lockout.Seconds(), at)
}

func (s *userStore) RecordLoginSuccess(ctx context.Context, tenantID, id string, at time.Time) error {
	const q = `
		UPDATE users SET failed_logins = 0, locked_until = NULL, last_login_at = $3
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id, at)
}

func (s *userStore) CountActiveAdmins(ctx context.Context, tenantID string) (int, error) {
	const q = `SELECT count(*) FROM users WHERE tenant_id = $1 AND role = 'admin' AND NOT disabled AND deleted_at IS NULL`
	var n int
	if err := s.db.QueryRowContext(ctx, q, tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count active admins: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

type userSessionStore struct{ db *sql.DB }

var _ store.UserSessionStore = (*userSessionStore)(nil)

func (s *userSessionStore) Create(ctx context.Context, us *store.UserSession) error {
	const q = `
		INSERT INTO user_sessions (id, user_id, tenant_id, csrf_token, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, last_seen_at`
	err := s.db.QueryRowContext(ctx, q, us.ID, us.UserID, us.TenantID, us.CSRFToken, us.ExpiresAt, us.UserAgent, us.IP).
		Scan(&us.CreatedAt, &us.LastSeenAt)
	return mapWriteErr(err)
}

func (s *userSessionStore) Get(ctx context.Context, id string) (*store.UserSession, error) {
	const q = `
		SELECT id, user_id, tenant_id, csrf_token, created_at, last_seen_at, expires_at, revoked_at, user_agent, ip
		FROM user_sessions WHERE id = $1`
	var us store.UserSession
	if err := s.db.QueryRowContext(ctx, q, id).Scan(
		&us.ID, &us.UserID, &us.TenantID, &us.CSRFToken, &us.CreatedAt, &us.LastSeenAt, &us.ExpiresAt, &us.RevokedAt, &us.UserAgent, &us.IP,
	); err != nil {
		return nil, mapReadErr(err)
	}
	return &us, nil
}

func (s *userSessionStore) Touch(ctx context.Context, id string, lastSeen time.Time) error {
	const q = `UPDATE user_sessions SET last_seen_at = $2 WHERE id = $1`
	return execExpectingOneRow(ctx, s.db, q, id, lastSeen)
}

func (s *userSessionStore) Revoke(ctx context.Context, id string) error {
	// COALESCE keeps the original revocation time on a repeat call.
	const q = `UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`
	return execExpectingOneRow(ctx, s.db, q, id)
}

func (s *userSessionStore) RevokeAllForUser(ctx context.Context, userID, exceptID string) (int, error) {
	const q = `UPDATE user_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL AND id <> $2`
	res, err := s.db.ExecContext(ctx, q, userID, exceptID)
	if err != nil {
		return 0, fmt.Errorf("postgres: revoke user sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("postgres: rows affected: %w", err)
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

type authAuditStore struct{ db *sql.DB }

var _ store.AuthAuditStore = (*authAuditStore)(nil)

func (s *authAuditStore) Append(ctx context.Context, e *store.AuthAuditEntry) error {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	const q = `
		INSERT INTO auth_audit (tenant_id, actor_kind, actor_id, action, target_user_id, ip, detail, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, at`
	err := s.db.QueryRowContext(ctx, q, nullableTenant(e.TenantID), e.ActorKind, e.ActorID, e.Action, nullableTenant(e.TargetUserID), e.IP, e.Detail, at).
		Scan(&e.ID, &e.At)
	return mapWriteErr(err)
}

func (s *authAuditStore) List(ctx context.Context, tenantID string, opts store.AuthAuditListOptions) ([]*store.AuthAuditEntry, int, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	where := ` WHERE tenant_id = $1`
	args := []any{tenantID}
	if opts.Action != "" {
		args = append(args, opts.Action)
		where += fmt.Sprintf(` AND action = $%d`, len(args))
	}
	if opts.TargetUserID != "" {
		args = append(args, opts.TargetUserID)
		where += fmt.Sprintf(` AND target_user_id = $%d`, len(args))
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM auth_audit`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("postgres: count auth audit: %w", err)
	}

	args = append(args, limit, max(opts.Offset, 0))
	q := `SELECT id, COALESCE(tenant_id::text, ''), actor_kind, actor_id, action, COALESCE(target_user_id::text, ''), ip, detail, at
		FROM auth_audit` + where + fmt.Sprintf(` ORDER BY at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: list auth audit: %w", err)
	}
	defer rows.Close()

	out := []*store.AuthAuditEntry{}
	for rows.Next() {
		var e store.AuthAuditEntry
		if err := rows.Scan(&e.ID, &e.TenantID, &e.ActorKind, &e.ActorID, &e.Action, &e.TargetUserID, &e.IP, &e.Detail, &e.At); err != nil {
			return nil, 0, err
		}
		out = append(out, &e)
	}
	return out, total, rows.Err()
}
