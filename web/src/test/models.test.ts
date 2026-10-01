import { describe, expect, it } from 'vitest'
import { mergeModelRows, type ModelUsageRow, type RegisteredModel } from '@/lib/models'
import {
  apiKeyCredentialName,
  applyVendorChoice,
  newTargetRow,
  resolveProviderPresets,
  rowsToTargets,
  targetRowError,
  targetsToRows,
  vendorChoice,
  vendorChoicesFor,
  type TargetRowState,
} from '@/components/app/models/targets'

const NEBIUS_URL = 'https://api.tokenfactory.eu-west2.nebius.com/v1/'
const TOGETHER_URL = 'https://api.together.ai/v1'

function registeredModel(overrides: Partial<RegisteredModel> = {}): RegisteredModel {
  return {
    id: 'm1',
    name: 'claude-sonnet',
    description: '',
    enabled: true,
    targets: [{ vendor: 'anthropic', model: 'claude-sonnet-4-5-20250929' }],
    price: null,
    limits: null,
    scope: 'tenant',
    metadata: {},
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

function usageRow(overrides: Partial<ModelUsageRow> = {}): ModelUsageRow {
  return {
    name: 'claude-sonnet',
    provider: 'anthropic',
    calls: 10,
    tokens: 1000,
    cost_usd: 1.5,
    used_by: 2,
    last_seen: '2026-01-02T00:00:00Z',
    status: 'active',
    ...overrides,
  }
}

describe('mergeModelRows', () => {
  it('merges a registered model with matching traffic into one "active" row', () => {
    const rows = mergeModelRows([registeredModel()], [usageRow()])
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      key: 'm1',
      name: 'claude-sonnet',
      status: 'active',
      calls: 10,
      tokens: 1000,
      costUsd: 1.5,
      usedBy: 2,
    })
    expect(rows[0]!.registered?.id).toBe('m1')
  })

  it('shows a registered-but-unseen model as "registered" with zeroed traffic', () => {
    const rows = mergeModelRows([registeredModel()], [])
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      status: 'registered',
      calls: 0,
      tokens: 0,
      costUsd: null,
      usedBy: 0,
      lastSeen: null,
    })
  })

  it('shows a disabled, unseen registered model as "disabled"', () => {
    const rows = mergeModelRows([registeredModel({ enabled: false })], [])
    expect(rows[0]!.status).toBe('disabled')
  })

  it('keeps traffic for a model nobody registered as its own "discovered" row with no registry record', () => {
    const rows = mergeModelRows(
      [],
      [usageRow({ name: 'gpt-4o-mini', provider: 'openai_compat' })],
    )
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      key: 'gpt-4o-mini',
      name: 'gpt-4o-mini',
      status: 'discovered',
    })
    expect(rows[0]!.registered).toBeNull()
  })

  it('falls back to the first target vendor as provider when unseen', () => {
    const rows = mergeModelRows(
      [
        registeredModel({
          targets: [
            { vendor: 'bedrock', model: 'anthropic.claude', region: 'us-east-1' },
          ],
        }),
      ],
      [],
    )
    expect(rows[0]!.provider).toBe('bedrock')
  })

  it('sorts by tokens descending, then name', () => {
    const rows = mergeModelRows(
      [],
      [
        usageRow({ name: 'low', tokens: 10 }),
        usageRow({ name: 'high', tokens: 1000 }),
        usageRow({ name: 'mid', tokens: 100 }),
      ],
    )
    expect(rows.map((r) => r.name)).toEqual(['high', 'mid', 'low'])
  })

  it('handles no registry rows and no traffic rows', () => {
    expect(mergeModelRows([], [])).toEqual([])
  })
})

describe('target row helpers', () => {
  it('newTargetRow defaults to anthropic with empty fields', () => {
    const row = newTargetRow()
    expect(row.vendor).toBe('anthropic')
    expect(row.model).toBe('')
    expect(row.id).toBeTruthy()
  })

  it('targetsToRows falls back to a single blank row for an empty/missing target list', () => {
    expect(targetsToRows(undefined)).toHaveLength(1)
    expect(targetsToRows([])).toHaveLength(1)
  })

  it('targetsToRows/rowsToTargets round-trip a full target', () => {
    const rows = targetsToRows([
      {
        vendor: 'openai_compat',
        model: 'gpt-4o-mini',
        base_url: 'https://api.example.com/v1',
        credential: 'openai-key',
      },
    ])
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      vendor: 'openai_compat',
      model: 'gpt-4o-mini',
      baseUrl: 'https://api.example.com/v1',
      credential: 'openai-key',
    })
    expect(rowsToTargets(rows)).toEqual([
      {
        vendor: 'openai_compat',
        model: 'gpt-4o-mini',
        base_url: 'https://api.example.com/v1',
        credential: 'openai-key',
      },
    ])
  })

  it('rowsToTargets drops rows with no model id', () => {
    const rows: TargetRowState[] = [
      newTargetRow(),
      { ...newTargetRow(), model: 'claude' },
    ]
    expect(rowsToTargets(rows)).toEqual([{ vendor: 'anthropic', model: 'claude' }])
  })

  it('rowsToTargets preserves order (the fallback order)', () => {
    const rows: TargetRowState[] = [
      { ...newTargetRow(), model: 'first' },
      { ...newTargetRow(), model: 'second' },
    ]
    expect(rowsToTargets(rows).map((t) => t.model)).toEqual(['first', 'second'])
  })

  it('rowsToTargets sends only what the API accepts for the vendor', () => {
    const typed = {
      ...newTargetRow(),
      model: 'm',
      baseUrl: 'https://llm.internal/v1',
      region: 'us-east-1',
    }
    // The API refuses a region on anything but Bedrock, and a base_url on Bedrock.
    expect(rowsToTargets([{ ...typed, vendor: 'anthropic' }])).toEqual([
      { vendor: 'anthropic', model: 'm', base_url: 'https://llm.internal/v1' },
    ])
    expect(rowsToTargets([{ ...typed, vendor: 'bedrock' }])).toEqual([
      { vendor: 'bedrock', model: 'm', region: 'us-east-1' },
    ])
  })

  it('rowsToTargets sends allow_caller_key only when set and without a credential', () => {
    const row = { ...newTargetRow(), model: 'm' }
    expect(rowsToTargets([row])).toEqual([{ vendor: 'anthropic', model: 'm' }])
    expect(rowsToTargets([{ ...row, allowCallerKey: true }])).toEqual([
      { vendor: 'anthropic', model: 'm', allow_caller_key: true },
    ])
    // The API answers 400 to the combination; the credential wins.
    expect(rowsToTargets([{ ...row, allowCallerKey: true, credential: 'c' }])).toEqual([
      { vendor: 'anthropic', model: 'm', credential: 'c' },
    ])
  })

  it('rowsToTargets sends a label on an OpenAI-compatible target only', () => {
    const row = {
      ...newTargetRow(),
      model: 'm',
      baseUrl: 'https://api.groq.com/openai/v1',
      label: ' groq ',
    }
    expect(rowsToTargets([{ ...row, vendor: 'openai_compat' }])).toEqual([
      {
        vendor: 'openai_compat',
        model: 'm',
        base_url: 'https://api.groq.com/openai/v1',
        label: 'groq',
      },
    ])
    // The API refuses a label on any other vendor.
    expect(rowsToTargets([{ ...row, vendor: 'anthropic' }])).toEqual([
      { vendor: 'anthropic', model: 'm', base_url: 'https://api.groq.com/openai/v1' },
    ])
    expect(
      targetsToRows([{ vendor: 'openai_compat', model: 'm', label: 'groq' }])[0]!.label,
    ).toBe('groq')
  })

  it("targetRowError applies the registry's label rules", () => {
    const row: TargetRowState = {
      ...newTargetRow(),
      vendor: 'openai_compat',
      model: 'm',
      baseUrl: 'https://x.test/v1',
    }
    for (const label of ['groq', 'openai', 'z.ai', 'my_vendor-2', 'a'.repeat(32)]) {
      expect(targetRowError({ ...row, label })).toBeNull()
    }
    expect(targetRowError({ ...row, label: 'Groq' })).toMatch(/label/i)
    expect(targetRowError({ ...row, label: 'my vendor' })).toMatch(/label/i)
    expect(targetRowError({ ...row, label: 'a'.repeat(33) })).toMatch(/label/i)
    expect(targetRowError({ ...row, label: 'anthropic' })).toMatch(/reserved/i)
    expect(targetRowError({ ...row, label: 'bedrock' })).toMatch(/reserved/i)
    // Not checked (and not sent) on a vendor that takes no label.
    expect(targetRowError({ ...row, vendor: 'gemini', label: 'Not A Label' })).toBeNull()
  })

  it("a registered, unseen model shows its first target's label as provider", () => {
    const rows = mergeModelRows(
      [
        registeredModel({
          targets: [
            { vendor: 'openai_compat', model: 'm', base_url: 'https://x', label: 'groq' },
          ],
        }),
      ],
      [],
    )
    expect(rows[0]!.provider).toBe('groq')
  })

  it('targetsToRows reads allow_caller_key (omitted by the API when false)', () => {
    const rows = targetsToRows([
      { vendor: 'anthropic', model: 'a', allow_caller_key: true },
      { vendor: 'anthropic', model: 'b' },
    ])
    expect(rows.map((r) => r.allowCallerKey)).toEqual([true, false])
  })

  it("targetRowError applies the registry's base_url rules", () => {
    const row: TargetRowState = { ...newTargetRow(), vendor: 'openai_compat', model: 'm' }
    expect(targetRowError({ ...row, baseUrl: 'http://127.0.0.1:11434/v1' })).toBeNull()
    expect(targetRowError({ ...row, baseUrl: 'http://localhost:11434/v1' })).toBeNull()
    expect(
      targetRowError({ ...row, baseUrl: 'http://host.docker.internal:11434/v1' }),
    ).toBeNull()
    expect(
      targetRowError({ ...row, baseUrl: 'http://host.docker.internal.example.com/v1' }),
    ).toMatch(/https/i)
    expect(targetRowError({ ...row, baseUrl: 'http://llm.internal/v1' })).toMatch(
      /https/i,
    )
    expect(targetRowError({ ...row, baseUrl: 'llm.internal/v1' })).toMatch(/absolute/i)
    expect(targetRowError({ ...row, baseUrl: 'https://llm.internal/v1?key=x' })).toMatch(
      /query/i,
    )
    // Not checked on a vendor that takes no base_url.
    expect(
      targetRowError({
        ...row,
        vendor: 'bedrock',
        region: 'us-east-1',
        baseUrl: 'nonsense',
      }),
    ).toBeNull()
  })

  it('targetRowError requires a model id', () => {
    expect(targetRowError(newTargetRow())).toMatch(/model id/i)
  })

  it('targetRowError requires base_url for openai_compat', () => {
    const row: TargetRowState = {
      ...newTargetRow(),
      vendor: 'openai_compat',
      model: 'gpt-4o',
    }
    expect(targetRowError(row)).toMatch(/base url/i)
    expect(targetRowError({ ...row, baseUrl: 'https://api.example.com' })).toBeNull()
  })

  it('targetRowError requires region for bedrock', () => {
    const row: TargetRowState = {
      ...newTargetRow(),
      vendor: 'bedrock',
      model: 'anthropic.claude',
    }
    expect(targetRowError(row)).toMatch(/region/i)
    expect(targetRowError({ ...row, region: 'us-east-1' })).toBeNull()
  })
})

describe('provider choices and the typed API key', () => {
  const kimi = { vendor: 'openai_compat' as const, model: 'moonshotai/Kimi-K3' }

  it('vendorChoice names a saved row by its label: Nebius, Together AI, else Others', () => {
    expect(
      vendorChoice(
        targetsToRows([{ ...kimi, base_url: NEBIUS_URL, label: 'nebius' }])[0]!,
      ),
    ).toBe('nebius')
    expect(
      vendorChoice(
        targetsToRows([{ ...kimi, base_url: TOGETHER_URL, label: 'together' }])[0]!,
      ),
    ).toBe('together')
    expect(
      vendorChoice(
        targetsToRows([{ ...kimi, base_url: 'https://x.test/v1', label: 'ollama' }])[0]!,
      ),
    ).toBe('openai_compat')
    expect(
      vendorChoice(targetsToRows([{ ...kimi, base_url: 'https://x.test/v1' }])[0]!),
    ).toBe('openai_compat')
    expect(vendorChoice(newTargetRow())).toBe('anthropic')
    expect(vendorChoice({ ...newTargetRow(), vendor: 'bedrock', label: 'nebius' })).toBe(
      'bedrock',
    )
  })

  it('a provider is an OpenAI-compatible target with its label and default base URL', () => {
    const nebius = applyVendorChoice(
      { ...newTargetRow(), model: 'moonshotai/Kimi-K3' },
      'nebius',
    )
    expect(nebius).toMatchObject({
      vendor: 'openai_compat',
      label: 'nebius',
      baseUrl: NEBIUS_URL,
    })
    expect(targetRowError(nebius)).toBeNull()
    expect(rowsToTargets([nebius])).toEqual([
      { ...kimi, base_url: NEBIUS_URL, label: 'nebius' },
    ])
    expect(applyVendorChoice(newTargetRow(), 'together')).toMatchObject({
      vendor: 'openai_compat',
      label: 'together',
      baseUrl: TOGETHER_URL,
    })
  })

  it('picking a vendor fills its link: Anthropic, Google Gemini, OpenAI; Others and Bedrock none', () => {
    expect(applyVendorChoice(newTargetRow(), 'anthropic')).toMatchObject({
      vendor: 'anthropic',
      baseUrl: 'https://api.anthropic.com',
    })
    expect(applyVendorChoice(newTargetRow(), 'openai')).toMatchObject({
      vendor: 'openai_compat',
      label: 'openai',
      baseUrl: 'https://api.openai.com/v1',
    })
    // Google Gemini is Google's OpenAI-compatible endpoint: what Anthropic
    // clients (Claude Code, the Anthropic SDK) can be translated to.
    expect(applyVendorChoice(newTargetRow(), 'google_gemini')).toMatchObject({
      vendor: 'openai_compat',
      label: 'gemini',
      baseUrl: 'https://generativelanguage.googleapis.com/v1beta/openai/',
    })
    const nebius = applyVendorChoice(newTargetRow(), 'nebius')
    expect(applyVendorChoice(nebius, 'openai_compat')).toMatchObject({
      vendor: 'openai_compat',
      label: '',
      baseUrl: '',
    })
    expect(applyVendorChoice(nebius, 'bedrock')).toMatchObject({
      vendor: 'bedrock',
      baseUrl: '',
    })
    expect(applyVendorChoice(nebius, 'anthropic')).toMatchObject({
      vendor: 'anthropic',
      label: '',
      baseUrl: 'https://api.anthropic.com',
    })
    // Others keeps a label the user typed that names no provider.
    const groq = { ...newTargetRow(), vendor: 'openai_compat' as const, label: 'groq' }
    expect(applyVendorChoice(groq, 'openai_compat').label).toBe('groq')
  })

  it('names saved rows by vendor: OpenAI and Google Gemini by label, native Gemini kept', () => {
    const row = { ...newTargetRow(), vendor: 'openai_compat' as const }
    expect(vendorChoice({ ...row, label: 'openai' })).toBe('openai')
    expect(vendorChoice({ ...row, label: 'gemini' })).toBe('google_gemini')
    const native = { ...newTargetRow(), vendor: 'gemini' as const }
    expect(vendorChoice(native)).toBe('gemini')
    // The dropdown lists the native API only for a row already on it.
    expect(vendorChoicesFor(row).map((c) => c.label)).toEqual([
      'Anthropic',
      'Amazon Bedrock',
      'Google Gemini',
      'OpenAI',
      'Nebius',
      'Together AI',
      'Others',
    ])
    expect(vendorChoicesFor(native).map((c) => c.value)).toContain('gemini')
  })

  it('a typed key is cleared on any vendor change, and Bedrock takes none', () => {
    const typed = {
      ...applyVendorChoice(newTargetRow(), 'nebius'),
      apiKey: 'not-a-real-key',
    }
    expect(applyVendorChoice(typed, 'together').apiKey).toBe('')
    expect(applyVendorChoice(typed, 'bedrock').apiKey).toBeUndefined()
    expect(applyVendorChoice(newTargetRow(), 'nebius').apiKey).toBeUndefined()
  })

  it('a typed key is required once chosen, and never sent as a target field', () => {
    const row = {
      ...applyVendorChoice({ ...newTargetRow(), model: 'm' }, 'nebius'),
      apiKey: '',
    }
    expect(targetRowError(row)).toMatch(/api key/i)
    expect(targetRowError({ ...row, apiKey: '  ' })).toMatch(/api key/i)
    expect(targetRowError({ ...row, apiKey: 'not-a-real-key' })).toBeNull()
    expect(rowsToTargets([{ ...row, apiKey: 'not-a-real-key' }])[0]).not.toHaveProperty(
      'apiKey',
    )
  })

  it('apiKeyCredentialName is one per provider, not one per model', () => {
    expect(apiKeyCredentialName(applyVendorChoice(newTargetRow(), 'nebius'))).toBe(
      'nebius-api-key',
    )
    // Two different models on the same provider name the same credential.
    expect(
      apiKeyCredentialName(
        applyVendorChoice({ ...newTargetRow(), model: 'a' }, 'nebius'),
      ),
    ).toBe(
      apiKeyCredentialName(applyVendorChoice({ ...newTargetRow(), model: 'b' }, 'nebius')),
    )
    // No label (a plain native vendor) falls back to the vendor name.
    expect(apiKeyCredentialName(newTargetRow())).toBe('anthropic-api-key')
  })

  it('picking a provider whose credential is already on file preselects it', () => {
    const creds = [{ name: 'nebius-api-key' }]
    expect(applyVendorChoice(newTargetRow(), 'nebius', creds)).toMatchObject({
      credential: 'nebius-api-key',
    })
    // No matching credential yet -- nothing preselected.
    expect(applyVendorChoice(newTargetRow(), 'together', creds)).toMatchObject({
      credential: '',
    })
  })

  it("switching from one provider's on-file credential to another's repoints it, but leaves a hand-picked credential alone", () => {
    const creds = [{ name: 'nebius-api-key' }, { name: 'together-api-key' }]
    const nebius = applyVendorChoice(newTargetRow(), 'nebius', creds)
    expect(nebius.credential).toBe('nebius-api-key')
    // Auto-picked slot -> auto-picked slot: follows the vendor.
    expect(applyVendorChoice(nebius, 'together', creds).credential).toBe('together-api-key')

    const handPicked = { ...nebius, credential: 'some-other-cred' }
    // A credential the user chose by hand survives a vendor switch.
    expect(applyVendorChoice(handPicked, 'together', creds).credential).toBe(
      'some-other-cred',
    )
  })

  it('resolveProviderPresets falls back to the static list when the catalog is empty or absent', () => {
    expect(resolveProviderPresets(undefined).map((p) => p.value)).toEqual([
      'google_gemini',
      'openai',
      'nebius',
      'together',
    ])
    expect(resolveProviderPresets([]).map((p) => p.value)).toEqual([
      'google_gemini',
      'openai',
      'nebius',
      'together',
    ])
    expect(
      resolveProviderPresets([
        { slug: 'nebius', display_name: 'Nebius', base_url: 'https://x/v1', enabled: false },
      ]).map((p) => p.value),
    ).toEqual(['google_gemini', 'openai', 'nebius', 'together'])
  })

  it('resolveProviderPresets uses the catalog, base URL included, once it has enabled providers', () => {
    const presets = resolveProviderPresets([
      {
        slug: 'nebius',
        display_name: 'Nebius AI Studio',
        base_url: 'https://api.tokenfactory.eu-west2.nebius.com/v1',
        enabled: true,
      },
      { slug: 'disabled-one', display_name: 'Off', base_url: 'https://x/v1', enabled: false },
    ])
    expect(presets).toEqual([
      {
        value: 'nebius',
        label: 'Nebius AI Studio',
        tag: 'nebius',
        baseUrl: 'https://api.tokenfactory.eu-west2.nebius.com/v1',
      },
    ])
  })
})
