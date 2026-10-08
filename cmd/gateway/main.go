// Command gateway is the tusk-ai-secured-gateway binary: it starts the MCP,
// API, and LLM proxy planes (whichever are enabled) behind a shared
// supervisor and a Postgres connection.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/handlers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/ui"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/devmode"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/capture"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/clientip"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/httpplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/mcpcatalog"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/metrics"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	// Blank-imported for its init() side effect: registers driver
	// "postgres" with pkg/store. See pkg/store.Open.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	// Blank-imported for their init() side effects: register the LLM
	// translation adapters -- dialect "anthropic", providers "anthropic" and
	// "openai_compat" -- with pkg/llm (a model registry target of another
	// wire format than the client's is served through them), and every
	// Reader the detection tee reads a route's bodies with (anthropic,
	// openai, gemini and bedrock wire formats).
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/anthropic"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/bedrock"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/gemini"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/openaicompat"
	// Blank-imported for its init() side effect: registers metrics driver
	// "prometheus" with pkg/metrics ("none" is built in). See
	// pkg/metrics.Open.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/prometheus"
	// Blank-imported for their init() side effects: register session
	// drivers "memory" and "redis" with pkg/session. See pkg/session.Open.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/redis"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	fsbodystore "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/bodystore/fs"
	s3bodystore "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/bodystore/s3"
	chsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/clickhouse"
	otelsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/otel"
	pgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/postgres"
	stdoutsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/stdout"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// version is the build's service version, set at link time via
// -ldflags "-X main.version=...". Defaults to "dev" for local builds.
var version = "dev"

const (
	dbConnectTimeout   = 10 * time.Second
	defaultReadTimeout = 30 * time.Second
	defaultIdleTimeout = 120 * time.Second
	// detectionDrainTimeout bounds posting the detection tee's queued turns
	// at shutdown; what is left after it is dropped.
	detectionDrainTimeout = 5 * time.Second
)

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

func run() error {
	configPath := flag.String("config", os.Getenv("CONFIG_PATH"), "path to YAML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.Service.Version = version

	logger := applog.New(cfg.Logging)
	slog.SetDefault(logger)
	if unknown := config.UnknownEnvVars(cfg); len(unknown) > 0 {
		logger.Warn("ignoring GATEWAY_* environment variables that match no config key (typo?)", "vars", unknown)
	}

	if msg := cfg.Database.DefaultPasswordWarning(); msg != "" {
		logger.Warn("INSECURE CONFIGURATION: " + msg)
	}

	// Metrics come first so every later component can record through
	// them, and close last (deferred first) so a push driver flushes what
	// the shutdown itself recorded.
	gwMetrics, err := metrics.New(context.Background(), cfg.Metrics, version, logger)
	if err != nil {
		return fmt.Errorf("open metrics: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := gwMetrics.Close(ctx); err != nil {
			logger.Warn("metrics: shutdown", "error", err)
		}
	}()
	if gwMetrics.Enabled() {
		logger.Info("metrics enabled", "driver", cfg.Metrics.Driver, "address", cfg.Metrics.Address, "path", cfg.Metrics.Path)
	}

	dbCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	st, err := store.Open(dbCtx, cfg.Database.Driver, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()
	logger.Info("connected to database", "driver", cfg.Database.Driver, "host", cfg.Database.Host, "port", cfg.Database.Port, "database", cfg.Database.Database)

	if cfg.Database.Migrate {
		migrateCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
		defer cancel()
		if err := st.Migrate(migrateCtx); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		logger.Info("migrations applied")
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	if err := st.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	lockoutCtx, cancelLockout := context.WithTimeout(context.Background(), dbConnectTimeout)
	warnAdminlessTenants(lockoutCtx, st, cfg.Auth.ConsoleAPIKeyLogin, logger)
	cancelLockout()

	// llm_proxy.models: upsert the configured platform model-registry rows
	// (idempotent; every replica may do it). Done whatever planes this
	// process serves, so an api-only pod with the same config file seeds
	// the same rows the LLM pods resolve.
	if len(cfg.LLMProxy.Models) > 0 {
		seedCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
		defer cancel()
		if _, _, err := llmplane.SeedModels(seedCtx, st.Models(), cfg.LLMProxy.Models, logger); err != nil {
			return fmt.Errorf("seed model registry: %w", err)
		}
	}

	// mcp_catalog.seed: upsert the platform MCP catalog entries by slug
	// (idempotent; every replica may do it).
	if len(cfg.MCPCatalog.Seed) > 0 {
		seedCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
		defer cancel()
		if _, _, err := mcpcatalog.Seed(seedCtx, st.MCPCatalog(), cfg.MCPCatalog.Seed, logger); err != nil {
			return fmt.Errorf("seed mcp catalog: %w", err)
		}
	}

	// skills.seed_dir: upsert the skills & commands registry's platform
	// rows from a directory of skill bundles (idempotent; every replica
	// may do it). Empty (the default) skips this entirely.
	if cfg.Skills.SeedDir != "" {
		seedCtx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
		defer cancel()
		if _, _, err := skills.SeedSkills(seedCtx, st.Skills(), cfg.Skills.SeedDir, logger); err != nil {
			return fmt.Errorf("seed skills registry: %w", err)
		}
	}

	// The secret store is needed unconditionally today (every deployment
	// gets a Service and header Registry, even before any plane route
	// consumes them), so a missing/invalid master key is always fatal at
	// startup, not just when some future secret-store-dependent route is
	// enabled.
	secDeps, err := buildSecretsDeps(cfg, st, logger)
	if err != nil {
		return fmt.Errorf("initialize secret store: %w", err)
	}
	// secDeps.Service and secDeps.Registry feed the API plane's
	// credentials/connectors/headers routes below; secDeps.Service also
	// resolves the credentials model-registry targets name.

	// The model registry resolver is shared by the LLM plane (resolution)
	// and the API plane (cache invalidation on /models writes) when both
	// run in this process; either plane works without the other.
	modelRegistry := llmplane.NewRegistry(st.Models(), secDeps.Service, llmplane.RegistryOptions{})

	if cfg.Auth.DevMode.Enabled {
		if err := resolveDevTenant(cfg, st, logger); err != nil {
			return err
		}
	}

	// clientIPs is THE client-IP resolver: every plane's limiter and every
	// captured client_ip go through it (api.trusted_proxies; validated by
	// config.Validate, so the error cannot happen here).
	clientIPs, err := clientip.New(cfg.API.TrustedProxies)
	if err != nil {
		return fmt.Errorf("api.trusted_proxies: %w", err)
	}
	if !clientIPs.Trusts() {
		logger.Info("api.trusted_proxies is empty: X-Forwarded-For is ignored and clients are keyed on the TCP peer address; behind a load balancer or ingress set it, or all clients share one rate-limit identity")
	}

	// Auth-failure limiters, one PER PLANE (in-memory, so per process): each
	// has its own per-IP failure lockout and per-credential-prefix lockout,
	// so bad credentials on one plane never lock a client out of another
	// (a wrong key in an MCP client must not stop model calls). All three
	// use the same thresholds; only the API plane applies the general
	// requests-per-minute cap. See internal/auth/ratelimit.go.
	newPlaneLimiter := func(plane string, rpm int) *internalauth.RateLimiter {
		return internalauth.NewRateLimiter(internalauth.RateLimiterConfig{
			Plane:             plane,
			Enabled:           cfg.Auth.RateLimit.Enabled,
			MaxFailures:       cfg.Auth.RateLimit.MaxFailures,
			Window:            cfg.Auth.RateLimit.Window,
			Lockout:           cfg.Auth.RateLimit.Lockout,
			RequestsPerMinute: rpm,
			CredMaxFailures:   cfg.Auth.RateLimit.CredentialMaxFailures,
			CredWindow:        cfg.Auth.RateLimit.CredentialWindow,
			Resolver:          clientIPs,
			Logger:            logger,
		})
	}
	rateLimiter := newPlaneLimiter("api", cfg.Auth.RateLimit.RequestsPerMinute)
	defer rateLimiter.Close()
	mcpRateLimiter := newPlaneLimiter("mcp", 0)
	defer mcpRateLimiter.Close()
	llmRateLimiter := newPlaneLimiter("llm", 0)
	defer llmRateLimiter.Close()

	authenticator, apiAuthenticator, apiKeyAuthenticator := buildAuthenticator(cfg, st, logger)
	// The MCP and LLM planes refuse an IP locked out for failed auth with a
	// 429 in their own error format, before any credential lookup.
	mcpAuthenticator := internalauth.PlaneRateLimitedAuthenticator(authenticator, mcpRateLimiter)
	llmAuthenticator := internalauth.PlaneRateLimitedAuthenticator(authenticator, llmRateLimiter)
	authorizer := pkgauth.NewRoleAuthorizer()
	// cfg.Auth.Roles (config.Auth.Roles' doc comment) is merged over the
	// built-ins here, after NewRoleAuthorizer seeds admin/agent: a
	// configured role sharing a built-in's name replaces it outright.
	for name, patterns := range cfg.Auth.Roles {
		authorizer.Rules[name] = patterns
	}
	// SingleTenant backs the one dynamic permission carve-out
	// RoleAuthorizer.Allow applies (platform.catalog.manage for a console
	// admin) -- the same "exactly one tenant" test GET /auth/config uses
	// for its own single_tenant field (handlers.Auth.defaultTenant).
	authorizer.SingleTenant = func(ctx context.Context) bool {
		tenants, err := st.Tenants().List(ctx)
		return err == nil && len(tenants) == 1
	}
	authMiddleware := internalauth.MiddlewareWith(llmAuthenticator, logger, llmplane.WriteRateLimited)

	// ingestRateLimiter guards POST /api/v1/ingest only: a per-API-key cap
	// (not per-IP -- see internal/api/router.go's isIngestPath and
	// internal/auth.KeyRateLimiter's doc comment for why). Built
	// unconditionally; it's a no-op cost when ingest is disabled or
	// RatePerMinute is left at its default.
	ingestRateLimiter := internalauth.NewKeyRateLimiter(cfg.Ingest.RatePerMinute, nil)

	sinks, err := buildSinks(cfg, gwMetrics, logger)
	if err != nil {
		return fmt.Errorf("build sinks: %w", err)
	}
	defer sinks.Sink.Close()

	// The body store (llm_proxy.capture.body_store) is built regardless of
	// which planes run: the LLM plane offloads bodies into it, and the API
	// plane resolves body_refs out of it for the LLM-log detail -- in the
	// per-plane deployment those are different processes. nil = inline.
	bodyStore, err := buildBodyStore(cfg, logger)
	if err != nil {
		return fmt.Errorf("build llm body store: %w", err)
	}
	if bodyStore != nil {
		defer bodyStore.Close()
	}
	// recorder is set below when the LLM plane is enabled; GET /health reads
	// its offload counters (zero on an API-only instance).
	var recorder *capture.Recorder
	// limiter is set below when the LLM plane is enabled (per-key limits);
	// GET /health reads its denial counters.
	var limiter *llmplane.Limiter
	sinksStatus := sinks.Status
	if bodyStore != nil {
		sinksStatus = func() map[string]any {
			out := sinks.Status()
			var offloaded, fallbacks uint64
			if recorder != nil {
				offloaded, fallbacks = recorder.Offloaded(), recorder.Fallbacks()
			}
			out["body_store"] = map[string]any{
				"type": cfg.LLMProxy.Capture.BodyStore.Type, "offloaded": offloaded, "fallbacks": fallbacks,
			}
			return out
		}
	}

	// The LLM cost rate card: embedded by default, optionally overridden
	// by llm_proxy.pricing_file. Built unconditionally (not gated on
	// cfg.LLMProxy.Enabled) because pricingSource also feeds the API
	// plane's GET /analytics/overview -- a malformed override file is
	// fatal at startup either way, never silently ignored.
	pricingCard, pricingSource, err := buildPricingCard(cfg.LLMProxy.PricingFile, logger)
	if err != nil {
		return fmt.Errorf("load llm pricing file: %w", err)
	}

	var routines []supervisor.Routine

	// The metrics listener goes FIRST: routines stop in reverse order, so
	// it stops last and stays scrapeable while the planes drain.
	if r := gwMetrics.Routine(); r != nil {
		routines = append(routines, r)
	}
	nonPlaneRoutines := len(routines)

	// Cross-replica key revocation: when the store can push key-revoked
	// events (Postgres LISTEN/NOTIFY), every process evicts a revoked key
	// from its cache at once. Without it, other processes wait out
	// auth.api_keys.cache_ttl.
	if apiKeyAuthenticator != nil {
		if n, ok := st.(store.RevocationNotifier); ok {
			routines = append(routines, newRevocationRoutine(apiKeyAuthenticator, n, logger))
		} else {
			logger.Warn("store has no revocation notifier: a revoked API key stays valid on other replicas for up to auth.api_keys.cache_ttl", "ttl", cfg.Auth.APIKeys.CacheTTL)
		}
	}

	// connectorOps and cacheOps back the API plane's connector
	// health/discover and cache-management routes (pkg/ops), and come from
	// the data plane -- so the data plane is built whenever EITHER plane
	// needs it. With api.enabled && !mcp.enabled (the per-plane Kubernetes
	// deployment: deploy/k8s runs gateway-api with GATEWAY_MCP_ENABLED=false)
	// it is built for those ops only, which must not crash and must not
	// start anything: no :8080 listener, and none of its background
	// routines, which the mcp replicas own. They stay nil -- and those
	// routes answer 503 -- only when neither plane is enabled.
	var connectorOps ops.ConnectorOps
	var cacheOps ops.CacheOps
	var profileOps ops.ProfileOps

	if cfg.MCP.Enabled || cfg.API.Enabled {
		mcpPlane, err := dataplane.New(dataplane.Deps{
			Config:        cfg,
			Store:         st,
			Headers:       secDeps.Registry,
			Authenticator: mcpAuthenticator,
			Authorizer:    authorizer,
			Sink:          sinks.Sink,
			Logger:        logger,
			ClientIP:      clientIPs.IP,
		})
		if err != nil {
			return fmt.Errorf("build mcp plane: %w", err)
		}
		// dataplane.New only assembles structs -- its tickers and
		// goroutines all live in Plane.Routines, appended below -- so
		// holding a Plane costs nothing until one of them is started.
		defer mcpPlane.Close()
		connectorOps = mcpPlane.ConnectorOps
		cacheOps = mcpPlane.CacheOps
		profileOps = mcpPlane.ProfileOps

		if cfg.MCP.Enabled {
			routines = append(routines, httpplane.New(
				"mcp", cfg.MCP.Address, gwMetrics.Instrument("mcp", metrics.MCPRoute)(mcpPlane.Handler),
				cfg.MCP.ReadTimeout, cfg.MCP.WriteTimeout, defaultIdleTimeout, logger,
			))
			routines = append(routines, mcpPlane.Routines...)
			logger.Info("plane enabled", "plane", "mcp", "addr", cfg.MCP.Address,
				"require_profile", cfg.MCP.RequireProfile, "tool_cache", cfg.ToolCache.Enabled)
		}
	}

	if cfg.API.Enabled {
		// The rate limiter's failure/success reporting is wired onto the
		// API plane's authenticator (RateLimitMiddleware gates locked IPs
		// ahead of it, with route exemptions); the MCP and LLM planes use
		// planeAuthenticator, which also enforces the lock itself.
		// apiAuthenticator is also the only chain that accepts the console
		// session cookie: the data planes take API keys only (a cookie is
		// not port-scoped, and they have no CSRF check).
		apiDeps := api.Deps{
			Instrument:     gwMetrics.Instrument("api", metrics.APIRoute),
			ServiceVersion: cfg.Service.Version,
			Authenticator:  internalauth.RateLimitedAuthenticator(apiAuthenticator, rateLimiter),
			Authorizer:     authorizer,
			Logger:         logger,
			Store:          st,
			Secrets:        secDeps.Service,
			Headers:        secDeps.Registry,
			ConnectorOps:   connectorOps,
			CacheOps:       cacheOps,
			ProfileOps:     profileOps,
			Analytics:      sinks.Analytics,
			AnalyticsMeta:  analytics.Meta{PricingSource: pricingSource},
			BodyStore:      bodyStore,
			LLMCalls:       llmCallReader(cfg, sinks.Analytics, logger),
			SinksStatus:    sinksStatus,
			ServeUI:        cfg.API.ServeUI,
			RateLimiter:    rateLimiter,
			Console: handlers.ConsoleConfig{
				APIKeyLogin:   cfg.Auth.ConsoleAPIKeyLogin,
				DefaultTenant: cfg.Auth.DefaultTenant,
				CookieSecure:  cfg.Auth.CookieSecure,
				SessionIdle:   cfg.Auth.SessionIdle,
				SessionMax:    cfg.Auth.SessionMax,
			},
			// A /models write drops the tenant's cached resolutions on
			// this replica at once; other replicas wait out the 30s TTL.
			ModelInvalidator: modelRegistry,

			// POST /api/v1/ingest: fed by a companion capture component,
			// not a normal gateway-proxied caller. IngestSink
			// is nil (route answers 503) unless the ClickHouse sink is
			// enabled; Capture/LLMCapture reuse the SAME body-capture
			// config the gateway's own proxy paths apply, so an ingested
			// row is bounded identically to a gateway-proxied one.
			IngestConfig:      cfg.Ingest,
			Capture:           cfg.Capture,
			LLMCapture:        cfg.LLMProxy.Capture,
			IngestSink:        sinks.IngestSink,
			IngestRateLimiter: ingestRateLimiter,
			DefaultTimeoutMS:  cfg.Connectors.DefaultTimeoutMS,
		}
		// apiKeyAuthenticator is nil when auth.api_keys.enabled is
		// false; api.Deps.KeyInvalidator's nil interface is a documented
		// no-op, so only assign it when there's a real Authenticator
		// instance whose cache actually needs evicting on revoke/rotate.
		if apiKeyAuthenticator != nil {
			apiDeps.KeyInvalidator = apiKeyAuthenticator
		}
		if cfg.LLMProxy.Enabled {
			apiDeps.LimitsStatus = func() map[string]any { return limiter.Status() }
		}
		if cfg.API.ServeUI {
			apiDeps.UIHandler = ui.Handler()
		}
		apiHandler := api.NewRouter(apiDeps)
		routines = append(routines, httpplane.New(
			"api", cfg.API.Address, apiHandler,
			defaultReadTimeout, defaultReadTimeout, defaultIdleTimeout, logger,
		))
		logger.Info("plane enabled", "plane", "api", "addr", cfg.API.Address)
	}

	if cfg.LLMProxy.Enabled {
		// Lossless capture: the plane writes each call synchronously to the sink
		// (Postgres), committed before the request completes, so nothing is lost
		// on a later crash. Same single-write pattern as Tuskira's runner. It also
		// tees a best-effort copy into the shared analytics sink (sinks.Sink →
		// ClickHouse/OTel), which feeds the API plane's LLM-logs/analytics views.
		batchSink, err := buildBatchSink(cfg, logger)
		if err != nil {
			return fmt.Errorf("build llm capture sink: %w", err)
		}
		defer batchSink.Close()

		recorder = capture.NewRecorder(batchSink, sinks.Sink, bodyStore, cfg.LLMProxy.Capture.BodyStore.InlineMaxBytes)

		// The auth middleware runs AHEAD of the plane (it puts the Principal on
		// the context that the plane's llm.access check, capture and the
		// per-tenant limiter read); /health stays unauthenticated, mirroring
		// httpplane.GatedHandler.
		// Per-key limits (api_keys.limits). USD budgets read spend back from
		// the Postgres capture table; with any other capture store a key
		// that HAS a USD budget is refused rather than served unchecked.
		spend, _ := batchSink.(sink.SpendReader)
		if spend == nil {
			logger.Warn("llm limits: capture store is not postgres; API keys with daily_usd/monthly_usd budgets will be refused (503)")
		}
		limiter, err = llmplane.NewLimiter(llmplane.LimiterConfig{Keys: st.APIKeys(), Spend: spend})
		if err != nil {
			return fmt.Errorf("build llm limiter: %w", err)
		}
		llmCfg := llmConfig(cfg.LLMProxy, pricingCard, authorizer)
		llmCfg.ClientIP = clientIPs.IP
		llmCfg.Limiter = limiter
		llmCfg.Registry = modelRegistry
		if d := cfg.LLMProxy.Detection; d.AgentURL != "" {
			tee := llmplane.NewDetectionTee(llmplane.DetectionTeeConfig{
				AgentURL: d.AgentURL, Timeout: d.Timeout, QueueSize: d.QueueSize,
				QueueBytes: d.QueueBytes, MaxInFlight: d.MaxInFlight,
			})
			// Runs after supervisor.Run has stopped the planes, so no handler
			// is still offering turns: drain for a bounded time, drop the rest.
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), detectionDrainTimeout)
				defer cancel()
				tee.Close(ctx)
			}()
			llmCfg.DetectionTee = tee
			logger.Info("llm detection tee enabled", "agent_url", d.AgentURL)
		}
		core, err := llmplane.Handler(llmCfg, recorder)
		if err != nil {
			return fmt.Errorf("build llm plane: %w", err)
		}
		llmMux := http.NewServeMux()
		llmMux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			health := map[string]any{
				"status": "ok", "plane": "llm", "version": cfg.Service.Version, "limits": limiter.Status(),
			}
			if llmCfg.DetectionTee != nil {
				health["detection"] = llmCfg.DetectionTee.Status()
			}
			_ = json.NewEncoder(w).Encode(health)
		})
		llmMux.Handle("/", authMiddleware(core))

		// Write timeout 0 (unlimited): LLM responses may stream for a long time.
		routines = append(routines, httpplane.New(
			"llm", cfg.LLMProxy.Address, gwMetrics.Instrument("llm", metrics.LLMRoute)(llmMux),
			defaultReadTimeout, 0, defaultIdleTimeout, logger,
		))
		logger.Info("plane enabled", "plane", "llm", "addr", cfg.LLMProxy.Address)
	}

	if len(routines) == nonPlaneRoutines {
		logger.Warn("no planes enabled; nothing to serve")
	}

	return supervisor.Run(context.Background(), logger, routines...)
}

// secretsDeps bundles the secret store's constructed pieces: the Service
// (encrypt/decrypt/rotate/cache over the credential store) and the header
// Registry (static/token_field/incoming_field/external resolvers, with the
// secret_store/env/file providers already registered). It exists so the
// API and dataplane planes have a single, already-built place to pull
// these from once they grow credential-backed routes.
type secretsDeps struct {
	Service  *secrets.Service
	Registry *dataplaneheaders.Registry
}

// buildSecretsDeps loads the master key ring, builds the secrets.Service
// over st's credential store, and builds a header Registry with the four
// built-in resolvers plus the secret_store/env/file external providers
// (env and file are always registered but gated internally by
// cfg.SecretStore.AllowEnvProvider/AllowFileProvider -- see
// internal/secrets.EnvProvider/FileProvider). It logs the active key id
// (never the key itself) and which providers are enabled.
func buildSecretsDeps(cfg *config.Config, st store.Store, logger *slog.Logger) (*secretsDeps, error) {
	ring, err := secrets.LoadKeyRing(cfg.SecretStore)
	if err != nil {
		return nil, fmt.Errorf("load master key ring: %w", err)
	}

	keyIDs := make([]string, 0, len(ring.Keys))
	for id := range ring.Keys {
		keyIDs = append(keyIDs, id)
	}
	sort.Strings(keyIDs)
	fingerprints := make([]string, len(keyIDs))
	for i, id := range keyIDs {
		fingerprints[i] = id + ":" + secrets.Fingerprint(ring.Keys[id])
	}
	logger.Info("secret store master key ring loaded",
		"active_key_id", ring.ActiveKeyID, "key_count", len(ring.Keys), "key_fingerprints", fingerprints)

	svc := secrets.NewService(st.Credentials(), ring)

	registry := dataplaneheaders.NewRegistry()
	registry.SetAllowBearerForwarding(cfg.Connectors.AllowBearerTokenForwarding)
	if err := registry.RegisterExternal(secrets.NewSecretStoreProvider(svc)); err != nil {
		return nil, fmt.Errorf("register secret_store provider: %w", err)
	}
	if err := registry.RegisterExternal(secrets.NewEnvProvider(cfg.SecretStore.AllowEnvProvider)); err != nil {
		return nil, fmt.Errorf("register env provider: %w", err)
	}
	if err := registry.RegisterExternal(secrets.NewFileProvider(cfg.SecretStore.AllowFileProvider, cfg.SecretStore.FileProviderRoot)); err != nil {
		return nil, fmt.Errorf("register file provider: %w", err)
	}

	logger.Info("header providers registered",
		"secret_store", true,
		"env_enabled", cfg.SecretStore.AllowEnvProvider,
		"file_enabled", cfg.SecretStore.AllowFileProvider,
	)

	return &secretsDeps{Service: svc, Registry: registry}, nil
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

// llmConfig maps the LLMProxy config section onto the llmplane package's own
// Config (which doesn't import config). card is the rate card built by
// buildPricingCard (embedded, or merged with llm_proxy.pricing_file).
func llmConfig(c config.LLMProxy, card *pricing.Card, z pkgauth.Authorizer) llmplane.Config {
	return llmplane.Config{
		UpstreamBaseURL:         c.UpstreamBaseURL,
		MaxRequestBytes:         c.Limits.MaxRequestBytes,
		MaxStreamDuration:       c.Limits.MaxStreamDuration,
		MaxConcurrentPerTenant:  c.Limits.MaxConcurrentPerTenant,
		BedrockEnabled:          c.Bedrock.Enabled,
		BedrockRegion:           c.Bedrock.Region,
		BedrockCredentialMode:   c.Bedrock.CredentialMode,
		BedrockRoleAccounts:     c.Bedrock.AllowedRoleAccounts,
		Authorizer:              z,
		OpenAIEnabled:           c.Providers.OpenAI.Enabled,
		OpenAIBaseURL:           c.Providers.OpenAI.BaseURL,
		OpenAIStreamUsage:       c.Providers.OpenAI.InjectStreamUsage,
		GeminiEnabled:           c.Providers.Gemini.Enabled,
		GeminiBaseURL:           c.Providers.Gemini.BaseURL,
		StoreBodies:             c.Capture.StoreBodies,
		MaxCaptureRequestBytes:  c.Capture.MaxRequestBytes,
		MaxCaptureResponseBytes: c.Capture.MaxResponseBytes,
		Pricing:                 card,
	}
}

// buildPricingCard builds the LLM cost rate card: pricing.Default (the
// embedded prices.json) when path is empty, or pricing.Default merged with
// path's models (file wins per model) when set. Returns the card, which
// analytics.PricingSource* value describes it, and a non-nil error only for
// a set-but-malformed path -- callers must treat that as fatal, never fall
// back to the embedded card silently.
func buildPricingCard(path string, logger *slog.Logger) (*pricing.Card, string, error) {
	if strings.TrimSpace(path) == "" {
		return pricing.Default, analytics.PricingSourceEmbedded, nil
	}
	merged, n, err := pricing.Default.MergeFile(path)
	if err != nil {
		return nil, "", err
	}
	logger.Info("llm pricing: loaded override file", "path", path, "models_loaded", n)
	return merged, analytics.PricingSourceFile, nil
}

// buildBatchSink is the LLM plane's durable, synchronous capture destination:
// Postgres is the lossless default (row committed before the request completes),
// stdout the best-effort dev fallback. ClickHouse capture is NOT built here --
// the LLM plane tees a best-effort copy of each call into the shared analytics
// sink (buildSinks) instead, so the binary has exactly one ClickHouse sink.
func buildBatchSink(cfg *config.Config, logger *slog.Logger) (sink.BatchSink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	if cfg.LLMProxy.Capture.Store == "postgres" {
		pgSink, err := pgsink.New(ctx, pgsink.Options{DSN: cfg.Database.DSN()})
		if err != nil {
			return nil, err
		}
		logger.Info("llm capture sink enabled", "store", "postgres", "table", "llm_calls")
		return pgSink, nil
	}

	logger.Warn("llm capture: no durable store configured; shipping to stdout (best-effort, not durable)")
	return stdoutsink.New(os.Stdout), nil
}

// llmCallReader gives the API plane's LLM Logs a Postgres source when no
// ClickHouse reader is configured: the capture table records every call
// whatever the analytics sinks are. Nil when ClickHouse serves them, or
// when the capture store is not Postgres.
func llmCallReader(cfg *config.Config, ch analytics.Reader, logger *slog.Logger) analytics.LLMCallReader {
	if ch != nil || cfg.LLMProxy.Capture.Store != "postgres" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()
	pg, err := pgsink.New(ctx, pgsink.Options{DSN: cfg.Database.DSN()})
	if err != nil {
		logger.Warn("llm logs: postgres reader unavailable", "error", err)
		return nil
	}
	logger.Info("llm logs served from postgres capture table (no clickhouse sink)")
	return pg
}

// buildBodyStore returns the sink.BodyStore selected by
// llm_proxy.capture.body_store.type, or nil for "none" (bodies stay inline in
// the capture row). The S3 secret access key is never logged.
func buildBodyStore(cfg *config.Config, logger *slog.Logger) (sink.BodyStore, error) {
	bs := cfg.LLMProxy.Capture.BodyStore
	if (bs.Type == "filesystem" || bs.Type == "s3") && !cfg.LLMProxy.Capture.StoreBodies {
		logger.Warn("llm_proxy.capture.body_store is set but store_bodies is false: no bodies will be offloaded (the store only resolves existing refs)")
	}
	switch bs.Type {
	case "filesystem":
		s, err := fsbodystore.New(bs.Filesystem.Root)
		if err != nil {
			return nil, err
		}
		logger.Info("body store enabled", "type", "filesystem", "root", bs.Filesystem.Root, "inline_max_bytes", bs.InlineMaxBytes)
		return s, nil
	case "s3":
		ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
		defer cancel()
		s, err := s3bodystore.New(ctx, s3bodystore.Config{
			Bucket:          bs.S3.Bucket,
			Prefix:          bs.S3.Prefix,
			Region:          bs.S3.Region,
			Endpoint:        bs.S3.Endpoint,
			ForcePathStyle:  bs.S3.ForcePathStyle,
			AccessKeyID:     bs.S3.AccessKeyID,
			SecretAccessKey: bs.S3.SecretAccessKey,
		})
		if err != nil {
			return nil, err
		}
		logger.Info("body store enabled", "type", "s3", "bucket", bs.S3.Bucket, "prefix", bs.S3.Prefix,
			"endpoint", bs.S3.Endpoint, "static_credentials", bs.S3.AccessKeyID != "", "inline_max_bytes", bs.InlineMaxBytes)
		return s, nil
	}
	return nil, nil
}

// sinksResult bundles what buildSinks produces: the combined sink.Multi
// every plane writes through, the analytics.Reader the API plane's
// /analytics/* routes read through (nil unless the ClickHouse sink is
// enabled), and a Status closure GET /api/v1/health calls on every
// request to report each enabled sink's live state.
type sinksResult struct {
	Sink      sink.LogSink
	Analytics analytics.Reader
	// IngestSink backs POST /api/v1/ingest's synchronous, duplicate-checked
	// write path (sink.IngestSink; see pkg/sink/clickhouse.Sink.
	// WriteIngestBatch). Nil when no ClickHouse sink is configured -- the
	// same nil-means-unconfigured contract Analytics carries.
	IngestSink sink.IngestSink
	Status     func() map[string]any
}

// buildSinks constructs every enabled sink.LogSink from cfg.Sinks,
// combines them into one sink.Multi, and recovers the ClickHouse sink's
// analytics.Reader and every sink's drop counter via type assertions on
// the concrete sink each constructor returns -- the same seam pattern
// pkg/ops uses (this package depends on the concrete sink packages so
// the seam types (sink.LogSink, analytics.Reader) don't have to know
// about Dropped() or connection details). gwMetrics may be nil or
// disabled; an enabled one adds its record-derived metrics sink.
func buildSinks(cfg *config.Config, gwMetrics *metrics.Metrics, logger *slog.Logger) (*sinksResult, error) {
	var sinks []sink.LogSink
	statusFns := map[string]func() map[string]any{}

	if cfg.Sinks.Stdout.Enabled {
		s := stdoutsink.New(os.Stdout, stdoutsink.WithBodies(cfg.Sinks.Stdout.IncludeBodies))
		if cfg.Sinks.Stdout.IncludeBodies && (cfg.LLMProxy.Capture.StoreBodies || cfg.Capture.StoreBodies) {
			logger.Warn("sinks.stdout.include_bodies and store_bodies are both on: request/response bodies (prompts, completions, tool arguments) are printed to stdout and will reach any log shipper; turn include_bodies off unless this is a throwaway dev run")
		}
		sinks = append(sinks, s)
		statusFns["stdout"] = func() map[string]any {
			return map[string]any{"enabled": true, "dropped": s.Dropped()}
		}
		logger.Info("sink enabled", "sink", "stdout")
	}

	if cfg.Sinks.Otel.Enabled {
		s, err := otelsink.New(otelsink.Config{
			Endpoint:    cfg.Sinks.Otel.Endpoint,
			Protocol:    cfg.Sinks.Otel.Protocol,
			Insecure:    cfg.Sinks.Otel.Insecure,
			Headers:     cfg.Sinks.Otel.Headers,
			ServiceName: cfg.Sinks.Otel.ServiceName,
		})
		if err != nil {
			return nil, fmt.Errorf("build otel sink: %w", err)
		}
		sinks = append(sinks, s)
		statusFns["otel"] = func() map[string]any {
			return map[string]any{"enabled": true, "endpoint": cfg.Sinks.Otel.Endpoint, "protocol": cfg.Sinks.Otel.Protocol}
		}
		logger.Info("sink enabled", "sink", "otel", "endpoint", cfg.Sinks.Otel.Endpoint, "protocol", cfg.Sinks.Otel.Protocol)
	}

	var analyticsReader analytics.Reader
	var ingestSink sink.IngestSink
	if cfg.Sinks.ClickHouse.Enabled {
		s, err := chsink.New(chsink.Config{
			Host:          cfg.Sinks.ClickHouse.Host,
			Port:          cfg.Sinks.ClickHouse.Port,
			Database:      cfg.Sinks.ClickHouse.Database,
			Username:      cfg.Sinks.ClickHouse.Username,
			Password:      cfg.Sinks.ClickHouse.Password,
			Secure:        cfg.Sinks.ClickHouse.Secure,
			BatchSize:     cfg.Sinks.ClickHouse.BatchSize,
			FlushInterval: cfg.Sinks.ClickHouse.FlushInterval,
			BufferSize:    cfg.Sinks.ClickHouse.BufferSize,
		}, logger)
		if err != nil {
			return nil, fmt.Errorf("build clickhouse sink: %w", err)
		}
		sinks = append(sinks, s)

		if reader, ok := s.(analytics.Reader); ok {
			analyticsReader = reader
		}
		if is, ok := s.(sink.IngestSink); ok {
			ingestSink = is
		}
		if dc, ok := s.(interface{ Dropped() uint64 }); ok {
			statusFns["clickhouse"] = func() map[string]any {
				return map[string]any{"enabled": true, "dropped": dc.Dropped()}
			}
		}
		logger.Info("sink enabled", "sink", "clickhouse", "host", cfg.Sinks.ClickHouse.Host, "port", cfg.Sinks.ClickHouse.Port,
			"database", cfg.Sinks.ClickHouse.Database, "analytics_api_enabled", analyticsReader != nil)
	}

	if len(sinks) == 0 {
		logger.Warn("no log sinks enabled: access logs and LLM calls will not be recorded anywhere")
	}

	// The metrics sink derives counters from the same records. It is added
	// after the check above on purpose: it stores nothing, so it must not
	// hide the warning that no record is being kept. The LLM plane reaches
	// it too: capture.Recorder tees every call into this Multi.
	if gwMetrics.Enabled() {
		sinks = append(sinks, gwMetrics.Sink())
		logger.Info("sink enabled", "sink", "metrics")
	}

	status := func() map[string]any {
		out := make(map[string]any, len(statusFns))
		for name, fn := range statusFns {
			out[name] = fn()
		}
		return out
	}

	return &sinksResult{Sink: sink.Multi(sinks...), Analytics: analyticsReader, IngestSink: ingestSink, Status: status}, nil
}

// resolveDevTenant resolves auth.dev_mode.tenant (a slug, or a tenant UUID)
// to the tenant's id, stores it back in cfg for buildAuthenticator, and logs
// the one loud startup warning dev mode deserves. A missing tenant is a
// startup error: dev mode must not run against a tenant that does not exist.
func resolveDevTenant(cfg *config.Config, st store.Store, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	defer cancel()

	want := strings.TrimSpace(cfg.Auth.DevMode.Tenant)
	var id string
	if t, err := st.Tenants().GetBySlug(ctx, want); err == nil {
		id = t.ID
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("auth.dev_mode: look up tenant %q: %w", want, err)
	} else {
		tenants, lerr := st.Tenants().List(ctx)
		if lerr != nil {
			return fmt.Errorf("auth.dev_mode: list tenants: %w", lerr)
		}
		for _, t := range tenants {
			if t.ID == want {
				id = t.ID
			}
		}
	}
	if id == "" {
		return fmt.Errorf("auth.dev_mode: tenant %q does not exist (no tenant has that slug or id); create it first, e.g. `gateway bootstrap-key --tenant %s`", want, want)
	}
	cfg.Auth.DevMode.Tenant = id

	roles := "admin"
	if cfg.Auth.DevMode.Platform {
		roles = "admin + platform-admin"
	}
	logger.Warn("DEV MODE IS ON: every request that carries no credential is served as an unauthenticated "+roles+" for the tenant below. "+
		"Never enable auth.dev_mode outside local development.",
		"tenant", want, "tenant_id", id,
		"allow_remote", cfg.Auth.DevMode.AllowRemote, "platform", cfg.Auth.DevMode.Platform)
	return nil
}

// buildAuthenticator assembles the gateway's auth chains from cfg.
//
// The data-plane chain (first return) is the api-key Authenticator when
// enabled, then the dev-mode gap-filler when enabled. The API-plane chain
// (second return) is the same with the console session Authenticator
// between them: API keys first (a request with an Authorization header is
// never treated as a browser session), then the session cookie, then
// dev-mode, which only fires when nothing ahead of it recognized a
// credential at all and so must run last.
//
// It also returns the concrete *apikey.Authenticator instance (nil when
// auth.api_keys.enabled is false), separately from the opaque
// pkgauth.Authenticator chains, so the caller can wire its InvalidateKey
// method into api.Deps.KeyInvalidator -- the chain type itself exposes no
// such method.
func buildAuthenticator(cfg *config.Config, st store.Store, logger *slog.Logger) (plane, apiPlane pkgauth.Authenticator, apiKeyAuth *apikey.Authenticator) {
	var keyAuth, devAuth []pkgauth.Authenticator

	if cfg.Auth.APIKeys.Enabled {
		apiKeyAuth = apikey.New(st.APIKeys(), apikey.Options{
			CacheTTL: cfg.Auth.APIKeys.CacheTTL,
		})
		keyAuth = append(keyAuth, apiKeyAuth)
	}
	if cfg.Auth.DevMode.Enabled {
		devAuth = append(devAuth, devmode.New(cfg.Auth.DevMode.Tenant, cfg.Auth.DevMode.Platform, logger))
	}

	sessionAuth := session.New(st.Users(), st.UserSessions(), session.Options{
		Idle:   cfg.Auth.SessionIdle,
		Logger: logger,
	})

	plane = internalauth.Chain(append(append([]pkgauth.Authenticator{}, keyAuth...), devAuth...)...)
	apiChain := append(append(append([]pkgauth.Authenticator{}, keyAuth...), sessionAuth), devAuth...)
	return plane, internalauth.Chain(apiChain...), apiKeyAuth
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

// revocationRoutine runs the API-key revocation listener
// (apikey.Authenticator.ListenRevocations) under the supervisor.
type revocationRoutine struct {
	auth   *apikey.Authenticator
	n      store.RevocationNotifier
	logger *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func newRevocationRoutine(a *apikey.Authenticator, n store.RevocationNotifier, logger *slog.Logger) *revocationRoutine {
	ctx, cancel := context.WithCancel(context.Background())
	return &revocationRoutine{auth: a, n: n, logger: logger, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (r *revocationRoutine) Name() string { return "key-revocation-listener" }

func (r *revocationRoutine) Init(context.Context) error { return nil }

func (r *revocationRoutine) Run(context.Context) error {
	defer close(r.done)
	r.auth.ListenRevocations(r.ctx, r.n, r.logger)
	return nil
}

func (r *revocationRoutine) Stop(ctx context.Context) error {
	r.cancel()
	select {
	case <-r.done:
	case <-ctx.Done():
	}
	return nil
}
