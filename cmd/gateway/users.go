package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/user"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// openStoreForCLI loads config, opens and migrates the store. It points the
// process-default logger at stderr: like bootstrap-key, the user commands
// reserve stdout for one thing (the temporary password) so a caller can
// capture it, and everything else goes to stderr.
func openStoreForCLI(configPath string) (store.Store, func(), error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	dbCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	st, err := store.Open(dbCtx, cfg.Database.Driver, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := st.Migrate(dbCtx); err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("apply migrations: %w", err)
	}
	return st, func() { st.Close() }, nil
}

// cliActor is who the audit trail should blame: the OS user running the
// command.
func cliActor() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// runCreateUser implements `gateway create-user`: create a console user
// with a generated temporary password (printed once to stdout; the user
// must change it at first login). The tenant must already exist (see
// bootstrap-key). Audited with actor_kind "cli".
func runCreateUser() error {
	fs := flag.NewFlagSet("create-user", flag.ExitOnError)
	tenant := fs.String("tenant", "", "slug of the tenant the user belongs to (required)")
	username := fs.String("username", "", "username, lowercase; an email address is fine (required)")
	role := fs.String("role", "", "admin | viewer (required)")
	displayName := fs.String("display-name", "", "name shown in the console")
	configPath := fs.String("config", os.Getenv("CONFIG_PATH"), "path to YAML config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *tenant == "" || *username == "" || *role == "" {
		return errors.New("-tenant, -username and -role are required")
	}
	name, err := store.ValidateUsername(*username)
	if err != nil {
		return err
	}
	if *role != store.UserRoleAdmin && *role != store.UserRoleViewer {
		return errors.New("-role must be admin or viewer")
	}

	st, closeStore, err := openStoreForCLI(*configPath)
	if err != nil {
		return err
	}
	defer closeStore()

	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	t, err := st.Tenants().GetBySlug(ctx, *tenant)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("tenant %q does not exist (create it first, e.g. with `gateway bootstrap-key -tenant %s`)", *tenant, *tenant)
		}
		return fmt.Errorf("look up tenant %q: %w", *tenant, err)
	}

	temp, err := password.GenerateTemporary()
	if err != nil {
		return err
	}
	hash, err := password.Hash(temp)
	if err != nil {
		return err
	}
	u := &store.User{
		TenantID: t.ID, Username: name, DisplayName: *displayName, PasswordHash: hash,
		Role: *role, MustChangePassword: true,
	}
	if err := st.Users().Create(ctx, u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("user %q already exists in tenant %q", name, *tenant)
		}
		return fmt.Errorf("create user: %w", err)
	}
	if err := st.AuthAudit().Append(ctx, &store.AuthAuditEntry{
		TenantID: t.ID, ActorKind: "cli", ActorID: cliActor(), Action: "user_create",
		TargetUserID: u.ID, Detail: "role=" + u.Role,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write the audit entry: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "created %s %q in tenant %q (id=%s)\n", u.Role, u.Username, *tenant, u.ID)
	fmt.Fprintln(os.Stderr, "temporary password below -- shown once; the user must change it at first login:")
	fmt.Println(temp)
	return nil
}

// runResetPassword implements `gateway reset-password`: the break-glass
// path when no admin can log in. It sets a new temporary password (printed
// once to stdout), forces a change at next login, and revokes all of the
// user's sessions. Audited with actor_kind "cli".
func runResetPassword() error {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	tenant := fs.String("tenant", "", "slug of the user's tenant (required)")
	username := fs.String("username", "", "username (required)")
	configPath := fs.String("config", os.Getenv("CONFIG_PATH"), "path to YAML config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *tenant == "" || *username == "" {
		return errors.New("-tenant and -username are required")
	}
	name := store.NormalizeUsername(*username)

	st, closeStore, err := openStoreForCLI(*configPath)
	if err != nil {
		return err
	}
	defer closeStore()

	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	t, err := st.Tenants().GetBySlug(ctx, *tenant)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("tenant %q does not exist", *tenant)
		}
		return fmt.Errorf("look up tenant %q: %w", *tenant, err)
	}
	u, err := st.Users().GetByUsername(ctx, t.ID, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("user %q does not exist in tenant %q", name, *tenant)
		}
		return fmt.Errorf("look up user: %w", err)
	}

	temp, err := password.GenerateTemporary()
	if err != nil {
		return err
	}
	hash, err := password.Hash(temp)
	if err != nil {
		return err
	}
	if err := st.Users().SetPassword(ctx, t.ID, u.ID, hash, true); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	revoked, err := st.UserSessions().RevokeAllForUser(ctx, u.ID, "")
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if err := st.AuthAudit().Append(ctx, &store.AuthAuditEntry{
		TenantID: t.ID, ActorKind: "cli", ActorID: cliActor(), Action: "password_reset", TargetUserID: u.ID,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write the audit entry: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "reset the password of %q in tenant %q; revoked %d session(s)\n", u.Username, *tenant, revoked)
	if u.Disabled {
		fmt.Fprintln(os.Stderr, "note: this user is disabled and still cannot log in")
	}
	fmt.Fprintln(os.Stderr, "temporary password below -- shown once; the user must change it at next login:")
	fmt.Println(temp)
	return nil
}
