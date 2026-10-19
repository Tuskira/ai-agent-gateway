package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/devmode"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/metrics"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	fsbodystore "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/bodystore/fs"
	s3bodystore "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/bodystore/s3"
	chsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/clickhouse"
	otelsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/otel"
	pgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/postgres"
	stdoutsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/stdout"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

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

// llmConfig maps the LLMProxy config section onto the llmplane package's own
// Config (which doesn't import config). card is the rate card built by
// buildPricingCard (embedded, or merged with llm_proxy.pricing_file).
func llmConfig(c config.LLMProxy, card pricing.Pricer, z pkgauth.Authorizer) llmplane.Config {
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

// llmCalls picks the API plane's LLM Logs source: the program's reader
// (Options.LLMCalls) when set, else llmCallReader's choice.
func llmCalls(supplied analytics.LLMCallReader, cfg *config.Config, ch analytics.Reader, logger *slog.Logger) analytics.LLMCallReader {
	if supplied != nil {
		return supplied
	}
	return llmCallReader(cfg, ch, logger)
}

// callerOwnedSink is an Options.LogSinks entry inside the sink.Multi: it
// forwards every write, but Close is a no-op, because the program that
// passed the sink closes it after Run returns.
type callerOwnedSink struct{ sink.LogSink }

func (callerOwnedSink) Close() error { return nil }

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
	// Dropped reads each enabled sink's drop counter ("stdout",
	// "clickhouse"; the otel sink has none), for the metrics collector.
	Dropped map[string]func() uint64
}

// buildSinks constructs every enabled sink.LogSink from cfg.Sinks,
// combines them into one sink.Multi, and recovers the ClickHouse sink's
// analytics.Reader and every sink's drop counter via type assertions on
// the concrete sink each constructor returns -- the same seam pattern
// pkg/ops uses (this package depends on the concrete sink packages so
// the seam types (sink.LogSink, analytics.Reader) don't have to know
// about Dropped() or connection details). extra (Options.LogSinks) is
// added after the built-in sinks; the caller keeps ownership of those, so
// closing the Multi does not close them. gwMetrics may be nil or disabled;
// an enabled one adds its record-derived metrics sink.
func buildSinks(cfg *config.Config, gwMetrics *metrics.Metrics, extra []sink.LogSink, logger *slog.Logger) (*sinksResult, error) {
	var sinks []sink.LogSink
	statusFns := map[string]func() map[string]any{}
	dropped := map[string]func() uint64{}

	if cfg.Sinks.Stdout.Enabled {
		s := stdoutsink.New(os.Stdout, stdoutsink.WithBodies(cfg.Sinks.Stdout.IncludeBodies))
		if cfg.Sinks.Stdout.IncludeBodies && (cfg.LLMProxy.Capture.StoreBodies || cfg.Capture.StoreBodies) {
			logger.Warn("sinks.stdout.include_bodies and store_bodies are both on: request/response bodies (prompts, completions, tool arguments) are printed to stdout and will reach any log shipper; turn include_bodies off unless this is a throwaway dev run")
		}
		sinks = append(sinks, s)
		dropped["stdout"] = s.Dropped
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
			dropped["clickhouse"] = dc.Dropped
			statusFns["clickhouse"] = func() map[string]any {
				return map[string]any{"enabled": true, "dropped": dc.Dropped()}
			}
		}
		logger.Info("sink enabled", "sink", "clickhouse", "host", cfg.Sinks.ClickHouse.Host, "port", cfg.Sinks.ClickHouse.Port,
			"database", cfg.Sinks.ClickHouse.Database, "analytics_api_enabled", analyticsReader != nil)
	}

	for _, s := range extra {
		sinks = append(sinks, callerOwnedSink{s})
	}
	if len(extra) > 0 {
		logger.Info("sink enabled", "sink", "supplied by the program", "count", len(extra))
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

	return &sinksResult{Sink: sink.Multi(sinks...), Analytics: analyticsReader, IngestSink: ingestSink, Status: status, Dropped: dropped}, nil
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
// enabled, then extra (Options.Authenticators, in order), then the
// dev-mode gap-filler when enabled. The API-plane chain (second return) is
// the same with the console session Authenticator after extra: API keys
// first (a request with an Authorization header is never treated as a
// browser session; a bearer value without the gk_ prefix falls through to
// extra), then extra, then the session cookie, then dev-mode, which only
// fires when nothing ahead of it recognized a credential at all and so
// must run last.
//
// It also returns the concrete *apikey.Authenticator instance (nil when
// auth.api_keys.enabled is false), separately from the opaque
// pkgauth.Authenticator chains, so the caller can wire its InvalidateKey
// method into api.Deps.KeyInvalidator -- the chain type itself exposes no
// such method.
func buildAuthenticator(cfg *config.Config, st store.Store, extra []pkgauth.Authenticator, logger *slog.Logger) (plane, apiPlane pkgauth.Authenticator, apiKeyAuth *apikey.Authenticator) {
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

	keyAuth = append(keyAuth, extra...)
	plane = internalauth.Chain(append(append([]pkgauth.Authenticator{}, keyAuth...), devAuth...)...)
	apiChain := append(append(append([]pkgauth.Authenticator{}, keyAuth...), sessionAuth), devAuth...)
	return plane, internalauth.Chain(apiChain...), apiKeyAuth
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
