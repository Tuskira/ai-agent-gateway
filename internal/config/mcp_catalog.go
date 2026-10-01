package config

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Tuskira/tusk-ai-secured-gateway/configs"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
)

// MCP catalog auth kinds. Kept here (not in pkg/store, which imports this
// package) so the config seed is validated with the same vocabulary the
// add-to-tenant handler acts on.
const (
	MCPAuthNone   = "none"
	MCPAuthBearer = "bearer"
	MCPAuthBasic  = "basic"
	MCPAuthHeader = "header"
	MCPAuthOAuth  = "oauth"
)

var (
	reMCPCatalogSlug  = regexp.MustCompile(`^[a-z0-9-]+$`)
	reMCPCatalogField = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	reHeaderName      = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// ValidateMCPCatalogSeed checks one mcp_catalog.seed entry: a well-formed
// slug and https URL, a known auth kind, and a field list that fits that
// kind (none/oauth take no credential field, bearer/header exactly one,
// basic exactly two; a field with a query name is routed into the URL and
// is not a credential field, so it may not be secret). ctx bounds the DNS
// lookup ValidateMCPCatalogURL performs (see netguard.CheckHostResolve);
// pass context.Background() when validating a YAML-seeded entry at
// startup, where no request-scoped context exists.
func ValidateMCPCatalogSeed(ctx context.Context, s MCPCatalogSeed) error {
	if !reMCPCatalogSlug.MatchString(s.Slug) || len(s.Slug) > 100 {
		return fmt.Errorf("slug %q must be 1-100 characters of lowercase letters, numbers and hyphens", s.Slug)
	}
	if n := len([]rune(s.Name)); n < 1 || n > 120 {
		return fmt.Errorf("name must be between 1 and 120 characters")
	}
	if err := ValidateMCPCatalogURL(ctx, "url", s.URL); err != nil {
		return err
	}
	if s.DocsURL != "" {
		if err := ValidateMCPCatalogURL(ctx, "docs_url", s.DocsURL); err != nil {
			return err
		}
	}
	if s.Transport != "" && s.Transport != "streamable-http" {
		return fmt.Errorf("transport %q is not supported (only streamable-http)", s.Transport)
	}
	for name, value := range s.DefaultHeaders {
		if !reHeaderName.MatchString(name) {
			return fmt.Errorf("default_headers: %q is not a valid header name", name)
		}
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("default_headers: %q needs a non-empty, single-line value", name)
		}
	}

	seen := map[string]bool{}
	credFields := 0
	for i, f := range s.Auth.Fields {
		if !reMCPCatalogField.MatchString(f.Name) {
			return fmt.Errorf("auth.fields[%d]: name %q must match %s", i, f.Name, reMCPCatalogField.String())
		}
		if seen[f.Name] {
			return fmt.Errorf("auth.fields[%d]: duplicate name %q", i, f.Name)
		}
		seen[f.Name] = true
		if strings.TrimSpace(f.Label) == "" {
			return fmt.Errorf("auth.fields[%d] (%s): label is required", i, f.Name)
		}
		if f.Query != "" {
			if f.Secret {
				return fmt.Errorf("auth.fields[%d] (%s): a query field is part of the URL and cannot be secret", i, f.Name)
			}
			continue
		}
		credFields++
	}

	want := -1
	switch s.Auth.Kind {
	case MCPAuthNone, MCPAuthOAuth:
		want = 0
	case MCPAuthBearer, MCPAuthHeader:
		want = 1
	case MCPAuthBasic:
		want = 2
	default:
		return fmt.Errorf("auth.kind %q must be one of none, bearer, basic, header, oauth", s.Auth.Kind)
	}
	if credFields != want {
		return fmt.Errorf("auth.kind %q takes %d credential field(s), got %d", s.Auth.Kind, want, credFields)
	}
	if s.Auth.Kind == MCPAuthHeader && (s.Auth.HeaderTemplate == nil || !reHeaderName.MatchString(s.Auth.HeaderTemplate.Name)) {
		return fmt.Errorf("auth.header_template.name is required for kind header")
	}
	if ht := s.Auth.HeaderTemplate; ht != nil && ht.Name != "" && !reHeaderName.MatchString(ht.Name) {
		return fmt.Errorf("auth.header_template.name %q is not a valid header name", ht.Name)
	}
	return nil
}

// ValidateMCPCatalogURL requires an absolute https URL, or http for a
// loopback host (localhost, 127.0.0.0/8, ::1) so a local MCP server can be
// catalogued during development.
func ValidateMCPCatalogURL(ctx context.Context, field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q must be an absolute https URL", field, raw)
	}
	if err := netguard.CheckHostResolve(ctx, u.Hostname()); err != nil {
		return fmt.Errorf("%s %q: %w", field, raw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip, ok := netguard.ParseHostIP(host); ok && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%s %q must be an absolute https URL (http is allowed for loopback hosts only)", field, raw)
}

// defaultMCPCatalog is the mcp_catalog section of configs/base/default.yml,
// so the shipped catalog is seeded without a CONFIG_PATH. A config file
// that sets mcp_catalog.seed replaces it; `seed: []` turns it off.
func defaultMCPCatalog() MCPCatalog {
	var f struct {
		MCPCatalog MCPCatalog `yaml:"mcp_catalog"`
	}
	if err := yaml.Unmarshal(configs.DefaultYAML, &f); err != nil {
		panic("configs/base/default.yml: " + err.Error())
	}
	return f.MCPCatalog
}
