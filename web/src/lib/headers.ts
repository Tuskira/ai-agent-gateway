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
}

export interface HeaderProvidersResponse {
  providers: HeaderProvider[]
  resolver_types: string[]
}

/** `GET /api/v1/headers/providers` */
export function fetchHeaderProviders(): Promise<HeaderProvidersResponse> {
  return apiFetch<HeaderProvidersResponse>('/headers/providers')
}
