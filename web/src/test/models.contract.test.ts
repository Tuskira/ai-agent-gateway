import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/lib/api'
import {
  createModel,
  fetchModelsSummary,
  listRegisteredModels,
  mergeModelRows,
  updateModel,
} from '@/lib/models'
import { contract } from './fixtures/contract'
import { expectModelShape, expectSummaryShape } from './fixtures/shapes'

/**
 * The console's types against the real handlers' output
 * (`fixtures/models-api.json`, produced and checked by
 * `internal/api/handlers/models_contract_test.go`).
 *
 * The fixture's JSON is held to the types' own key lists
 * (`fixtures/shapes.ts`), so the types cannot drift from the API in either
 * direction without a test failing here or in the Go test.
 */
function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('model registry API contract (real handler output)', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('every model the API returns has exactly the keys RegisteredModel declares', () => {
    expect(contract.create.status).toBe(201)
    expect(contract.update.status).toBe(200)
    expectModelShape('create', contract.create.response)
    expectModelShape('seed', contract.seed.response)
    expectModelShape('update', contract.update.response)
    expect(Object.keys(contract.list.response).sort()).toEqual(['items', 'total'])
    expect(contract.list.response.total).toBe(contract.list.response.items.length)
    contract.list.response.items.forEach((m) => expectModelShape(`list:${m.name}`, m))
  })

  it('covers the fields the console relies on: scope, allow_caller_key, limits, null price', () => {
    const items = contract.list.response.items
    expect(items.map((m) => [m.name, m.scope])).toEqual([
      ['haiku', 'platform'],
      ['local-coder', 'tenant'],
      ['team-sonnet', 'tenant'],
    ])
    const coder = items.find((m) => m.name === 'local-coder')!
    expect(coder.targets.map((t) => t.allow_caller_key)).toEqual([true, undefined])
    expect(coder.targets.map((t) => t.label)).toEqual(['ollama', undefined])
    expect(coder.targets[1]!.credential).toBe('anthropic-prod')
    expect(coder.limits).toEqual({ rpm: 5, max_tokens: 1024 })
    expect(coder.price).toBeNull()
    const platform = items.find((m) => m.name === 'haiku')!
    expect(platform.limits).toBeNull()
    expect(platform.description).toBe('')
    expect(platform.metadata).toEqual({})
    expect(contract.update.response.limits).toEqual({
      monthly_usd: 20,
      rpm: 5,
      max_tokens: 1024,
    })
  })

  it('GET /analytics/models has exactly the keys ModelsSummary declares', () => {
    const summary = contract.analytics_models.response
    expectSummaryShape('summary', summary)
    expect(summary.models).toHaveLength(2)
    expect(summary.highest_traffic).toEqual({ name: 'local-coder', tokens: 3400 })
    // An unpriced row is null, never a fabricated 0.
    expect(summary.models.map((r) => r.cost_usd)).toEqual([0.0421, null])
  })

  it('merges the real registry and traffic bodies by requested name', () => {
    const rows = mergeModelRows(
      contract.list.response.items,
      contract.analytics_models.response.models,
    )
    expect(rows.map((r) => [r.name, r.status, r.provider, r.calls])).toEqual([
      ['local-coder', 'active', 'openai_compat', 12],
      ['claude-sonnet-4-5', 'discovered', 'anthropic', 1],
      ['haiku', 'registered', 'anthropic', 0],
      ['team-sonnet', 'registered', 'anthropic', 0],
    ])
    expect(rows[1]!.registered).toBeNull()
  })

  it('the client functions send the fixture requests and return the fixture responses', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url === '/api/v1/models' && init?.method === 'POST') {
        return jsonResponse(contract.create.response, contract.create.status)
      }
      if (
        url === `/api/v1/models/${contract.seed.response.id}` &&
        init?.method === 'PUT'
      ) {
        return jsonResponse(contract.update.response, contract.update.status)
      }
      if (url === '/api/v1/models') return jsonResponse(contract.list.response)
      if (url === '/api/v1/analytics/models?range=7d') {
        return jsonResponse(contract.analytics_models.response)
      }
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    await expect(createModel(contract.create.request)).resolves.toEqual(
      contract.create.response,
    )
    await expect(
      updateModel(contract.seed.response.id, contract.update.request),
    ).resolves.toEqual(contract.update.response)
    await expect(listRegisteredModels()).resolves.toEqual(contract.list.response)
    await expect(fetchModelsSummary()).resolves.toEqual(
      contract.analytics_models.response,
    )

    const sent = fetchMock.mock.calls
      .filter(([, init]) => init?.method === 'POST' || init?.method === 'PUT')
      .map(([, init]) => JSON.parse(init!.body as string))
    expect(sent).toEqual([contract.create.request, contract.update.request])
  })

  it("surfaces the API's validation envelope as an ApiError", async () => {
    expect(contract.invalid.status).toBe(400)
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse(contract.invalid.response, contract.invalid.status)),
    )
    const err = await createModel(contract.invalid.request).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({
      status: 400,
      type: 'validation_error',
      message: contract.invalid.response.error.message,
    })
    expect((err as ApiError).message).toMatch(
      /allow_caller_key cannot be combined with credential/,
    )
  })
})
