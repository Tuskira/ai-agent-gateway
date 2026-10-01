import { describe, expect, it } from 'vitest'
import { ApiError } from '@/lib/api'
import {
  capabilityWarnings,
  defaultCredentialName,
  mergeModelCatalogRows,
  usageCountFromError,
  type CatalogModel,
  type CatalogProvider,
  type ModelCatalog,
} from '@/lib/model-catalog'
import type { MergedModelRow, RegisteredModel } from '@/lib/models'

function catalogModel(overrides: Partial<CatalogModel> = {}): CatalogModel {
  return {
    id: 'catm1',
    model_id: 'zai-org/GLM-5.3',
    display_name: 'GLM 5.3',
    suggested_name: 'glm-5.3-nebius',
    price: null,
    capabilities: { tools: true, vision: null, streaming: null, max_context: null },
    notes: '',
    enabled: true,
    tenant_model_id: null,
    tenant_model_name: null,
    ...overrides,
  }
}

function provider(overrides: Partial<CatalogProvider> = {}): CatalogProvider {
  return {
    id: 'prov1',
    slug: 'nebius',
    display_name: 'Nebius AI Studio',
    vendor: 'openai_compat',
    base_url: 'https://api.tokenfactory.eu-west2.nebius.com/v1',
    docs_url: '',
    enabled: true,
    models: [catalogModel()],
    ...overrides,
  }
}

function catalog(providers: CatalogProvider[]): ModelCatalog {
  return { providers }
}

function registeredModel(overrides: Partial<RegisteredModel> = {}): RegisteredModel {
  return {
    id: 'm1',
    name: 'glm-5.3-nebius',
    description: '',
    enabled: true,
    targets: [{ vendor: 'openai_compat', model: 'zai-org/GLM-5.3' }],
    price: null,
    limits: null,
    scope: 'tenant',
    metadata: {},
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

function row(overrides: Partial<MergedModelRow> = {}): MergedModelRow {
  return {
    key: 'm1',
    name: 'glm-5.3-nebius',
    provider: 'openai_compat',
    calls: 0,
    tokens: 0,
    costUsd: null,
    usedBy: 0,
    lastSeen: null,
    status: 'registered',
    registered: null,
    ...overrides,
  }
}

describe('mergeModelCatalogRows', () => {
  it('is a no-op when the catalog is null or undefined', () => {
    const rows = [row()]
    expect(mergeModelCatalogRows(rows, null)).toEqual(rows)
    expect(mergeModelCatalogRows(rows, undefined)).toEqual(rows)
  })

  it('adds an "available" row for a catalog model this tenant has not set up', () => {
    const rows = mergeModelCatalogRows([], catalog([provider()]))
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      key: 'catalog:catm1',
      name: 'glm-5.3-nebius',
      provider: 'Nebius AI Studio',
      status: 'available',
      registered: null,
      calls: 0,
      tokens: 0,
      costUsd: null,
      catalogSetup: { providerId: 'prov1', modelId: 'catm1' },
    })
  })

  it('shows the catalog provider name on a registered row already connected from it', () => {
    const rows = mergeModelCatalogRows(
      [
        row({
          key: 'm1',
          provider: 'openai_compat',
          status: 'registered',
          registered: registeredModel(),
        }),
      ],
      catalog([provider({ models: [catalogModel({ tenant_model_id: 'm1', tenant_model_name: 'glm-5.3-nebius' })] })]),
    )
    expect(rows).toHaveLength(1)
    expect(rows[0]!.provider).toBe('Nebius AI Studio')
    expect(rows[0]!.status).toBe('registered')
    expect(rows[0]!.catalogSetup).toBeUndefined()
  })

  it('gives a "discovered" row a catalogSetup ref when its name matches a catalog model, without duplicating it as available', () => {
    const rows = mergeModelCatalogRows(
      [row({ key: 'glm-5.3-nebius', status: 'discovered', registered: null })],
      catalog([provider()]),
    )
    expect(rows).toHaveLength(1)
    expect(rows[0]!.status).toBe('discovered')
    expect(rows[0]!.catalogSetup).toEqual({ providerId: 'prov1', modelId: 'catm1' })
  })

  it('matches a "discovered" row by raw model id too', () => {
    const rows = mergeModelCatalogRows(
      [row({ key: 'zai-org/GLM-5.3', name: 'zai-org/GLM-5.3', status: 'discovered' })],
      catalog([provider()]),
    )
    expect(rows[0]!.catalogSetup).toEqual({ providerId: 'prov1', modelId: 'catm1' })
  })
})

describe('capabilityWarnings', () => {
  it('warns when tool calling is explicitly unsupported', () => {
    const warnings = capabilityWarnings(
      catalogModel({ capabilities: { tools: false, vision: null, streaming: null, max_context: null } }),
    )
    expect(warnings).toContain(
      'No tool calling: will not work with Claude Code or other agents that use tools',
    )
  })

  it('warns when tool support is unknown, distinctly from unsupported', () => {
    const warnings = capabilityWarnings(
      catalogModel({ capabilities: { tools: null, vision: null, streaming: null, max_context: null } }),
    )
    expect(warnings).toEqual(['Tool support unknown', "No price set: cost and dollar budgets won't apply until an admin sets one"])
  })

  it('does not warn about tools when they are known to work', () => {
    const warnings = capabilityWarnings(
      catalogModel({
        capabilities: { tools: true, vision: null, streaming: null, max_context: null },
        price: { input: 1, output: 2 },
      }),
    )
    expect(warnings).toEqual([])
  })

  it('warns when price is unset', () => {
    const warnings = capabilityWarnings(catalogModel({ price: null }))
    expect(warnings).toContain(
      "No price set: cost and dollar budgets won't apply until an admin sets one",
    )
  })
})

describe('defaultCredentialName', () => {
  it('suffixes the provider slug', () => {
    expect(defaultCredentialName('nebius')).toBe('nebius-api-key')
  })
})

describe('usageCountFromError', () => {
  it('reads a tenant_models count off the error body when present', () => {
    const err = new ApiError(409, 'conflict', 'in use', { tenant_models: 3 })
    expect(usageCountFromError(err)).toBe(3)
  })

  it('returns null when there is no usable count', () => {
    expect(usageCountFromError(new ApiError(409, 'conflict', 'in use'))).toBeNull()
    expect(usageCountFromError(new Error('not an ApiError'))).toBeNull()
  })
})
