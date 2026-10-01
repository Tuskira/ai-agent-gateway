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
