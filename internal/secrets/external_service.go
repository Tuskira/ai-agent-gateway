package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
)

// ExternalServiceProviderID is the provider id of ExternalServiceProvider,
// used in header configs and credential sources.
const ExternalServiceProviderID = "external_service"

// externalServiceTokenField is the field of a source's auth_credential
// whose value is sent to the credential service as a bearer token.
const externalServiceTokenField = "token"

// maxExternalServiceResponse caps how much of a credential service's
// response body is read.
const maxExternalServiceResponse = 1 << 20

// ExternalServiceProvider is the "external_service" headers.ExternalProvider:
// it resolves a header value by asking a tenant's outside credential
// service over a fixed HTTP contract. The gateway never learns the
// service's own vendor API; the service adapts whatever it fronts.
//
// Each header names a credential source (headers.Source) holding the
// service's base URL and the NAME of a gateway credential whose "token"
// field authenticates the gateway to it. Per header the config is
// {"credential_id": "...", "field": "...", "prefix": "Bearer "}, prefix
// optional. Resolve calls
//
//	POST {url}/v1/credentials/resolve
//	{"tenant_id", "connector_slug", "header_name", "credential_id", "field"}
//
// and expects {"value": "...", "ttl_seconds": N}. Values are cached per
// tenant for min(ttl_seconds, cache_ttl); Invalidate drops the tenant's
// entries (the MCP plane calls it when a backend answers 401).
//
// The tenant always comes from the authenticated principal in ctx, never
// from config, and the source is loaded for that same tenant by the
// header registry, so one tenant's header cannot reach another tenant's
// source or credentials.
type ExternalServiceProvider struct {
	fetch    headers.CredentialFetcher
	client   *http.Client
	timeout  time.Duration
	cacheTTL time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]map[string]externalServiceEntry // tenant -> key -> entry
}

type externalServiceEntry struct {
	value   string
	expires time.Time
}

var (
	_ headers.ExternalProvider     = (*ExternalServiceProvider)(nil)
	_ headers.Invalidator          = (*ExternalServiceProvider)(nil)
	_ headers.SourceSchemaProvider = (*ExternalServiceProvider)(nil)
	_ headers.SourceTester         = (*ExternalServiceProvider)(nil)
)

// NewExternalServiceProvider builds the provider. fetch reads the token
// of a source's auth_credential from this gateway's secret store; timeout
// bounds one call to a service; cacheTTL caps how long a value is cached.
// Every call dials through internal/netguard, so the egress policy that
// guards connector endpoints also guards credential services.
func NewExternalServiceProvider(fetch headers.CredentialFetcher, timeout, cacheTTL time.Duration) *ExternalServiceProvider {
	return &ExternalServiceProvider{
		fetch: fetch,
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: netguard.LimitRedirects,
			Transport:     netguard.Transport(),
		},
		timeout:  timeout,
		cacheTTL: cacheTTL,
		now:      time.Now,
		cache:    make(map[string]map[string]externalServiceEntry),
	}
}

func (p *ExternalServiceProvider) ProviderID() string   { return ExternalServiceProviderID }
func (p *ExternalServiceProvider) ProviderName() string { return "External credential service" }
func (p *ExternalServiceProvider) Type() string         { return "external" }

func (p *ExternalServiceProvider) ConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"credential_id": map[string]any{
				"type":        "string",
				"description": "Id of the credential in the outside credential service.",
			},
			"field": map[string]any{
				"type":        "string",
				"description": "Field of that credential to return.",
			},
			"prefix": map[string]any{
				"type":        "string",
				"description": "Optional text prepended to the value, e.g. \"Bearer \".",
			},
		},
		"required": []string{"credential_id", "field"},
	}
}

// SourceSchema describes a credential source's config for this provider.
func (p *ExternalServiceProvider) SourceSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Base URL of the credential service. https is required; http only for a loopback or private address the gateway's egress policy allows.",
			},
			"auth_credential": map[string]any{
				"type":        "string",
				"description": "Name of a credential in this gateway's secret store; its \"token\" field is sent as Authorization: Bearer.",
			},
		},
		"required": []string{"url", "auth_credential"},
	}
}

// ValidateSource checks a credential source's config.
func (p *ExternalServiceProvider) ValidateSource(cfg map[string]any) error {
	_, _, err := externalServiceSourceArgs(cfg)
	return err
}

func (p *ExternalServiceProvider) Validate(cfg map[string]any) error {
	_, _, _, err := externalServiceArgs(cfg)
	return err
}

func (p *ExternalServiceProvider) Resolve(ctx context.Context, cfg map[string]any) (string, error) {
	credentialID, field, prefix, err := externalServiceArgs(cfg)
	if err != nil {
		return "", err
	}
	principal, ok := auth.PrincipalFrom(ctx)
	if !ok || principal == nil || principal.TenantID == "" {
		return "", errors.New("external_service: no authenticated principal in context")
	}
	info, _ := headers.ResolveInfoFrom(ctx)
	src := info.Source
	if src == nil {
		return "", errors.New("external_service: header must name a credential source (\"source\")")
	}
	if src.TenantID != "" && src.TenantID != principal.TenantID {
		return "", fmt.Errorf("external_service: source %q belongs to another tenant", src.Name)
	}
	base, authCredential, err := externalServiceSourceArgs(src.Config)
	if err != nil {
		return "", fmt.Errorf("external_service: source %q: %w", src.Name, err)
	}

	tenant := principal.TenantID
	sourceKey := src.ID
	if sourceKey == "" {
		sourceKey = src.Name
	}
	key := sourceKey + "|" + credentialID + "|" + field
	if v, ok := p.cached(tenant, key); ok {
		return prefix + v, nil
	}

	token, err := p.token(ctx, tenant, authCredential)
	if err != nil {
		return "", fmt.Errorf("external_service: source %q: %w", src.Name, err)
	}

	body, err := json.Marshal(map[string]string{
		"tenant_id":      tenant,
		"connector_slug": info.ConnectorSlug,
		"header_name":    info.HeaderName,
		"credential_id":  credentialID,
		"field":          field,
	})
	if err != nil {
		return "", fmt.Errorf("external_service: encode request: %w", err)
	}

	resp, err := p.call(ctx, http.MethodPost, base.JoinPath("v1", "credentials", "resolve").String(), token, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("external_service: source %q: %w", src.Name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxExternalServiceResponse))
	if err != nil {
		return "", fmt.Errorf("external_service: source %q: read response: %w", src.Name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("external_service: source %q: resolve returned HTTP %d", src.Name, resp.StatusCode)
	}

	var out struct {
		Value      string `json:"value"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("external_service: source %q: response is not the expected JSON", src.Name)
	}
	if out.Value == "" {
		return "", fmt.Errorf("external_service: source %q: response has an empty value", src.Name)
	}

	ttl := p.cacheTTL
	if out.TTLSeconds > 0 {
		if d := time.Duration(out.TTLSeconds) * time.Second; d < ttl {
			ttl = d
		}
	}
	p.store(tenant, key, out.Value, ttl)
	return prefix + out.Value, nil
}

// TestSource checks that src's service is reachable and accepts the
// gateway's token, by listing the credential ids it offers this tenant:
// GET {url}/v1/credentials/options?tenant_id=...&field=credential_id. No
// secret value is requested or returned.
func (p *ExternalServiceProvider) TestSource(ctx context.Context, src headers.Source) error {
	base, authCredential, err := externalServiceSourceArgs(src.Config)
	if err != nil {
		return err
	}
	if src.TenantID == "" {
		return errors.New("source has no tenant")
	}
	token, err := p.token(ctx, src.TenantID, authCredential)
	if err != nil {
		return err
	}
	u := base.JoinPath("v1", "credentials", "options")
	u.RawQuery = url.Values{"tenant_id": {src.TenantID}, "field": {"credential_id"}}.Encode()

	resp, err := p.call(ctx, http.MethodGet, u.String(), token, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxExternalServiceResponse))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("credential service returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Invalidate drops every cached value for the calling principal's tenant.
// Like secret_store's, it is tenant-wide: the Invalidator contract does
// not say which source or credential backs connectorSlug.
func (p *ExternalServiceProvider) Invalidate(ctx context.Context, _, _ string) {
	principal, ok := auth.PrincipalFrom(ctx)
	if !ok || principal == nil {
		return
	}
	p.InvalidateTenant(principal.TenantID)
}

// InvalidateTenant drops every cached value for tenantID.
func (p *ExternalServiceProvider) InvalidateTenant(tenantID string) {
	p.mu.Lock()
	delete(p.cache, tenantID)
	p.mu.Unlock()
}

func (p *ExternalServiceProvider) cached(tenant, key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cache[tenant][key]
	if !ok {
		return "", false
	}
	if !p.now().Before(e.expires) {
		delete(p.cache[tenant], key)
		return "", false
	}
	return e.value, true
}

func (p *ExternalServiceProvider) store(tenant, key, value string, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.cache[tenant]
	if m == nil {
		m = make(map[string]externalServiceEntry)
		p.cache[tenant] = m
	}
	m[key] = externalServiceEntry{value: value, expires: p.now().Add(ttl)}
}

func (p *ExternalServiceProvider) token(ctx context.Context, tenant, credential string) (string, error) {
	if p.fetch == nil {
		return "", errors.New("no secret store configured")
	}
	token, err := p.fetch(ctx, tenant, credential, externalServiceTokenField)
	if err != nil {
		return "", fmt.Errorf("auth_credential %q: %w", credential, err)
	}
	if token == "" {
		return "", fmt.Errorf("auth_credential %q: field %q is empty", credential, externalServiceTokenField)
	}
	return token, nil
}

func (p *ExternalServiceProvider) call(ctx context.Context, method, target, token string, body io.Reader) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("call credential service: %w", err)
	}
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose releases a request's timeout context once its body is
// closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func externalServiceArgs(cfg map[string]any) (credentialID, field, prefix string, err error) {
	credentialID, _ = cfg["credential_id"].(string)
	field, _ = cfg["field"].(string)
	prefix, _ = cfg["prefix"].(string)
	if credentialID == "" {
		return "", "", "", errors.New("external_service: \"credential_id\" is required")
	}
	if field == "" {
		return "", "", "", errors.New("external_service: \"field\" is required")
	}
	if strings.ContainsAny(prefix, "\r\n") {
		return "", "", "", errors.New("external_service: \"prefix\" must not contain a line break")
	}
	return credentialID, field, prefix, nil
}

func externalServiceSourceArgs(cfg map[string]any) (*url.URL, string, error) {
	rawURL, _ := cfg["url"].(string)
	authCredential, _ := cfg["auth_credential"].(string)
	if rawURL == "" {
		return nil, "", errors.New("\"url\" is required")
	}
	if authCredential == "" {
		return nil, "", errors.New("\"auth_credential\" is required")
	}
	u, err := validateServiceURL(rawURL)
	if err != nil {
		return nil, "", err
	}
	return u, authCredential, nil
}

// internalRanges is a policy with no allowances: CheckIP fails exactly for
// the addresses netguard treats as internal (loopback, private, ...).
var internalRanges = func() *netguard.Policy {
	p, err := netguard.NewPolicy(nil, nil)
	if err != nil {
		panic(err)
	}
	return p
}()

// validateServiceURL accepts an https URL, or an http one whose host is a
// loopback or private address that the installed egress policy
// (egress.allowed_cidrs / egress.allowed_hosts) lets the gateway reach.
// Credentials, a query string and a fragment are refused: the URL is a
// base the gateway appends fixed paths to.
func validateServiceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("\"url\" is not a valid URL")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("\"url\" must include a host")
	}
	if u.User != nil {
		return nil, errors.New("\"url\" must not carry credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("\"url\" must not have a query or fragment")
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
		if err := netguard.CheckHost(host); err != nil {
			return nil, fmt.Errorf("\"url\": %w", err)
		}
	case "http":
		if !plainHTTPAllowed(host) {
			return nil, errors.New("\"url\" must use https (http is accepted only for a loopback or private address the egress policy allows)")
		}
	default:
		return nil, errors.New("\"url\" must use https")
	}
	return u, nil
}

func plainHTTPAllowed(host string) bool {
	policy := netguard.Current()
	ip, ok := netguard.ParseHostIP(host)
	if !ok {
		lower := strings.ToLower(strings.TrimSuffix(host, "."))
		if lower != "localhost" && !strings.HasSuffix(lower, ".localhost") {
			// A hostname: only one the operator allowlisted as internal.
			return policy.HostAllowed(host)
		}
		ip = netip.MustParseAddr("127.0.0.1")
	}
	if internalRanges.CheckIP(ip) == nil {
		return false // a public address: https only
	}
	return policy.HostAllowed(host) || policy.CheckIP(ip) == nil
}
