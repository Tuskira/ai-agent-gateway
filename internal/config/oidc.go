package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// OIDC configures single sign-on for console users (internal/auth/oidc):
// the gateway is a generic OpenID Connect relying party, and each provider
// adds a "Sign in with ..." button to the console login page. With no
// providers SSO is off.
//
// A list cannot be expressed as GATEWAY_* leaf variables, so the whole
// list is set from the YAML file or, as one value, from
// GATEWAY_AUTH_OIDC_PROVIDERS holding a JSON array of provider objects
// (the same keys as the YAML).
type OIDC struct {
	Providers []OIDCProvider `yaml:"providers" env:"json"`
}

// OIDCProvider is one OpenID Connect issuer users can sign in with.
type OIDCProvider struct {
	// Name is the provider's slug, used in its URLs
	// (/api/v1/auth/oidc/<name>/start and .../callback) and recorded on
	// identity links and audit rows. Lowercase letters, digits and "-".
	Name string `yaml:"name"`
	// DisplayName is the button label ("Sign in with <display_name>").
	// Defaults to Name.
	DisplayName string `yaml:"display_name"`

	// Issuer is the OIDC issuer URL; its discovery document is fetched
	// from <issuer>/.well-known/openid-configuration and must name the
	// same issuer. https, or http on a loopback host only.
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client_id"`
	// ClientSecretFile is a file holding the client secret;
	// ClientSecretEnv names an environment variable holding it. Exactly
	// one is required. The secret itself is never part of the config.
	ClientSecretFile string `yaml:"client_secret_file"`
	ClientSecretEnv  string `yaml:"client_secret_env"`
	// Scopes requested at the authorize endpoint. Default
	// [openid, email, profile]; when set it must include openid.
	Scopes []string `yaml:"scopes"`

	// RedirectURL is the callback URL registered at the IdP:
	// <console origin>/api/v1/auth/oidc/<name>/callback, exactly. The
	// origin may be a host that proxies /api/v1/auth/* to the gateway.
	// RedirectURLs adds further registered callbacks when the console is
	// reachable on several hosts; a login uses the one whose host is the
	// host the login started on (the first one when none matches).
	RedirectURL  string   `yaml:"redirect_url"`
	RedirectURLs []string `yaml:"redirect_urls"`

	// AuthParams are extra query parameters on the authorize request,
	// e.g. {"identity_provider": "EntraID"} to have a Cognito user pool
	// forward straight to a federated IdP. The OAuth/OIDC parameters the
	// gateway sets itself cannot be overridden.
	AuthParams map[string]string `yaml:"auth_params"`

	// Tenant is the tenant (slug or id) every login through this provider
	// lands in. Required.
	Tenant string `yaml:"tenant"`
	// AllowedEmailDomains lists the email domains that may sign in, exact
	// match (no subdomains). Required.
	AllowedEmailDomains []string `yaml:"allowed_email_domains"`
	// RequireVerifiedEmail refuses an ID token whose email_verified is not
	// true. Default true. It may be false only together with
	// RequiredUpstreamIdP, which then vouches for the address.
	RequireVerifiedEmail *bool `yaml:"require_verified_email"`
	// RequiredUpstreamIdP, when set, refuses an ID token that does not
	// show the user came through that upstream identity provider: every
	// entry of the token's "identities" claim (the format AWS Cognito
	// uses for federated users) must have this providerName.
	RequiredUpstreamIdP string `yaml:"required_upstream_idp"`

	// Provision decides which users may sign in: "existing" (default)
	// only users an admin already created in Tenant, matched by username
	// == email; "jit" also creates a missing user on first login with
	// JITRole (default viewer; admin allowed, platform-admin never).
	Provision string `yaml:"provision"`
	JITRole   string `yaml:"jit_role"`
	// AllowPlatformAdmin lets a platform-admin user sign in through this
	// provider. Default false: such a user is refused (role) and keeps
	// password login.
	AllowPlatformAdmin bool `yaml:"allow_platform_admin"`
	// SessionMax, when set, caps the absolute lifetime of sessions this
	// provider starts below auth.session_max.
	SessionMax time.Duration `yaml:"session_max"`
}

// Provision modes.
const (
	OIDCProvisionExisting = "existing"
	OIDCProvisionJIT      = "jit"
)

// VerifiedEmailRequired reports the effective require_verified_email.
func (p OIDCProvider) VerifiedEmailRequired() bool {
	return p.RequireVerifiedEmail == nil || *p.RequireVerifiedEmail
}

// AllRedirectURLs is RedirectURL followed by RedirectURLs.
func (p OIDCProvider) AllRedirectURLs() []string {
	out := make([]string, 0, 1+len(p.RedirectURLs))
	if p.RedirectURL != "" {
		out = append(out, p.RedirectURL)
	}
	return append(out, p.RedirectURLs...)
}

// OIDCCallbackPath is the gateway path of a provider's callback route.
func OIDCCallbackPath(name string) string { return "/api/v1/auth/oidc/" + name + "/callback" }

var (
	reOIDCName   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	reOIDCDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// oidcReservedParams are the authorize parameters the gateway sets itself.
var oidcReservedParams = map[string]bool{
	"response_type": true, "client_id": true, "redirect_uri": true, "scope": true, "state": true,
	"nonce": true, "code_challenge": true, "code_challenge_method": true, "response_mode": true,
	"request": true, "request_uri": true,
}

// checkOIDCURL parses an absolute http(s) URL with no query or fragment;
// plain http only on a loopback host.
func checkOIDCURL(field, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s %q must be an absolute URL with no userinfo, query or fragment", field, raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("%s %q must use https (plain http only on a loopback host)", field, raw)
		}
	default:
		return nil, fmt.Errorf("%s %q must use https", field, raw)
	}
	return u, nil
}

// validate checks every provider; globalSessionMax is auth.session_max.
func (o OIDC) validate(globalSessionMax time.Duration) error {
	seen := map[string]bool{}
	for i, p := range o.Providers {
		at := fmt.Sprintf("auth.oidc.providers[%d]", i)
		if !reOIDCName.MatchString(p.Name) {
			return fmt.Errorf("%s: name %q must match %s", at, p.Name, reOIDCName)
		}
		at = fmt.Sprintf("auth.oidc.providers[%s]", p.Name)
		if seen[p.Name] {
			return fmt.Errorf("%s: duplicate provider name", at)
		}
		seen[p.Name] = true
		if len(p.DisplayName) > 64 {
			return fmt.Errorf("%s: display_name is longer than 64 bytes", at)
		}
		if _, err := checkOIDCURL("issuer", p.Issuer); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		if strings.TrimSpace(p.ClientID) == "" {
			return fmt.Errorf("%s: client_id is required", at)
		}
		if (p.ClientSecretFile == "") == (p.ClientSecretEnv == "") {
			return fmt.Errorf("%s: set exactly one of client_secret_file and client_secret_env", at)
		}
		if len(p.Scopes) > 0 {
			hasOpenID := false
			for _, s := range p.Scopes {
				if s == "openid" {
					hasOpenID = true
				}
				if s == "" || strings.ContainsAny(s, " \t\r\n") {
					return fmt.Errorf("%s: invalid scope %q", at, s)
				}
			}
			if !hasOpenID {
				return fmt.Errorf("%s: scopes must include openid", at)
			}
		}

		urls := p.AllRedirectURLs()
		if len(urls) == 0 {
			return fmt.Errorf("%s: redirect_url is required", at)
		}
		for _, raw := range urls {
			u, err := checkOIDCURL("redirect_url", raw)
			if err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			if want := OIDCCallbackPath(p.Name); u.Path != want {
				return fmt.Errorf("%s: redirect_url %q must have the path %s", at, raw, want)
			}
		}

		for k := range p.AuthParams {
			if k == "" || oidcReservedParams[strings.ToLower(k)] {
				return fmt.Errorf("%s: auth_params may not set %q (the gateway sets it)", at, k)
			}
		}

		if strings.TrimSpace(p.Tenant) == "" {
			return fmt.Errorf("%s: tenant is required", at)
		}
		if len(p.AllowedEmailDomains) == 0 {
			return fmt.Errorf("%s: allowed_email_domains is required", at)
		}
		for _, d := range p.AllowedEmailDomains {
			if !reOIDCDomain.MatchString(strings.ToLower(d)) {
				return fmt.Errorf("%s: allowed_email_domains entry %q is not a domain name (no wildcards, no trailing dot)", at, d)
			}
		}
		if !p.VerifiedEmailRequired() && strings.TrimSpace(p.RequiredUpstreamIdP) == "" {
			return fmt.Errorf("%s: require_verified_email: false needs required_upstream_idp, "+
				"or any account at the issuer could claim an unverified address", at)
		}

		switch p.Provision {
		case "", OIDCProvisionExisting:
			if p.JITRole != "" {
				return fmt.Errorf("%s: jit_role is only valid with provision: jit", at)
			}
		case OIDCProvisionJIT:
			switch p.JITRole {
			case "", "viewer", "admin":
			default:
				return fmt.Errorf("%s: jit_role %q must be viewer or admin", at, p.JITRole)
			}
		default:
			return fmt.Errorf("%s: provision %q must be existing or jit", at, p.Provision)
		}

		if p.SessionMax < 0 || p.SessionMax > globalSessionMax {
			return fmt.Errorf("%s: session_max (%s) must be between 0 and auth.session_max (%s)", at, p.SessionMax, globalSessionMax)
		}
	}
	return nil
}
