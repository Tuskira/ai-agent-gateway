// Package router turns a gateway-visible tool name into the backend
// connector that serves it.
//
// The gateway presents one flat tool namespace over many backends, so
// every advertised tool is named "<connector>__<tool>". Routing is the
// inverse of that rename, plus the two things that have to happen before
// a call leaves: the connector must be usable, and the gateway's own
// argument overrides must be stamped onto the call.
package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Connector status values, as written to connectors.status.
const (
	StatusHealthy   = "healthy"
	StatusUnhealthy = "unhealthy"
	StatusUnknown   = "unknown"
)

// defaultCoolDown is how long an unhealthy connector is left alone
// before the router lets one call through to probe it.
const defaultCoolDown = 30 * time.Second

// Routing errors. Each maps to a distinct JSON-RPC code at the transport
// so a client can tell "you spelled the tool wrong" apart from "the
// backend is down".
var (
	// ErrBadToolName means the name is not "<connector>__<tool>".
	ErrBadToolName = errors.New("router: tool name must be \"<connector>__<tool>\"")
	// ErrBadPromptName means the name is not "<connector>__<prompt>".
	ErrBadPromptName = errors.New("router: prompt name must be \"<connector>__<prompt>\"")
	// ErrBadResourceURI means the URI is not "gw://<connector>/<uri>".
	ErrBadResourceURI = errors.New("router: resource uri must be \"" + client.ResourceURIScheme + "<connector>/<uri>\"")
	// ErrConnectorNotFound means no connector in the tenant matches the
	// prefix.
	ErrConnectorNotFound = errors.New("router: connector not found")
	// ErrConnectorUnhealthy means the connector is marked unhealthy and
	// is still inside its recovery cool-down.
	ErrConnectorUnhealthy = errors.New("router: connector is unhealthy")
)

// Route is a resolved tool call: which connector, and what the backend
// calls the tool.
type Route struct {
	Connector *store.Connector
	// ToolName is the backend's own, unprefixed name.
	ToolName string
	// QualifiedName is the name the caller used.
	QualifiedName string
	// Probe reports that the connector was unhealthy and this call is
	// the half-open probe that may bring it back.
	Probe bool
}

// Options configures a Router.
type Options struct {
	// CoolDown is how long an unhealthy connector is skipped before a
	// probe is allowed through. Zero uses 30s.
	CoolDown time.Duration
	// Now returns the current time; tests override it.
	Now func() time.Time
}

// Router resolves tool names to connectors within a tenant.
type Router struct {
	connectors store.ConnectorStore
	coolDown   time.Duration
	now        func() time.Time

	mu          sync.Mutex
	lastProbeAt map[string]time.Time
}

// New returns a Router over a connector store.
func New(connectors store.ConnectorStore, opts Options) *Router {
	if opts.CoolDown <= 0 {
		opts.CoolDown = defaultCoolDown
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Router{
		connectors:  connectors,
		coolDown:    opts.CoolDown,
		now:         opts.Now,
		lastProbeAt: make(map[string]time.Time),
	}
}

// ParseToolName splits "<connector>__<tool>" into its two halves.
//
// The split is on the FIRST "__", because a backend may legitimately
// have a tool called "get__all__groups" while a connector name may not
// contain "__" (the admin API slugifies it).
func ParseToolName(qualified string) (connector, tool string, err error) {
	connector, tool, found := strings.Cut(qualified, "__")
	if !found || connector == "" || tool == "" {
		return "", "", fmt.Errorf("%w (got %q)", ErrBadToolName, qualified)
	}
	return connector, tool, nil
}

// Resolve maps a qualified tool name onto a connector in tenantID.
//
// A connector marked unhealthy is not permanently dead: after the
// cool-down it is let through once as a half-open probe, and the caller
// flips it back to healthy with MarkHealthy if the call succeeds. This
// is the one behavioural addition to the pipeline the gateway's
// predecessor had, where an unhealthy connector stayed unreachable until
// something else re-ran initialize -- in practice, until a restart.
func (r *Router) Resolve(ctx context.Context, tenantID, qualified string) (*Route, error) {
	name, tool, err := ParseToolName(qualified)
	if err != nil {
		return nil, err
	}

	conn, probe, err := r.ResolveConnector(ctx, tenantID, name)
	if err != nil {
		return nil, err
	}
	return &Route{Connector: conn, ToolName: tool, QualifiedName: qualified, Probe: probe}, nil
}

// ParsePromptName splits "<connector>__<prompt>" into its two halves,
// on the first "__", exactly as ParseToolName does for tools.
func ParsePromptName(qualified string) (connector, prompt string, err error) {
	connector, prompt, found := strings.Cut(qualified, "__")
	if !found || connector == "" || prompt == "" {
		return "", "", fmt.Errorf("%w (got %q)", ErrBadPromptName, qualified)
	}
	return connector, prompt, nil
}

// ParseResourceURI splits "gw://<connector>/<uri>" into the connector
// and the backend's own URI, which is returned byte for byte as the
// backend advertised it. The split is on the first "/" after the scheme:
// a connector qualifier never contains one, while the backend URI
// usually does.
func ParseResourceURI(qualified string) (connector, uri string, err error) {
	rest, ok := strings.CutPrefix(qualified, client.ResourceURIScheme)
	if !ok {
		return "", "", fmt.Errorf("%w (got %q)", ErrBadResourceURI, qualified)
	}
	connector, uri, found := strings.Cut(rest, "/")
	if !found || connector == "" || uri == "" {
		return "", "", fmt.Errorf("%w (got %q)", ErrBadResourceURI, qualified)
	}
	return connector, uri, nil
}

// ResolveConnector finds the connector a qualifier names within tenantID,
// applying the same health gate as Resolve: an unhealthy connector is
// refused inside its cool-down and let through once (probe true) after
// it. It is what prompts/get and resources/read route through.
func (r *Router) ResolveConnector(ctx context.Context, tenantID, name string) (conn *store.Connector, probe bool, err error) {
	conn, err = r.findConnector(ctx, tenantID, name)
	if err != nil {
		return nil, false, err
	}

	if conn.Status == StatusUnhealthy {
		if !r.allowProbe(conn.ID) {
			return nil, false, fmt.Errorf("%w: %s (retrying in up to %s)", ErrConnectorUnhealthy, conn.Name, r.coolDown)
		}
		probe = true
	}

	return conn, probe, nil
}

// findConnector looks a connector up by slug first (the indexed unique
// key) and falls back to a name match within the tenant, because the
// tool prefix is the connector's NAME and a deployment may have named
// and slugged a connector differently.
func (r *Router) findConnector(ctx context.Context, tenantID, name string) (*store.Connector, error) {
	conn, err := r.connectors.GetBySlug(ctx, tenantID, name)
	switch {
	case err == nil:
		if !conn.Enabled() {
			return nil, fmt.Errorf("%w: %q is disabled", ErrConnectorNotFound, name)
		}
		return conn, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("router: look up connector %q: %w", name, err)
	}

	all, err := r.connectors.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("router: list connectors: %w", err)
	}
	for _, c := range all {
		if c.Name == name {
			if !c.Enabled() {
				return nil, fmt.Errorf("%w: %q is disabled", ErrConnectorNotFound, name)
			}
			return c, nil
		}
	}

	return nil, fmt.Errorf("%w: %q", ErrConnectorNotFound, name)
}

// allowProbe reports whether an unhealthy connector may be probed now,
// and records the attempt if so. The window is per process: with several
// replicas each gets its own probe, which costs one extra failed call per
// replica per cool-down and needs no shared state to get right.
func (r *Router) allowProbe(connectorID string) bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	if last, ok := r.lastProbeAt[connectorID]; ok && now.Sub(last) < r.coolDown {
		return false
	}
	r.lastProbeAt[connectorID] = now
	return true
}

// MarkHealthy flips a connector to healthy and clears its probe window.
// It is a no-op when the connector is already healthy, so the common
// path does not write to Postgres on every tool call.
func (r *Router) MarkHealthy(ctx context.Context, conn *store.Connector) error {
	r.mu.Lock()
	delete(r.lastProbeAt, conn.ID)
	r.mu.Unlock()

	if conn.Status == StatusHealthy {
		return nil
	}
	conn.Status = StatusHealthy
	if err := r.connectors.Update(ctx, conn); err != nil {
		return fmt.Errorf("router: mark connector %s healthy: %w", conn.ID, err)
	}
	return nil
}

// MarkUnhealthy flips a connector to unhealthy and starts its cool-down.
func (r *Router) MarkUnhealthy(ctx context.Context, conn *store.Connector) error {
	r.mu.Lock()
	r.lastProbeAt[conn.ID] = r.now()
	r.mu.Unlock()

	if conn.Status == StatusUnhealthy {
		return nil
	}
	conn.Status = StatusUnhealthy
	if err := r.connectors.Update(ctx, conn); err != nil {
		return fmt.Errorf("router: mark connector %s unhealthy: %w", conn.ID, err)
	}
	return nil
}

// Callable returns the tenant's connectors a tools/list should fan out
// to: everything except a disabled connector (metadata.enabled=false) and
// one that is unhealthy and still inside its cool-down. An unknown-status connector is included -- it has never
// been initialized, which is not the same as having failed.
func (r *Router) Callable(ctx context.Context, tenantID string) ([]*store.Connector, error) {
	all, err := r.connectors.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("router: list connectors: %w", err)
	}

	out := make([]*store.Connector, 0, len(all))
	for _, c := range all {
		if !c.Enabled() {
			continue
		}
		if c.Status == StatusUnhealthy && !r.allowProbe(c.ID) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// StampOverrides merges the connector's configured argument overrides
// into a tool call's arguments, overwriting anything the caller supplied
// for those names.
//
// Overwriting is the point. These arguments carry tenant constants; if a
// caller's value won, a model that guessed one could read across a tenant
// boundary. The matching scrub happens in the client's tools/list, which
// removes these names from the advertised schema so nothing is invited to
// guess in the first place.
func StampOverrides(conn *store.Connector, toolName string, args map[string]any) map[string]any {
	overrides := client.ToolArgOverrides(conn, toolName)
	if len(overrides) == 0 {
		return args
	}

	out := make(map[string]any, len(args)+len(overrides))
	for k, v := range args {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
