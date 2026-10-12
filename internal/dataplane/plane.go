// Package dataplane assembles the MCP plane from its parts: the backend
// client, the router, the session manager, the profile enforcer, the
// tool cache, the orchestrator and the HTTP transport.
//
// It exists so cmd/gateway asks for an MCP plane rather than knowing how
// one is built, and so the wiring is exercised by a test rather than
// only by the binary.
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/cache"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/orchestrator"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/skillsext"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/transport"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	pkgsession "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// sessionStoreOpenTimeout bounds opening the session store (for the
// Redis driver, the dial and ping). A wrong address should fail the boot
// in seconds.
const sessionStoreOpenTimeout = 10 * time.Second

// Deps are everything the MCP plane needs from the process around it.
type Deps struct {
	Config *config.Config
	// Store supplies the connector, profile, tool-cache and tenant
	// repositories.
	Store store.Store
	// Headers resolves connector metadata.headers entries.
	Headers *dpheaders.Registry
	// Authenticator resolves an inbound credential to a Principal.
	Authenticator pkgauth.Authenticator
	// Authorizer gates the plane on mcp.access.
	Authorizer pkgauth.Authorizer
	// Sink receives one AccessLog per request.
	Sink sink.LogSink
	// Logger is the plane's logger.
	Logger *slog.Logger
	// ClientIP derives the caller's address; see transport.Deps.ClientIP.
	ClientIP func(*http.Request) string
	// OnToolCacheLookup, when set, is told the outcome of every tools/list
	// read of the tool cache ("hit", "stale" or "miss"; see
	// orchestrator.Deps.OnToolCacheLookup). Nil observes nothing.
	OnToolCacheLookup func(result string)
}

// Plane is a built MCP plane.
type Plane struct {
	// Handler serves POST /mcp, GET /mcp/stream and GET /health.
	Handler http.Handler
	// Routines are the plane's background workers: session cleanup
	// (when the session store needs sweeping), the sweep that closes the
	// connector streams of sessions that are gone, the cross-replica
	// notification subscriber (when the session driver supplies a
	// Notifier), and, when the tool cache is enabled, its refresh and
	// cleanup loops. They are supervisor.Routines, so the caller owns
	// their lifecycle alongside the listener's.
	Routines []supervisor.Routine

	// ConnectorOps, CacheOps and ProfileOps back the control-plane API's
	// connector health/discover, tool-cache management, and agent-profile
	// cache invalidation routes (pkg/ops, adapted in ops.go). They are
	// always populated when a Plane is built -- independent of
	// cfg.ToolCache.Enabled, which only gates whether the live tools/list
	// path serves from the cache.
	ConnectorOps ops.ConnectorOps
	CacheOps     ops.CacheOps
	ProfileOps   ops.ProfileOps

	client   *client.Client
	sessions *session.Manager
	orch     *orchestrator.Orchestrator
	notifier orchestrator.Notifier
	// sessionStore and sessionNotifier are the pkg/session driver's
	// halves, closed with the plane. sessionNotifier is nil for a
	// single-replica driver.
	sessionStore    pkgsession.Store
	sessionNotifier pkgsession.Notifier
}

// NotifyToolsListChanged pushes notifications/tools/list_changed to every
// SSE stream open for the tenant -- on this replica, and, when the
// session driver supplies a Notifier (redis), on every other mcp replica
// too.
//
// The background refresher already calls this when it rewrites a
// tenant's cache; it is exported so the control plane can call it too,
// when an operator adds or edits a connector and the agents connected
// right now should not have to wait out a refresh interval to notice.
func (p *Plane) NotifyToolsListChanged(tenantID string) { p.notifier.ToolsListChanged(tenantID) }

// SessionCount is how many MCP sessions the plane's session store holds
// right now, or -1 when the driver cannot say cheaply (redis). The memory
// driver counts expired sessions until its sweep reclaims them.
func (p *Plane) SessionCount() int {
	if c, ok := p.sessionStore.(interface{ Len() int }); ok {
		return c.Len()
	}
	return -1
}

// SessionsCreated is how many sessions this process has created since
// start, whatever the session driver.
func (p *Plane) SessionsCreated() uint64 { return p.sessions.Created() }

// UpstreamStreams reports how many connector streams the plane holds
// open for agent sessions, and how many goroutines serve them.
func (p *Plane) UpstreamStreams() (streams, goroutines int) { return p.orch.UpstreamStreams() }

// New builds the MCP plane.
//
// The session store is opened through pkg/session's registry under
// cfg.Sessions.Store (the binary must have blank-imported the driver, as
// cmd/gateway does for memory and redis). With a driver that supplies a
// Notifier, tools/list_changed is relayed between replicas through it.
// Router health and the agent-profile cache stay per process either way:
// a connector one replica marks unhealthy is probed independently by
// another, and a profile edit reaches each replica within the profile
// cache's own short TTL.
func New(deps Deps) (*Plane, error) {
	switch {
	case deps.Config == nil:
		return nil, fmt.Errorf("dataplane: Config is required")
	case deps.Store == nil:
		return nil, fmt.Errorf("dataplane: Store is required")
	case deps.Authenticator == nil:
		return nil, fmt.Errorf("dataplane: Authenticator is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	cfg := deps.Config

	backend := client.New(client.Options{
		Headers:        deps.Headers,
		DefaultTimeout: cfg.Connectors.DefaultTimeout(),
		TLS:            cfg.Connectors.TLS,
		Logger:         deps.Logger,
	})

	sessionStore, sessionNotifier, err := openSessionStore(cfg)
	if err != nil {
		_ = backend.Close()
		return nil, err
	}
	closeSessions := func() {
		_ = sessionStore.Close()
		if sessionNotifier != nil {
			_ = sessionNotifier.Close()
		}
	}
	deps.Logger.Info("sessions", "store", cfg.Sessions.StoreName(),
		"ttl", cfg.Sessions.TTL, "cross_replica_notify", sessionNotifier != nil)

	sessions := session.NewManager(sessionStore, session.Options{
		TTL:             cfg.Sessions.TTL,
		CleanupInterval: cfg.Sessions.CleanupInterval,
		Logger:          deps.Logger,
	})

	toolRouter := router.New(deps.Store.Connectors(), router.Options{})

	profiles := profile.New(deps.Store.AgentProfiles(), deps.Store.Connectors(), deps.Store.Skills(), profile.Options{})
	skills := skillsext.New(deps.Store.AgentProfiles(), deps.Store.Skills(), skillsext.Options{})

	var toolCache cache.ToolCache
	if cfg.ToolCache.Enabled {
		toolCache = cache.NewL2(deps.Store.ToolCache(), cache.Options{
			TTL:        cfg.ToolCache.L2TTL,
			ServeStale: cfg.ToolCache.ServeStale,
		})
	}

	hub := transport.NewHub()
	var (
		notifier orchestrator.Notifier = hub
		bridge   *transport.Bridge
	)
	if sessionNotifier != nil {
		bridge = transport.NewBridge(hub, sessionNotifier, deps.Logger)
		notifier = bridge
	}

	orch, err := orchestrator.New(orchestrator.Deps{
		Sessions:       sessions,
		Profiles:       profiles,
		Router:         toolRouter,
		Client:         backend,
		Cache:          toolCache,
		Connectors:     deps.Store.Connectors(),
		Logger:         deps.Logger,
		Notifier:       notifier,
		Skills:         skills,
		RequireProfile: cfg.MCP.RequireProfile,

		MaxUpstreamStreamsPerSession:       cfg.MCP.MaxUpstreamStreamsPerSession,
		MaxSubscriptionsPerSession:         cfg.MCP.MaxSubscriptionsPerSession,
		MaxPendingServerRequestsPerSession: cfg.MCP.MaxPendingServerRequestsPerSession,
		ServerRequestTimeout:               cfg.MCP.ServerRequestTimeout,
		Sink:                               deps.Sink,
		OnToolCacheLookup:                  deps.OnToolCacheLookup,
	})
	if err != nil {
		closeSessions()
		_ = backend.Close()
		return nil, err
	}
	if bridge != nil {
		// An agent's answer to a relayed request that lands on another
		// replica comes back here, to the replica waiting for it.
		bridge.OnResponse(orch.DeliverRemoteResponse)
	}

	handler := transport.NewHandler(transport.Deps{
		Orchestrator:  orch,
		Sessions:      sessions,
		Authenticator: deps.Authenticator,
		Authorizer:    deps.Authorizer,
		Sink:          deps.Sink,
		Hub:           hub,
		Capture:       cfg.Capture,
		Logger:        deps.Logger,
		ClientIP:      deps.ClientIP,
		ServiceName:   cfg.Service.Name,
		Version:       cfg.Service.Version,
	})

	var routines []supervisor.Routine
	if sweep := sessions.CleanupRoutine(); sweep != nil {
		routines = append(routines, sweep)
	}
	routines = append(routines, orch.UpstreamRoutine())
	if bridge != nil {
		routines = append(routines, bridge.Routine())
	}
	if cfg.ToolCache.Enabled {
		routines = append(routines,
			cache.RefreshRoutine(deps.Store.Tenants(), orch.RefreshTenant, cfg.ToolCache.RefreshInterval, deps.Logger),
			cache.CleanupRoutine(deps.Store.ToolCache(), cfg.ToolCache.CleanupInterval, deps.Logger),
		)
	}

	opsAdapter := newOpsAdapter(deps, toolRouter, backend, profiles, hub)

	return &Plane{
		Handler:         handler,
		Routines:        routines,
		ConnectorOps:    opsAdapter,
		CacheOps:        opsAdapter,
		ProfileOps:      opsAdapter,
		client:          backend,
		sessions:        sessions,
		orch:            orch,
		notifier:        notifier,
		sessionStore:    sessionStore,
		sessionNotifier: sessionNotifier,
	}, nil
}

// openSessionStore resolves cfg.Sessions.Store through pkg/session's
// registry, handing the driver the redis section's connection fields and
// sessions.options.
func openSessionStore(cfg *config.Config) (pkgsession.Store, pkgsession.Notifier, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionStoreOpenTimeout)
	defer cancel()

	st, n, err := pkgsession.Open(ctx, cfg.Sessions.StoreName(), pkgsession.Config{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		TLS:      cfg.Redis.TLS,
		Options:  cfg.Sessions.Options,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dataplane: session store: %w", err)
	}
	return st, n, nil
}

// Close closes the plane's connector streams and releases its pooled
// backend connections and its session store (and notifier, when the
// driver has one).
func (p *Plane) Close() error {
	p.orch.Close()
	err := p.client.Close()
	if p.sessionNotifier != nil {
		err = errors.Join(err, p.sessionNotifier.Close())
	}
	if p.sessionStore != nil {
		err = errors.Join(err, p.sessionStore.Close())
	}
	return err
}
