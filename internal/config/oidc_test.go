package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validOIDCProvider() OIDCProvider {
	return OIDCProvider{
		Name: "corp", Issuer: "https://idp.example.com/realms/corp", ClientID: "gateway",
		ClientSecretFile: "/run/secrets/oidc", RedirectURL: "https://console.example.com/api/v1/auth/oidc/corp/callback",
		Tenant: "default", AllowedEmailDomains: []string{"example.com"},
	}
}

func TestOIDCValidate(t *testing.T) {
	f := false
	cases := []struct {
		name string
		edit func(p *OIDCProvider)
		want string // "" = valid
	}{
		{"valid", func(*OIDCProvider) {}, ""},
		{"loopback http issuer and callback", func(p *OIDCProvider) {
			p.Issuer = "http://127.0.0.1:18180/realms/x"
			p.RedirectURL = "http://localhost:18081/api/v1/auth/oidc/corp/callback"
		}, ""},
		{"extra redirect urls", func(p *OIDCProvider) {
			p.RedirectURLs = []string{"https://other.example.com/api/v1/auth/oidc/corp/callback"}
		}, ""},
		{"unverified with upstream idp", func(p *OIDCProvider) { p.RequireVerifiedEmail = &f; p.RequiredUpstreamIdP = "EntraID" }, ""},
		{"jit admin", func(p *OIDCProvider) { p.Provision = "jit"; p.JITRole = "admin" }, ""},
		{"email claim", func(p *OIDCProvider) { p.EmailClaim = "custom:upn" }, ""},
		{"bad email claim", func(p *OIDCProvider) { p.EmailClaim = "e mail" }, "email_claim"},
		{"bad name", func(p *OIDCProvider) { p.Name = "Corp SSO" }, "name"},
		{"plain http issuer", func(p *OIDCProvider) { p.Issuer = "http://idp.example.com" }, "https"},
		{"issuer with query", func(p *OIDCProvider) { p.Issuer = "https://idp.example.com/?x=1" }, "absolute URL"},
		{"no client id", func(p *OIDCProvider) { p.ClientID = "" }, "client_id"},
		{"no secret", func(p *OIDCProvider) { p.ClientSecretFile = "" }, "exactly one"},
		{"two secrets", func(p *OIDCProvider) { p.ClientSecretEnv = "X" }, "exactly one"},
		{"scopes without openid", func(p *OIDCProvider) { p.Scopes = []string{"email"} }, "openid"},
		{"no redirect", func(p *OIDCProvider) { p.RedirectURL = "" }, "redirect_url is required"},
		{"redirect wrong path", func(p *OIDCProvider) { p.RedirectURL = "https://console.example.com/callback" }, "path"},
		{"redirect other provider path", func(p *OIDCProvider) {
			p.RedirectURL = "https://console.example.com/api/v1/auth/oidc/other/callback"
		}, "path"},
		{"redirect plain http", func(p *OIDCProvider) {
			p.RedirectURL = "http://console.example.com/api/v1/auth/oidc/corp/callback"
		}, "https"},
		{"reserved auth param", func(p *OIDCProvider) { p.AuthParams = map[string]string{"redirect_uri": "x"} }, "auth_params"},
		{"reserved auth param any case", func(p *OIDCProvider) { p.AuthParams = map[string]string{"State": "x"} }, "auth_params"},
		{"no tenant", func(p *OIDCProvider) { p.Tenant = " " }, "tenant"},
		{"no domains", func(p *OIDCProvider) { p.AllowedEmailDomains = nil }, "allowed_email_domains"},
		{"wildcard domain", func(p *OIDCProvider) { p.AllowedEmailDomains = []string{"*.example.com"} }, "allowed_email_domains"},
		{"trailing dot domain", func(p *OIDCProvider) { p.AllowedEmailDomains = []string{"example.com."} }, "allowed_email_domains"},
		{"email as domain", func(p *OIDCProvider) { p.AllowedEmailDomains = []string{"a@example.com"} }, "allowed_email_domains"},
		{"unverified without upstream idp", func(p *OIDCProvider) { p.RequireVerifiedEmail = &f }, "required_upstream_idp"},
		{"bad provision", func(p *OIDCProvider) { p.Provision = "all" }, "provision"},
		{"jit_role without jit", func(p *OIDCProvider) { p.JITRole = "viewer" }, "jit_role"},
		{"jit platform-admin", func(p *OIDCProvider) { p.Provision = "jit"; p.JITRole = "platform-admin" }, "jit_role"},
		{"session_max above global", func(p *OIDCProvider) { p.SessionMax = 48 * time.Hour }, "session_max"},
		{"long display name", func(p *OIDCProvider) { p.DisplayName = strings.Repeat("x", 65) }, "display_name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			p := validOIDCProvider()
			tc.edit(&p)
			cfg.Auth.OIDC.Providers = []OIDCProvider{p}
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}

	cfg := Default()
	cfg.Auth.OIDC.Providers = []OIDCProvider{validOIDCProvider(), validOIDCProvider()}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate names: Validate error = %v", err)
	}
}

func TestOIDCVerifiedEmailDefault(t *testing.T) {
	p := validOIDCProvider()
	if !p.VerifiedEmailRequired() {
		t.Error("require_verified_email must default to true")
	}
	if p.EmailClaimName() != "email" {
		t.Errorf("email_claim default = %q, want email", p.EmailClaimName())
	}
}

func TestOIDCProvidersFromYAMLAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.yml")
	yml := `auth:
  oidc:
    providers:
      - name: keycloak
        display_name: Keycloak
        issuer: https://idp.example.com/realms/corp
        client_id: gateway
        client_secret_env: KC_SECRET
        redirect_url: https://console.example.com/api/v1/auth/oidc/keycloak/callback
        tenant: default
        allowed_email_domains: [example.com]
        auth_params: {kc_idp_hint: corp}
`
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_MASTER_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(yaml): %v", err)
	}
	ps := cfg.Auth.OIDC.Providers
	if len(ps) != 1 || ps[0].Name != "keycloak" || ps[0].AuthParams["kc_idp_hint"] != "corp" || !ps[0].VerifiedEmailRequired() {
		t.Fatalf("providers = %+v", ps)
	}

	t.Setenv("GATEWAY_AUTH_OIDC_PROVIDERS", `[{"name":"cognito","issuer":"https://cognito-idp.us-east-1.amazonaws.com/us-east-1_example",
		"client_id":"abc","client_secret_file":"/run/secrets/c","redirect_url":"https://console.example.com/api/v1/auth/oidc/cognito/callback",
		"tenant":"default","allowed_email_domains":["example.com"],"require_verified_email":false,"required_upstream_idp":"EntraID",
		"auth_params":{"identity_provider":"EntraID"}}]`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load(env): %v", err)
	}
	ps = cfg.Auth.OIDC.Providers
	if len(ps) != 1 || ps[0].Name != "cognito" || ps[0].VerifiedEmailRequired() || ps[0].AuthParams["identity_provider"] != "EntraID" {
		t.Fatalf("env providers = %+v (the env value replaces the file's list)", ps)
	}

	t.Setenv("GATEWAY_AUTH_OIDC_PROVIDERS", `[{"name":"x","bogus_key":1}]`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "GATEWAY_AUTH_OIDC_PROVIDERS") {
		t.Errorf("unknown key in env JSON: Load error = %v", err)
	}
	t.Setenv("GATEWAY_AUTH_OIDC_PROVIDERS", `not json [`)
	if _, err := Load(path); err == nil {
		t.Error("malformed env JSON loaded")
	}
}

func TestUnknownEnvVarsKnowsClientSecretEnv(t *testing.T) {
	cfg := Default()
	p := validOIDCProvider()
	p.ClientSecretFile, p.ClientSecretEnv = "", "GATEWAY_OIDC_CORP_SECRET"
	cfg.Auth.OIDC.Providers = []OIDCProvider{p}
	t.Setenv("GATEWAY_OIDC_CORP_SECRET", "s")
	t.Setenv("GATEWAY_AUTH_OIDC_PROVIDERS", "[]")
	for _, name := range UnknownEnvVars(cfg) {
		if name == "GATEWAY_OIDC_CORP_SECRET" || name == "GATEWAY_AUTH_OIDC_PROVIDERS" {
			t.Errorf("%s reported as unknown", name)
		}
	}
}
