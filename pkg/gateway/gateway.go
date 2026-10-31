// Package gateway starts the whole gateway -- the MCP, API and LLM planes
// and the metrics listener, whichever the config enables -- in the calling
// process. It is what cmd/gateway runs, exposed so that a separate Go module
// (an outside module cannot import this module's internal/ packages) can
// start the same gateway with its own pieces plugged in through Options.
//
// Every Options field is optional: the zero Options is exactly the built-in
// gateway the `gateway` binary starts. Run blocks until ctx is canceled, the
// process gets SIGINT/SIGTERM, or a plane fails, then shuts every plane down.
//
// Run reads the same config as the binary (Options.ConfigPath, then the
// GATEWAY_* environment variables), needs the same database and master key,
// and, like the binary, sets the process-wide slog default logger from the
// config's logging section.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/detectionevents"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/inflight"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/handlers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/ui"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/capture"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/clientip"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/httpplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/mcpcatalog"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/metrics"
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
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// DefaultVersion is the service version Run reports (health endpoints,
// metrics build info) when Options.Version is empty.
const DefaultVersion = "dev"

const (
	dbConnectTimeout   = 10 * time.Second
	defaultReadTimeout = 30 * time.Second
	defaultIdleTimeout = 120 * time.Second
	// detectionDrainTimeout bounds posting the detection tee's queued turns
	// at shutdown; what is left after it is dropped.
	detectionDrainTimeout = 5 * time.Second
)

// Options plugs a program's own pieces into Run. Every field is optional;
// a nil or empty field keeps the built-in behaviour, so Options{} starts
// exactly what the `gateway` binary starts.
//
// Run does not close anything passed in Options: the caller owns those
// values and closes them after Run returns (by then no plane writes to
// them any more).
type Options struct {
	// ConfigPath is the YAML config file, as the binary's --config flag
	// (or CONFIG_PATH). Empty loads the built-in defaults; GATEWAY_*
	// environment variables apply on top either way.
	ConfigPath string

	// Version is the service version reported on the health endpoints and
	// in metrics. Empty means DefaultVersion.
	Version string

	// Authenticators are added to every plane's authentication chain right
	// AFTER the built-in API-key authenticator and BEFORE the console
	// session cookie (API plane only) and dev mode. A request carrying a
	// gateway API key (gk_...) is therefore still decided by the API-key
	// authenticator; any other credential reaches these, in order. An
	// Authenticator returns pkg/auth.ErrNoCredential to pass the request on.
	Authenticators []pkgauth.Authenticator

	// LogSinks receive every access log and LLM call in addition to the
	// sinks the config enables (stdout, otel, clickhouse).
	LogSinks []sink.LogSink

	// BatchSink replaces the LLM plane's durable capture store
	// (llm_proxy.capture.store: Postgres or stdout). If it also implements
	// sink.SpendReader, per-key USD budgets read spend from it; otherwise
	// keys with a USD budget are refused, as with the stdout store.
	BatchSink sink.BatchSink

	// LLMCalls replaces the Postgres capture-table reader behind the
	// console's LLM Logs. The API serves LLM Logs from the analytics reader
	// whenever there is one (Analytics, or the ClickHouse sink's), so
	// LLMCalls is used only when there is none.
	LLMCalls analytics.LLMCallReader

	// Analytics replaces the reader behind /api/v1/analytics/*, LLM Logs
	// included (by default the ClickHouse sink's, or none).
	Analytics analytics.Reader

	// Pricer replaces the LLM cost rate card (the embedded prices.json,
	// merged with llm_proxy.pricing_file). When set, pricing_file is not
	// read (a warning is logged if it is configured) and the Overview's
	// pricingSource is "custom".
	Pricer pricing.Pricer

	// KeyLookup replaces where the LLM plane's per-key limiter reads each
	// API key's limits (by default the store's API keys).
	KeyLookup store.APIKeyLookup
}

// validate rejects nil entries in the Options slices: a nil Authenticator
// or LogSink would panic on the first request instead of failing startup.
func (o Options) validate() error {
	for i, a := range o.Authenticators {
		if a == nil {
			return fmt.Errorf("gateway: Options.Authenticators[%d] is nil", i)
		}
	}
	for i, s := range o.LogSinks {
		if s == nil {
			return fmt.Errorf("gateway: Options.LogSinks[%d] is nil", i)
		}
	}
	return nil
}

// Run starts the gateway described by the config and opts and blocks until
// it stops: when ctx is canceled, on SIGINT/SIGTERM, or when a plane fails.
// It returns nil after a clean shutdown, or the error that stopped startup
// or a plane. See Options for what each field replaces or adds.
func Run(ctx context.Context, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	version := opts.Version
	if version == "" {
		version = DefaultVersion
	}

	cfg, err := config.Load(opts.ConfigPath)
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
	// Pool gauges, for a store that can report them (postgres).
	if ds, ok := st.(metrics.DBStatser); ok {
		if err := metrics.RegisterDBStats(gwMetrics, ds); err != nil {
			return err
		}
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
	if err := metrics.RegisterAuthLimiters(gwMetrics, map[string]metrics.AuthLimiter{
		"api": rateLimiter, "mcp": mcpRateLimiter, "llm": llmRateLimiter,
	}); err != nil {
		return err
	}

	// Detection events (llm_proxy.detection.agent_url): tool-call outcomes
	// and refused keys go to the detection agent, off the request path.
	var events *detectionevents.Emitter
	if d := cfg.LLMProxy.Detection; d.AgentURL != "" {
		events = detectionevents.New(detectionevents.Config{AgentURL: d.AgentURL, Timeout: d.Timeout})
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), detectionDrainTimeout)
			defer cancel()
			events.Close(ctx)
		}()
	}
	// openCalls holds every data-plane request per API key, so key
	// containment (POST /api-keys/{id}/containment) cuts an agent's open
	// calls on this process at once.
	openCalls := inflight.New()

	authenticator, apiAuthenticator, apiKeyAuthenticator := buildAuthenticator(cfg, st, opts.Authenticators, refusedEvents(events), logger)
	// The MCP and LLM planes refuse an IP locked out for failed auth with a
	// 429 in their own error format, before any credential lookup.
	mcpAuthenticator := internalauth.PlaneRateLimitedAuthenticator(openCalls.Authenticator(authenticator), mcpRateLimiter)
	llmAuthenticator := internalauth.PlaneRateLimitedAuthenticator(openCalls.Authenticator(authenticator), llmRateLimiter)
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

	sinks, err := buildSinks(cfg, gwMetrics, opts.LogSinks, logger)
	if err != nil {
		return fmt.Errorf("build sinks: %w", err)
	}
	defer sinks.Sink.Close()
	if err := metrics.RegisterSinkDrops(gwMetrics, sinks.Dropped); err != nil {
		return err
	}

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
	// Options.Pricer replaces the card (and the file) outright.
	var pricer pricing.Pricer
	var pricingSource string
	if opts.Pricer != nil {
		pricer, pricingSource = opts.Pricer, analytics.PricingSourceCustom
		if strings.TrimSpace(cfg.LLMProxy.PricingFile) != "" {
			logger.Warn("llm pricing: llm_proxy.pricing_file is ignored because the program supplied its own pricer", "path", cfg.LLMProxy.PricingFile)
		}
	} else {
		card, source, err := buildPricingCard(cfg.LLMProxy.PricingFile, logger)
		if err != nil {
			return fmt.Errorf("load llm pricing file: %w", err)
		}
		pricer, pricingSource = card, source
	}

	// Options.Analytics replaces the ClickHouse sink's reader for the
	// /analytics/* routes, and Options.LLMCalls the LLM Logs source.
	analyticsReader := sinks.Analytics
	if opts.Analytics != nil {
		analyticsReader = opts.Analytics
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
		toolCacheLookup, err := metrics.ToolCacheLookupHook(gwMetrics)
		if err != nil {
			return err
		}
		mcpPlane, err := dataplane.New(dataplane.Deps{
			Config:            cfg,
			Store:             st,
			Headers:           secDeps.Registry,
			Authenticator:     mcpAuthenticator,
			Authorizer:        authorizer,
			Sink:              sinks.Sink,
			Logger:            logger,
			ClientIP:          clientIPs.IP,
			OnToolCacheLookup: toolCacheLookup,
			OnToolCall:        toolCallEvents(events),
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
			if err := metrics.RegisterSessions(gwMetrics, mcpPlane.SessionCount, mcpPlane.SessionsCreated); err != nil {
				return err
			}
			routines = append(routines, httpplane.New(
				"mcp", cfg.MCP.Address, gwMetrics.Instrument("mcp", metrics.MCPRoute)(openCalls.Middleware("mcp")(mcpPlane.Handler)),
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
			Analytics:      analyticsReader,
			AnalyticsMeta:  analytics.Meta{PricingSource: pricingSource},
			BodyStore:      bodyStore,
			LLMCalls:       llmCalls(opts.LLMCalls, cfg, analyticsReader, logger),
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
			Inflight:          openCalls,
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
		// A supplied BatchSink belongs to the caller, who closes it after
		// Run returns; the built-in one is closed here.
		batchSink := opts.BatchSink
		if batchSink == nil {
			batchSink, err = buildBatchSink(cfg, logger)
			if err != nil {
				return fmt.Errorf("build llm capture sink: %w", err)
			}
			defer batchSink.Close()
		} else {
			logger.Info("llm capture sink enabled", "store", "supplied by the program")
		}

		recorder = capture.NewRecorder(batchSink, sinks.Sink, bodyStore, cfg.LLMProxy.Capture.BodyStore.InlineMaxBytes)
		if err := metrics.RegisterBodyStore(gwMetrics, recorder); err != nil {
			return err
		}

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
		var keys store.APIKeyLookup = st.APIKeys()
		if opts.KeyLookup != nil {
			keys = opts.KeyLookup
		}
		limiter, err = llmplane.NewLimiter(llmplane.LimiterConfig{Keys: keys, Spend: spend})
		if err != nil {
			return fmt.Errorf("build llm limiter: %w", err)
		}
		if err := metrics.RegisterLLMLimits(gwMetrics, limiter); err != nil {
			return err
		}
		llmCfg := llmConfig(cfg.LLMProxy, pricer, authorizer)
		llmCfg.ClientIP = clientIPs.IP
		llmCfg.Limiter = limiter
		llmCfg.Registry = modelRegistry
		if d := cfg.LLMProxy.Detection; d.AgentURL != "" {
			tee := llmplane.NewDetectionTee(llmplane.DetectionTeeConfig{
				AgentURL: d.AgentURL, Timeout: d.Timeout, QueueSize: d.QueueSize,
				QueueBytes: d.QueueBytes, MaxInFlight: d.MaxInFlight,
				Inline: d.Inline, InlineTimeout: d.InlineTimeout,
			})
			// Runs after supervisor.Run has stopped the planes, so no handler
			// is still offering turns: drain for a bounded time, drop the rest.
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), detectionDrainTimeout)
				defer cancel()
				tee.Close(ctx)
			}()
			llmCfg.DetectionTee = tee
			if err := metrics.RegisterDetectionTee(gwMetrics, tee); err != nil {
				return err
			}
			logger.Info("llm detection tee enabled", "agent_url", d.AgentURL, "inline", d.Inline)
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
				health["detection_events"] = events.Status()
			}
			_ = json.NewEncoder(w).Encode(health)
		})
		llmMux.Handle("/", openCalls.Middleware("llm")(authMiddleware(core)))

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

	return supervisor.Run(ctx, logger, routines...)
}
