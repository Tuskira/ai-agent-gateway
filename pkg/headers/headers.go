// Package headers defines the gateway's outbound-header credential
// resolution contract. A connector's metadata.headers map names each
// outbound header and how to compute it: statically, from a Principal
// field, from an incoming request header, or by dispatching to an
// ExternalProvider (secret_store, env, file, gcp_wif,
// azure_msal_from_headers, http, ...).
//
// This package defines interfaces only; the resolver registry and the
// built-in resolver/provider implementations are built in a later commit.
package headers

import "context"

// HeaderResolver computes the value of one outbound header from a header
// config (the value of one entry in a connector's metadata.headers map).
// Type identifies the resolver kind (e.g. "static", "token_field",
// "incoming_field", "external"); Validate checks cfg at connector
// save-time; Resolve computes the header value at request time.
type HeaderResolver interface {
	Type() string
	Validate(cfg map[string]any) error
	Resolve(ctx context.Context, cfg map[string]any) (string, error)
}

// ExternalProvider is a HeaderResolver reachable via header config
// {"type": "external", "provider": ProviderID(), "config": {...}}. Each
// provider (secret_store, env, file, gcp_wif, azure_msal_from_headers,
// http, ...) implements this to plug into the "external" resolver type.
type ExternalProvider interface {
	HeaderResolver

	// ProviderID is the stable, machine-readable id used in header config
	// (e.g. "secret_store"). ProviderName is the human-readable label
	// shown in the admin UI's provider list.
	ProviderID() string
	ProviderName() string

	// ConfigSchema describes this provider's config shape (the value of
	// header config's "config" field) for GET /api/v1/headers/providers,
	// which drives the UI's schema-generated provider form.
	ConfigSchema() map[string]any
}

// Invalidator is implemented by providers whose resolved values are
// cached and can go stale (e.g. after a backend returns 401). The MCP
// plane's failure-handling step calls Invalidate(principal, connector) to
// evict any cached value before retrying once.
type Invalidator interface {
	Invalidate(ctx context.Context, principalID, connectorSlug string)
}

// CredentialFetcher resolves one field of a named credential for a tenant.
// It is the seam through which providers that need raw secret material
// (e.g. gcp_wif's credential_config) reach the secret store, without a
// direct dependency on its package.
type CredentialFetcher func(ctx context.Context, tenantID, credentialName, field string) (string, error)

// ---------------------------------------------------------------------------
// Credential sources
// ---------------------------------------------------------------------------

// Source is a tenant-scoped credential source: a named, stored set of
// non-secret settings (for example the base URL of an outside credential
// service and the name of the gateway credential that authenticates to
// it) that a provider needs in addition to one header's own config.
//
// A header opts into a source with a top-level "source" key next to
// "provider":
//
//	{"type": "external", "provider": "external_service", "source": "vault-a",
//	 "config": {"credential_id": "splunk-prod", "field": "token"}}
//
// The header registry loads the named source for the caller's own tenant,
// checks it is enabled and belongs to the same provider, and hands it to
// the provider through the request context (see ResolveInfoFrom). The
// ExternalProvider.Resolve signature is unchanged, so providers that do
// not use sources are unaffected.
type Source struct {
	ID       string
	TenantID string
	Name     string
	Provider string
	Enabled  bool
	// Config is the source's non-secret settings, shaped by the provider's
	// SourceSchema.
	Config map[string]any
}

// SourceSchemaProvider is implemented by an ExternalProvider whose headers
// need a credential source. SourceSchema describes a source's Config for
// the admin UI (GET /api/v1/headers/providers returns it as
// source_schema); ValidateSource checks a source's Config when it is
// created or updated.
type SourceSchemaProvider interface {
	SourceSchema() map[string]any
	ValidateSource(cfg map[string]any) error
}

// SourceTester is implemented by a SourceSchemaProvider that can check a
// source's reachability and authentication without resolving any secret
// (POST /api/v1/credential-sources/{id}/test).
type SourceTester interface {
	TestSource(ctx context.Context, src Source) error
}

// Option is one value a provider offers for a header config field, with a
// human-readable label (the value itself when the service gives none).
type Option struct {
	Value string
	Label string
}

// SourceOptionLister is implemented by a SourceSchemaProvider that can ask
// its outside service which values a header config field accepts (for
// example the credential ids the service holds for the tenant), so a
// console can offer a picker instead of a free-text box
// (GET /api/v1/credential-sources/{id}/options?field=...). Like
// SourceTester it never returns a secret value.
type SourceOptionLister interface {
	// OptionFields names the header config fields ListOptions answers for.
	OptionFields() []string
	// ListOptions returns the values src's service offers for field, one
	// of OptionFields.
	ListOptions(ctx context.Context, src Source, field string) ([]Option, error)
}

// ResolveInfo is request-scoped context a provider may read while
// resolving one header: which connector and header it is resolving for,
// and the credential source the header names (nil when it names none).
// Every field is optional; a provider must cope with any being empty.
type ResolveInfo struct {
	ConnectorSlug string
	HeaderName    string
	Source        *Source
}

type resolveInfoKey struct{}

// WithResolveInfo returns ctx carrying info.
func WithResolveInfo(ctx context.Context, info ResolveInfo) context.Context {
	return context.WithValue(ctx, resolveInfoKey{}, info)
}

// ResolveInfoFrom returns the ResolveInfo carried by ctx, if any.
func ResolveInfoFrom(ctx context.Context) (ResolveInfo, bool) {
	info, ok := ctx.Value(resolveInfoKey{}).(ResolveInfo)
	return info, ok
}
