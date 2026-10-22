// Command gateway is the tusk-ai-secured-gateway binary: it starts the MCP,
// API, and LLM proxy planes (whichever are enabled) behind a shared
// supervisor and a Postgres connection. The serving path lives in
// pkg/gateway (Run); this file parses the flags and holds the one-shot
// subcommands (migrate, bootstrap-key, create-user, reset-password,
// secrets).
//
// A fresh install is set up in two steps: `bootstrap-key` creates the
// first tenant and its platform-admin API key (the key alone goes to
// stdout), then `create-user -role platform-admin` creates the console
// account that can list tenants and act in any of them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	// Blank-imported for its init() side effect: registers driver
	// "postgres" with pkg/store, which the subcommands open. pkg/gateway
	// imports it too, for Run.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/gateway"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// version is the build's service version, set at link time via
// -ldflags "-X main.version=...". Defaults to "dev" for local builds.
var version = gateway.DefaultVersion

// dbConnectTimeout bounds each subcommand's database connect and query.
const dbConnectTimeout = 10 * time.Second

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version)
			return
		case "migrate":
			if err := runMigrate(); err != nil {
				slog.Error("migrate failed", "error", err)
				os.Exit(1)
			}
			return
		case "bootstrap-key":
			if err := runBootstrapKey(); err != nil {
				fmt.Fprintln(os.Stderr, "bootstrap-key: "+err.Error())
				os.Exit(1)
			}
			return
		case "create-user":
			if err := runCreateUser(); err != nil {
				fmt.Fprintln(os.Stderr, "create-user: "+err.Error())
				os.Exit(1)
			}
			return
		case "reset-password":
			if err := runResetPassword(); err != nil {
				fmt.Fprintln(os.Stderr, "reset-password: "+err.Error())
				os.Exit(1)
			}
			return
		case "secrets":
			if err := runSecrets(); err != nil {
				fmt.Fprintln(os.Stderr, "secrets: "+err.Error())
				os.Exit(1)
			}
			return
		}
	}

	if err := run(); err != nil {
		slog.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
}

// runMigrate loads config, connects to the store, applies every pending
// migration, and exits. It is the `gateway migrate` subcommand: the same
// path main() takes when database.migrate: true, runnable standalone
// (e.g. in a Kubernetes init container, ahead of a rolling deploy).
func runMigrate() error {
	configPath := flag.String("config", os.Getenv("CONFIG_PATH"), "path to YAML config file")
	_ = flag.CommandLine.Parse(os.Args[2:])

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := applog.New(cfg.Logging)
	slog.SetDefault(logger)

	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	st, err := store.Open(ctx, cfg.Database.Driver, cfg.Database)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	logger.Info("migrations applied", "driver", cfg.Database.Driver)
	return nil
}

// run is the default command: serve every enabled plane until SIGINT or
// SIGTERM (see pkg/gateway.Run).
func run() error {
	configPath := flag.String("config", os.Getenv("CONFIG_PATH"), "path to YAML config file")
	flag.Parse()

	return gateway.Run(context.Background(), gateway.Options{ConfigPath: *configPath, Version: version})
}

// runSecrets implements the `gateway secrets <genkey|rekey>` subcommands.
func runSecrets() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: gateway secrets <genkey|rekey> [flags]")
	}
	switch os.Args[2] {
	case "genkey":
		return runSecretsGenKey()
	case "rekey":
		return runSecretsRekey()
	default:
		return fmt.Errorf("unknown secrets subcommand %q (want genkey or rekey)", os.Args[2])
	}
}

// runSecretsGenKey implements `gateway secrets genkey`: it prints a fresh
// base64-encoded 32-byte key to stdout and nothing else, so it can be
// captured directly, e.g. GATEWAY_MASTER_KEY=$(gateway secrets genkey).
func runSecretsGenKey() error {
	fmt.Println(secrets.GenerateKey())
	return nil
}

// runSecretsRekey implements `gateway secrets rekey --tenant <slug|all>`:
// it re-encrypts every credential of the named tenant (or every tenant,
// for "all") under the key ring's current active key id. Used after
// rotating the master key -- add the new key to the ring alongside the
// old one, set active_key_id to the new one, then run this so every row
// is rewritten under it; once done, the old key id can be safely dropped
// from the ring.
//
// Output discipline mirrors bootstrap-key: stdout is reserved for nothing
// in particular here (there's no secret to capture), so progress goes to
// stderr like every other diagnostic message.
func runSecretsRekey() error {
	fs := flag.NewFlagSet("secrets rekey", flag.ExitOnError)
	tenantFlag := fs.String("tenant", "", `tenant slug to re-encrypt, or "all"`)
	configPath := fs.String("config", os.Getenv("CONFIG_PATH"), "path to YAML config file")
	if err := fs.Parse(os.Args[3:]); err != nil {
		return err
	}
	if strings.TrimSpace(*tenantFlag) == "" {
		return fmt.Errorf(`--tenant is required (a tenant slug, or "all")`)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	dbCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	st, err := store.Open(dbCtx, cfg.Database.Driver, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()

	migrateCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	if err := st.Migrate(migrateCtx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	ring, err := secrets.LoadKeyRing(cfg.SecretStore)
	if err != nil {
		return fmt.Errorf("load master key ring: %w", err)
	}
	svc := secrets.NewService(st.Credentials(), ring)

	lookupCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	type tenantRef struct{ id, slug string }
	var tenants []tenantRef

	if *tenantFlag == "all" {
		all, err := st.Tenants().List(lookupCtx)
		if err != nil {
			return fmt.Errorf("list tenants: %w", err)
		}
		for _, tn := range all {
			tenants = append(tenants, tenantRef{id: tn.ID, slug: tn.Slug})
		}
	} else {
		tn, err := st.Tenants().GetBySlug(lookupCtx, *tenantFlag)
		if err != nil {
			return fmt.Errorf("look up tenant %q: %w", *tenantFlag, err)
		}
		tenants = append(tenants, tenantRef{id: tn.ID, slug: tn.Slug})
	}

	var total int
	for _, tn := range tenants {
		opCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
		n, err := svc.ReEncryptAll(opCtx, tn.id)
		cancel()
		if err != nil {
			return fmt.Errorf("re-encrypt tenant %q: %w", tn.slug, err)
		}
		fmt.Fprintf(os.Stderr, "tenant %q: re-encrypted %d credential(s) under key %q\n", tn.slug, n, ring.ActiveKeyID)
		total += n
	}
	fmt.Fprintf(os.Stderr, "done: %d credential(s) re-encrypted across %d tenant(s) under key %q\n", total, len(tenants), ring.ActiveKeyID)
	return nil
}

// runBootstrapKey implements the `bootstrap-key` subcommand: it loads
// config, opens (and migrates) the store, ensures the target tenant
// exists, and issues it a new admin API key.
//
// Output discipline matters here: stdout carries exactly one line, the
// new key's plaintext, so a caller can capture it cleanly (e.g.
// KEY=$(gateway bootstrap-key)); every other message -- tenant creation,
// refusals, the "copy this now" reminder -- goes to stderr.
func runBootstrapKey() error {
	fs := flag.NewFlagSet("bootstrap-key", flag.ExitOnError)
	tenant := fs.String("tenant", "default", "tenant slug/name to bootstrap")
	name := fs.String("name", "bootstrap-admin", "name recorded on the created API key")
	force := fs.Bool("force", false, "create a new admin key even if the tenant already has a non-revoked one")
	platform := fs.Bool("platform", false, "give the key the platform-admin role (tenant enumeration/creation, platform catalog) as well as admin; implied for the very first key in the database")
	configPath := fs.String("config", os.Getenv("CONFIG_PATH"), "path to YAML config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// stdout is reserved for the plaintext key alone (see doc comment
	// above); point even the process-default logger at stderr so nothing
	// a dependency logs via slog.Default() can leak onto stdout.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	dbCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	st, err := store.Open(dbCtx, cfg.Database.Driver, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()

	migrateCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	if err := st.Migrate(migrateCtx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	opCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	tenantRow, err := st.Tenants().GetBySlug(opCtx, *tenant)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("look up tenant %q: %w", *tenant, err)
		}
		tenantRow = &store.Tenant{Slug: *tenant, Name: *tenant}
		if err := st.Tenants().Create(opCtx, tenantRow); err != nil {
			return fmt.Errorf("create tenant %q: %w", *tenant, err)
		}
		fmt.Fprintf(os.Stderr, "created tenant %q (id=%s)\n", *tenant, tenantRow.ID)
	} else {
		fmt.Fprintf(os.Stderr, "using existing tenant %q (id=%s)\n", *tenant, tenantRow.ID)
	}

	// The first key ever created (no tenant had any key) is the install's
	// owner: it gets platform-admin too, or nobody could ever create
	// another tenant. Every later key is a tenant admin unless --platform.
	firstKeyEver, err := noKeysExist(opCtx, st)
	if err != nil {
		return err
	}
	withPlatform := *platform || firstKeyEver

	if !*force {
		existing, err := st.APIKeys().List(opCtx, tenantRow.ID)
		if err != nil {
			return fmt.Errorf("list existing api keys: %w", err)
		}
		for _, k := range existing {
			if (k.Role == "admin" || k.Role == pkgauth.RolePlatformAdmin) && k.RevokedAt == nil {
				return fmt.Errorf("tenant %q already has a non-revoked admin key (id=%s, name=%q); pass --force to create another", *tenant, k.ID, k.Name)
			}
		}
	}

	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	role := "admin"
	if withPlatform {
		role = pkgauth.RolePlatformAdmin
	}
	k := &store.APIKey{
		TenantID:  tenantRow.ID,
		Name:      *name,
		Role:      role,
		KeyHash:   hash,
		KeyPrefix: prefix,
		CreatedBy: "bootstrap-key",
	}
	if err := st.APIKeys().Create(opCtx, k); err != nil {
		return fmt.Errorf("create api key: %w", err)
	}

	if withPlatform {
		fmt.Fprintf(os.Stderr, "created admin + platform-admin api key %q for tenant %q (id=%s, prefix=%s)\n", *name, *tenant, k.ID, prefix)
		fmt.Fprintf(os.Stderr, "for a console login that can act in every tenant, next run: gateway create-user -tenant %s -username <email> -role platform-admin\n", *tenant)
	} else {
		fmt.Fprintf(os.Stderr, "created admin api key %q for tenant %q (id=%s, prefix=%s); it is a tenant admin only -- pass --platform for platform-admin\n", *name, *tenant, k.ID, prefix)
	}
	fmt.Fprintln(os.Stderr, "copy the key below now -- it will not be shown again:")
	fmt.Println(plaintext)

	return nil
}

// noKeysExist reports whether the database holds no API key at all (any
// tenant, revoked or not): the "first key ever" test bootstrap-key uses to
// decide whether the new key is also a platform admin.
func noKeysExist(ctx context.Context, st store.Store) (bool, error) {
	tenants, err := st.Tenants().List(ctx)
	if err != nil {
		return false, fmt.Errorf("list tenants: %w", err)
	}
	for _, t := range tenants {
		keys, err := st.APIKeys().List(ctx, t.ID)
		if err != nil {
			return false, fmt.Errorf("list api keys: %w", err)
		}
		if len(keys) > 0 {
			return false, nil
		}
	}
	return true, nil
}
