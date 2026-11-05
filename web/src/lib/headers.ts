import { apiFetch } from '@/lib/api'

/** Data contract for `/headers/providers` — drives the schema-driven
 * "external" header provider form in the connector create/edit dialog. */

export interface JsonSchemaProperty {
  type?: string
  title?: string
  description?: string
  default?: unknown
}

export interface HeaderProviderConfigSchema {
  properties: Record<string, JsonSchemaProperty>
  required?: string[]
}

export interface HeaderProvider {
  id: string
  name: string
  config_schema: HeaderProviderConfigSchema
  /** True when a header of this provider must name a credential source
   * (`source` next to `provider`). Absent on older gateways, which still
   * send `source_schema` for such a provider. */
  needs_source?: boolean
  /** The shape of a source's config for this provider. */
  source_schema?: HeaderProviderConfigSchema
  /** The tenant's enabled source names for this provider. */
  sources?: string[]
  /** The same sources with their ids, which the options route is addressed
   * by (readable without `admin.manage`, unlike `/credential-sources`). */
  source_refs?: { id: string; name: string }[]
  /** `config_schema` fields the provider can list values for through
   * `GET /credential-sources/{id}/options`. */
  option_fields?: string[]
}

/** Whether headers of `provider` must name a credential source. */
export function providerNeedsSource(provider: HeaderProvider | undefined): boolean {
  return Boolean(provider && (provider.needs_source ?? provider.source_schema))
}

export interface HeaderProvidersResponse {
  providers: HeaderProvider[]
  resolver_types: string[]
}

/** `GET /api/v1/headers/providers` */
export function fetchHeaderProviders(): Promise<HeaderProvidersResponse> {
  return apiFetch<HeaderProvidersResponse>('/headers/providers')
}
