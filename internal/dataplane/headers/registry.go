// Package headers is the resolver framework implementing pkg/headers'
// contract: a Registry that dispatches a connector's per-header config
// ({"type": ..., ...}) to the right HeaderResolver, four built-in
// resolvers (static, token_field, incoming_field, external), and the
// context plumbing incoming_field needs to see the inbound request.
//
// ExternalProviders (secret_store, env, file, ...) are NOT built here --
// they live in internal/secrets and elsewhere, and are wired into a
// Registry via RegisterExternal at startup (see cmd/gateway/main.go).
package headers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"

	"golang.org/x/net/http/httpguts"
)

// Registry resolves connector header configs into concrete outbound header
// values at request time. Construct one with NewRegistry, which preloads
// the four built-in resolvers; add ExternalProviders with RegisterExternal
// as the deployment enables them.
type Registry struct {
	mu        sync.RWMutex
	resolvers map[string]pkgheaders.HeaderResolver
	providers map[string]pkgheaders.ExternalProvider

	// allowBearer permits token_field/bearer_token (connectors
	// .allow_bearer_token_forwarding). Off by default.
	allowBearer atomic.Bool
}

var crlfEscaper = strings.NewReplacer("\r\n", `\n`, "\r", `\n`, "\n", `\n`)

// ErrBearerForwardingDisabled is returned for a token_field header whose
// field is bearer_token while connectors.allow_bearer_token_forwarding is
// off.
var ErrBearerForwardingDisabled = errors.New(`token_field "bearer_token" forwards the caller's own credential and is disabled (set connectors.allow_bearer_token_forwarding: true to allow it)`)

// SetAllowBearerForwarding enables or disables bearer_token forwarding.
func (reg *Registry) SetAllowBearerForwarding(v bool) { reg.allowBearer.Store(v) }

// CheckConnectorHeaders validates a connector's metadata.headers map when
// it is saved: every header needs a registered type and a config its
// resolver accepts (so a denylisted incoming_field header or a disabled
// provider is refused up front instead of being dropped on every call),
// and bearer_token forwarding needs connectors.allow_bearer_token_forwarding.
// A nil Registry forwards nothing, so it is never an error.
func (reg *Registry) CheckConnectorHeaders(meta map[string]any) error {
	if reg == nil {
		return nil
	}
	raw, present := meta["headers"]
	hdrs, isMap := raw.(map[string]any)
	if present && raw != nil && !isMap {
		return errors.New("metadata.headers must be an object")
	}
	names := make([]string, 0, len(hdrs))
	for name := range hdrs {
		names = append(names, name)
	}
	sort.Strings(names) // the same bad header is reported every time
	for _, name := range names {
		v := hdrs[name]
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("header %q: not a valid header name", name)
		}
		cfg, _ := v.(map[string]any)
		if prefix, _ := cfg["prefix"].(string); strings.ContainsAny(prefix, "\r\n") {
			return fmt.Errorf("header %q: prefix must not contain a line break", name)
		}
		if isBearerForwarding(cfg) && !reg.allowBearer.Load() {
			return fmt.Errorf("header %q: %w", name, ErrBearerForwardingDisabled)
		}
		typ, _ := cfg["type"].(string)
		resolver, ok := reg.resolver(typ)
		if !ok {
			return fmt.Errorf("header %q: unknown type %q", name, typ)
		}
		if err := resolver.Validate(cfg); err != nil {
			return fmt.Errorf("header %q: %w", name, err)
		}
	}
	return nil
}

func isBearerForwarding(cfg map[string]any) bool {
	typ, _ := cfg["type"].(string)
	field, _ := cfg["field"].(string)
	return typ == "token_field" && field == tokenFieldBearerToken
}

// NewRegistry returns a Registry preloaded with the four built-in
// resolvers: static, token_field, incoming_field, and external (which
// dispatches to whatever ExternalProviders are later registered via
// RegisterExternal). No ExternalProvider is registered yet.
func NewRegistry() *Registry {
	reg := &Registry{
		resolvers: make(map[string]pkgheaders.HeaderResolver),
		providers: make(map[string]pkgheaders.ExternalProvider),
	}

	// These four Register calls can never fail: their Type()s are
	// distinct literals defined in this package, registered exactly
	// once, here. A failure would be a programmer error caught
	// instantly by any test that constructs a Registry.
	mustRegister(reg, staticResolver{})
	mustRegister(reg, tokenFieldResolver{})
	mustRegister(reg, incomingFieldResolver{})
	mustRegister(reg, &externalResolver{reg: reg})

	return reg
}

func mustRegister(reg *Registry, r pkgheaders.HeaderResolver) {
	if err := reg.Register(r); err != nil {
		panic("headers: " + err.Error())
	}
}

// Register adds r to the set of resolvers dispatched by a header config's
// "type" field. It returns an error if a resolver is already registered
// for r.Type().
func (reg *Registry) Register(r pkgheaders.HeaderResolver) error {
	if r == nil {
		return fmt.Errorf("headers: Register called with a nil resolver")
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, exists := reg.resolvers[r.Type()]; exists {
		return fmt.Errorf("headers: resolver already registered for type %q", r.Type())
	}
	reg.resolvers[r.Type()] = r
	return nil
}

// RegisterExternal adds p to the set of providers reachable via header
// config {"type": "external", "provider": p.ProviderID(), "config": {...}}.
// It returns an error if a provider is already registered under
// p.ProviderID().
func (reg *Registry) RegisterExternal(p pkgheaders.ExternalProvider) error {
	if p == nil {
		return fmt.Errorf("headers: RegisterExternal called with a nil provider")
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, exists := reg.providers[p.ProviderID()]; exists {
		return fmt.Errorf("headers: provider already registered for id %q", p.ProviderID())
	}
	reg.providers[p.ProviderID()] = p
	return nil
}

// Providers returns every registered ExternalProvider, sorted by
// ProviderID for a stable GET /headers/providers response.
func (reg *Registry) Providers() []pkgheaders.ExternalProvider {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	out := make([]pkgheaders.ExternalProvider, 0, len(reg.providers))
	for _, p := range reg.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID() < out[j].ProviderID() })
	return out
}

// Provider returns the ExternalProvider registered under id, if any.
//
// The MCP plane's 401-recovery step needs it: to know whether a
// connector's credentials are worth evicting and replaying, it has to
// look each of that connector's configured providers up by the id in the
// header config and ask whether it implements pkgheaders.Invalidator.
func (reg *Registry) Provider(id string) (pkgheaders.ExternalProvider, bool) {
	return reg.provider(id)
}

func (reg *Registry) provider(id string) (pkgheaders.ExternalProvider, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	p, ok := reg.providers[id]
	return p, ok
}

func (reg *Registry) resolver(typ string) (pkgheaders.HeaderResolver, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	r, ok := reg.resolvers[typ]
	return r, ok
}

// Resolve computes the value of one outbound header named headerName from
// cfg -- the value of one entry in a connector's metadata.headers map --
// by dispatching on cfg["type"]. r is the inbound request currently being
// proxied; it is consulted only by the incoming_field resolver and may be
// nil for any other type.
//
// cfg["prefix"], if present, is prepended to the resolved value (e.g.
// "Bearer "). The final value is then rejected if it contains a CR or LF
// byte -- otherwise a resolved value (an attacker-controlled incoming
// header, or a credential someone pasted with a trailing newline) could
// inject an extra outbound header or split the request.
func (reg *Registry) Resolve(ctx context.Context, headerName string, cfg map[string]any, r *http.Request) (string, error) {
	typ, _ := cfg["type"].(string)
	if typ == "" {
		return "", fmt.Errorf("headers: header %q: config is missing \"type\"", headerName)
	}

	resolver, ok := reg.resolver(typ)
	if !ok {
		return "", fmt.Errorf("headers: header %q: no resolver registered for type %q", headerName, typ)
	}

	if isBearerForwarding(cfg) && !reg.allowBearer.Load() {
		return "", fmt.Errorf("headers: header %q: %w", headerName, ErrBearerForwardingDisabled)
	}

	if err := resolver.Validate(cfg); err != nil {
		return "", fmt.Errorf("headers: header %q: invalid config: %w", headerName, err)
	}

	if r != nil {
		ctx = withIncomingRequest(ctx, r)
	}

	value, err := resolver.Resolve(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("headers: header %q: %w", headerName, err)
	}

	// A stored credential can legitimately be multi-line (a PEM key). Send
	// it as the two-character `\n` sequence the backends that accept such
	// values expect; every other type still fails the CR/LF check below.
	if typ == "external" {
		value = crlfEscaper.Replace(strings.TrimRight(value, "\r\n"))
	}

	if prefix, _ := cfg["prefix"].(string); prefix != "" {
		value = prefix + value
	}

	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("headers: header %q: resolved value contains a CR or LF byte", headerName)
	}

	return value, nil
}
