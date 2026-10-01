import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/lib/api'
import {
  createModel,
  deleteModel,
  fetchModelsSummary,
  listRegisteredModels,
  updateModel,
  type ModelInput,
} from '@/lib/models'
import { expectModelShape, expectSummaryShape } from './fixtures/shapes'

/**
 * The console's model-registry client against a RUNNING gateway: no stub,
 * the real API plane, Postgres and (for the traffic summary) ClickHouse.
 * Skipped unless both are set:
 *
 *   GATEWAY_LIVE_API_URL    e.g. http://127.0.0.1:8081
 *   GATEWAY_LIVE_ADMIN_KEY  an admin key (`gateway bootstrap-key`)
 *
 * It creates, edits and deletes one model of its own (`live-<random>`).
 */
const base = process.env.GATEWAY_LIVE_API_URL
const key = process.env.GATEWAY_LIVE_ADMIN_KEY

describe.skipIf(!base || !key)('model registry, live', () => {
  const name = `live-${Math.random().toString(36).slice(2, 10)}`
  const realFetch = globalThis.fetch
  let createdId = ''

  beforeAll(() => {
    // The console calls same-origin `/api/v1/...`; point that at the gateway.
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) =>
      realFetch(`${base}${String(input)}`, {
        ...init,
        headers: {
          ...(init?.headers as Record<string, string>),
          Authorization: `Bearer ${key}`,
        },
      }),
    )
  })

  afterAll(async () => {
    if (createdId) await deleteModel(createdId).catch(() => undefined)
    vi.unstubAllGlobals()
  })

  const input: ModelInput = {
    name,
    description: 'created by models.live.test.ts',
    enabled: true,
    targets: [
      {
        vendor: 'openai_compat',
        model: 'qwen2.5:1.5b',
        base_url: 'http://host.docker.internal:11434/v1',
        allow_caller_key: true,
        label: 'ollama',
      },
      {
        vendor: 'bedrock',
        model: 'us.anthropic.claude-sonnet-4-5-20250929-v1:0',
        region: 'us-east-1',
      },
    ],
    price: { input: 3, output: 15, cache_read: 0.3 },
    limits: { daily_usd: 5, rpm: 60, max_tokens: 4096 },
  }

  it('creates a model and gets back exactly the declared shape', async () => {
    const created = await createModel(input)
    createdId = created.id
    expectModelShape('created', created)
    expect(created).toMatchObject({ ...input, scope: 'tenant', metadata: {} })
  })

  it('lists it, with scope, allow_caller_key and limits', async () => {
    const page = await listRegisteredModels()
    expect(Object.keys(page).sort()).toEqual(['items', 'total'])
    page.items.forEach((m) => expectModelShape(`list:${m.name}`, m))
    const mine = page.items.find((m) => m.id === createdId)
    expect(mine).toMatchObject({ name, scope: 'tenant', limits: input.limits })
    expect(mine!.targets.map((t) => t.allow_caller_key)).toEqual([true, undefined])
    expect(mine!.targets.map((t) => t.label)).toEqual(['ollama', undefined])
  })

  it('replaces it on update: limits changed, price dropped', async () => {
    const updated = await updateModel(createdId, {
      name,
      enabled: false,
      targets: input.targets,
      limits: { rpm: 5 },
    })
    expectModelShape('updated', updated)
    expect(updated).toMatchObject({
      enabled: false,
      price: null,
      limits: { rpm: 5 },
      description: '',
    })
  })

  it('surfaces validation errors as ApiError', async () => {
    const bad = { ...input, name: `${name}-bad`, limits: {} }
    const err = await createModel(bad).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 400, type: 'validation_error' })
    // http is for the machine the gateway runs on only (loopback, or
    // host.docker.internal matched exactly).
    const remote = {
      ...input,
      name: `${name}-remote`,
      targets: [
        { ...input.targets[0]!, base_url: 'http://host.docker.internal.example.com/v1' },
      ],
    }
    const err2 = await createModel(remote).catch((e: unknown) => e)
    expect(err2).toMatchObject({ status: 400, type: 'validation_error' })
    expect((err2 as ApiError).message).toMatch(/must use https/)
    // A label may not take a native wire's name.
    const reserved = {
      ...input,
      name: `${name}-label`,
      targets: [{ ...input.targets[0]!, label: 'anthropic' }],
    }
    const err3 = await createModel(reserved).catch((e: unknown) => e)
    expect(err3).toMatchObject({ status: 400, type: 'validation_error' })
    expect((err3 as ApiError).message).toMatch(/label "anthropic" is reserved/)
  })

  it('reads the traffic summary in the declared shape', async () => {
    const summary = await fetchModelsSummary()
    // null only when the gateway runs without ClickHouse.
    expect(summary).not.toBeNull()
    expectSummaryShape('summary', summary!)
    expect(summary!.total_models).toBe(summary!.models.length)
  })

  it('deletes it', async () => {
    await deleteModel(createdId)
    const page = await listRegisteredModels()
    expect(page.items.find((m) => m.id === createdId)).toBeUndefined()
    createdId = ''
  })
})
