// Package oidc implements single sign-on for console users: the gateway
// as a generic OpenID Connect relying party (authorization code flow with
// PKCE, state and nonce), for any issuer that publishes a discovery
// document (Cognito, Keycloak, Entra ID, Okta, ...).
//
// This package owns the protocol half: the per-login state (sealed into a
// short-lived cookie with a key derived from the gateway master key, so
// any replica can finish a login another one started), the authorize
// redirect, the code exchange, the ID-token checks and the email /
// upstream-IdP rules. Mapping the verified identity to a console user and
// starting the session is the API handler's job (internal/api/handlers),
// which reuses the password login's session, cookie, CSRF and audit
// machinery unchanged.
package oidc

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
)

// Failure codes. A failed login is redirected to the console login page
// with ?sso_error=<code> -- one of these fixed values, never free text --
// and audited with the same code.
const (
	CodeState      = "state"      // missing, expired, tampered or mismatched login state
	CodeToken      = "token"      // code exchange failed, or the ID token failed a check
	CodeDomain     = "domain"     // no email, a malformed one, or a domain not allowed
	CodeUnverified = "unverified" // email_verified is not true (and is required)
	CodeIdP        = "idp"        // the token does not show the required upstream IdP
	CodeNoUser     = "no_user"    // no matching console user (provision: existing)
	CodeDisabled   = "disabled"   // the user is disabled
	CodeLinked     = "linked"     // the user is already linked to another identity
	// CodeLinkRequired: require_verified_email is off and the matching user
	// has a usable password, so the unverified email may not link to it.
	CodeLinkRequired = "link_required"
	CodeRole         = "role"     // a platform-admin, and the provider does not allow it
	CodeProvider     = "provider" // unknown provider, IdP unreachable or returned an error, misconfiguration
)

// Error is a failed login: Code is what the user and the audit trail see,
// Err (never shown to the user) is for the server log.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return "oidc: " + e.Code
	}
	return "oidc: " + e.Code + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func fail(code string, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

const (
	// StateLifetime bounds how long a started login may take.
	StateLifetime = 10 * time.Minute
	// httpTimeout bounds each call to the issuer (discovery, JWKS, token).
	httpTimeout = 10 * time.Second
	// discoveryRetry is the least time between two failed discovery
	// attempts, so a down IdP is not hammered by every click.
	discoveryRetry = 5 * time.Second
	// discoveryTTL is how long a fetched discovery document is used before
	// it is fetched again (endpoints or key URLs the issuer changed are
	// picked up). A failed refresh keeps the previous one in use.
	discoveryTTL = time.Hour
)

// signingAlgs are the ID-token algorithms accepted: asymmetric only, so
// "none" and every HS* (whose key would be the client secret) are refused
// whatever the discovery document advertises.
var signingAlgs = []string{
	gooidc.RS256, gooidc.RS384, gooidc.RS512,
	gooidc.PS256, gooidc.PS384, gooidc.PS512,
	gooidc.ES256, gooidc.ES384, gooidc.ES512,
	gooidc.EdDSA,
}

// Provider is one configured issuer.
type Provider struct {
	Name        string
	DisplayName string
	// Issuer is the configured issuer URL (the ID token's iss must equal it).
	Issuer string
	// TenantRef is the configured tenant (slug or id).
	TenantRef string

	cfg          config.OIDCProvider
	clientSecret string
	scopes       []string
	domains      map[string]bool
	redirects    []*url.URL
	client       *http.Client

	// mu guards the discovery cache only; it is never held across a
	// network call. sf collapses concurrent fetches into one.
	mu         sync.Mutex
	discovered *gooidc.Provider
	verifier   *gooidc.IDTokenVerifier
	fetchedAt  time.Time
	lastFail   time.Time
	sf         singleflight.Group
}

// Config returns the provider's configuration (no secret: the client
// secret is never part of it).
func (p *Provider) Config() config.OIDCProvider { return p.cfg }

// SessionMax is the provider's session lifetime cap (0 = the global one).
func (p *Provider) SessionMax() time.Duration { return p.cfg.SessionMax }

// Service holds every configured provider and the login-state sealing key.
type Service struct {
	providers map[string]*Provider
	order     []*Provider
	sealKey   []byte
	now       func() time.Time
}

// Options configures New. Zero values take the defaults.
type Options struct {
	// HTTPClient is used for every call to an issuer. Default: a client
	// dialing through the egress guard (internal/netguard), with a 10s
	// timeout and capped redirects.
	HTTPClient *http.Client
	// Now is overridable for tests.
	Now func() time.Time
	// Getenv and ReadFile resolve client_secret_env / client_secret_file;
	// overridable for tests.
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

// sealInfo is the HKDF info string: it separates the login-state key from
// every other use of the master key.
const sealInfo = "gateway oidc login state v1"

// New builds the Service from validated config (config.Config.Validate has
// run). masterKey is the active secret-store master key; the login-state
// key is derived from it with HKDF-SHA256, never used directly. Each
// provider's client secret is read now, so a missing one fails startup.
// No issuer is contacted here: discovery happens on the first login and
// is retried later if the issuer is unreachable.
func New(providers []config.OIDCProvider, masterKey []byte, opts Options) (*Service, error) {
	if len(masterKey) < 32 {
		return nil, errors.New("oidc: master key must be at least 32 bytes")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, sealInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("oidc: derive login-state key: %w", err)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.ReadFile == nil {
		opts.ReadFile = os.ReadFile
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{
			Transport:     netguard.Transport(),
			Timeout:       httpTimeout,
			CheckRedirect: netguard.LimitRedirects,
		}
	}

	s := &Service{providers: map[string]*Provider{}, sealKey: key, now: opts.Now}
	for _, c := range providers {
		p, err := newProvider(c, opts)
		if err != nil {
			return nil, err
		}
		s.providers[p.Name] = p
		s.order = append(s.order, p)
	}
	return s, nil
}

func newProvider(c config.OIDCProvider, opts Options) (*Provider, error) {
	var secret string
	switch {
	case c.ClientSecretFile != "":
		b, err := opts.ReadFile(c.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("oidc provider %s: read client_secret_file: %w", c.Name, err)
		}
		secret = strings.TrimSpace(string(b))
	case c.ClientSecretEnv != "":
		secret = strings.TrimSpace(opts.Getenv(c.ClientSecretEnv))
	}
	if secret == "" {
		return nil, fmt.Errorf("oidc provider %s: the client secret is empty", c.Name)
	}

	p := &Provider{
		Name: c.Name, DisplayName: c.DisplayName, Issuer: c.Issuer, TenantRef: strings.TrimSpace(c.Tenant),
		cfg: c, clientSecret: secret, domains: map[string]bool{}, client: opts.HTTPClient,
		scopes: c.Scopes,
	}
	if p.DisplayName == "" {
		p.DisplayName = c.Name
	}
	if len(p.scopes) == 0 {
		p.scopes = []string{gooidc.ScopeOpenID, "email", "profile"}
	}
	for _, d := range c.AllowedEmailDomains {
		p.domains[strings.ToLower(d)] = true
	}
	for _, raw := range c.AllRedirectURLs() {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("oidc provider %s: redirect_url: %w", c.Name, err)
		}
		p.redirects = append(p.redirects, u)
	}
	return p, nil
}

// Providers returns the providers in configuration order.
func (s *Service) Providers() []*Provider {
	if s == nil {
		return nil
	}
	return s.order
}

// Provider returns the named provider.
func (s *Service) Provider(name string) (*Provider, bool) {
	if s == nil {
		return nil, false
	}
	p, ok := s.providers[name]
	return p, ok
}

// StartPath is the gateway path that starts a login with provider name.
func StartPath(name string) string { return "/api/v1/auth/oidc/" + name + "/start" }

// discover returns the provider's discovery result and ID-token verifier.
// The document is fetched on first use and again once it is older than
// discoveryTTL; concurrent callers share one fetch (singleflight) and the
// cache lock is never held across it. A failed fetch is retried no sooner
// than discoveryRetry later; meanwhile a previously fetched document, if
// any, stays in use.
func (p *Provider) discover(now time.Time) (*gooidc.Provider, *gooidc.IDTokenVerifier, error) {
	p.mu.Lock()
	d, v, fetched, failed := p.discovered, p.verifier, p.fetchedAt, p.lastFail
	p.mu.Unlock()
	if d != nil && now.Sub(fetched) < discoveryTTL {
		return d, v, nil
	}
	if !failed.IsZero() && now.Sub(failed) < discoveryRetry {
		if d != nil {
			return d, v, nil
		}
		return nil, nil, errors.New("issuer discovery failed recently; retrying shortly")
	}

	type result struct {
		d *gooidc.Provider
		v *gooidc.IDTokenVerifier
	}
	got, err, _ := p.sf.Do("discover", func() (any, error) {
		// The context outlives this request on purpose: go-oidc keeps it
		// for the JWKS fetches it makes later (key rotation, unknown kid).
		ctx := gooidc.ClientContext(context.Background(), p.client)
		nd, err := gooidc.NewProvider(ctx, p.Issuer)
		if err != nil {
			return nil, err
		}
		nv := nd.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID, SupportedSigningAlgs: signingAlgs})
		return result{nd, nv}, nil
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.lastFail = now
		if p.discovered != nil {
			return p.discovered, p.verifier, nil
		}
		return nil, nil, err
	}
	r := got.(result)
	p.discovered, p.verifier, p.fetchedAt, p.lastFail = r.d, r.v, now, time.Time{}
	return r.d, r.v, nil
}

func (p *Provider) oauth2Config(d *gooidc.Provider, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.clientSecret,
		Endpoint:     d.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       p.scopes,
	}
}

// redirectFor picks the registered callback for a login started by r: the
// one whose host is the host r was addressed to (X-Forwarded-Host first,
// for a console behind a proxy), else the first. Only configured URLs are
// ever chosen, so a forged header can at worst pick another registered
// callback.
func (p *Provider) redirectFor(r *http.Request) *url.URL {
	var hosts []string
	if fh := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); fh != "" {
		hosts = append(hosts, fh)
	}
	hosts = append(hosts, r.Host)
	for _, h := range hosts {
		for _, u := range p.redirects {
			if strings.EqualFold(u.Host, h) {
				return u
			}
		}
	}
	return p.redirects[0]
}
