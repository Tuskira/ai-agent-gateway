import { afterEach, describe, expect, it, vi } from 'vitest'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import { formatCompactNumber } from '@/lib/overview'
import {
  fetchTokenMonitoring,
  fetchTokenMonitoringKey,
  fetchTokenMonitoringModel,
  formatShare,
  formatTokenCost,
  callerLabel,
  tokenSplit,
} from '@/lib/token-monitoring'

describe('formatTokenCost', () => {
  it('shows small costs instead of rounding them to zero', () => {
    expect(formatTokenCost(0.0000097)).toBe('$0.0000097')
    expect(formatTokenCost(0.0042)).toBe('$0.0042')
    expect(formatTokenCost(0.00000012)).toBe('$0.00000012')
  })
  it('uses cents from one cent up', () => {
    expect(formatTokenCost(0.01)).toBe('$0.01')
    expect(formatTokenCost(4.1234)).toBe('$4.12')
    expect(formatTokenCost(1234.5)).toBe('$1,234.50')
  })
  it('keeps a real zero and an unknown cost apart', () => {
    expect(formatTokenCost(0)).toBe('$0.00')
    expect(formatTokenCost(null)).toBe('—')
  })
})

describe('formatShare', () => {
  it('never shows a non-zero share as 0%', () => {
    expect(formatShare(12, 339012)).toBe('<0.1%')
    expect(formatShare(0, 100)).toBe('0%')
  })
  it('rounds normal shares to one decimal', () => {
    expect(formatShare(300000, 339012)).toBe('88.5%')
    expect(formatShare(5, 5)).toBe('100%')
  })
  it('has no share of nothing', () => {
    expect(formatShare(0, 0)).toBe('—')
  })
})

describe('formatCompactNumber (token counts)', () => {
  it('keeps small counts exact and compacts large ones', () => {
    expect(formatCompactNumber(0)).toBe('0')
    expect(formatCompactNumber(42)).toBe('42')
    expect(formatCompactNumber(37914969)).toBe('37.91M')
  })
})

describe('callerLabel', () => {
  it('names a caller by its key, else says why it has none', () => {
    expect(callerLabel({ name: 'ci-bot', key_id: 'k1' })).toBe('ci-bot')
    expect(callerLabel({ name: '', key_id: 'k1' })).toBe('Unknown key')
    expect(callerLabel({ name: '', key_id: '' })).toBe('No key')
  })
})

describe('tokenSplit', () => {
  it('splits input, output, cache read and cache write by their share of all four', () => {
    const s = tokenSplit({
      prompt_tokens: 200,
      completion_tokens: 100,
      cache_read_tokens: 600,
      cache_write_tokens: 100,
    })
    expect(s.map((p) => [p.key, p.label])).toEqual([
      ['in', '20%'],
      ['out', '10%'],
      ['cr', '60%'],
      ['cw', '10%'],
    ])
  })
  it('marks a tiny but real part as <0.1%', () => {
    const s = tokenSplit({
      prompt_tokens: 1,
      completion_tokens: 0,
      cache_read_tokens: 99999,
      cache_write_tokens: 0,
    })
    expect(s[0]?.label).toBe('<0.1%')
    expect(s[1]?.label).toBe('0%')
  })
})

describe('fetchers', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    sessionStorage.clear()
  })

  function stub(status: number, body: unknown) {
    sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
    const fn = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { 'Content-Type': 'application/json' },
        }),
    )
    vi.stubGlobal('fetch', fn)
    return fn
  }

  it('asks for the selected window', async () => {
    const fn = stub(200, { window: '30d' })
    await fetchTokenMonitoring('30d')
    expect(String(fn.mock.calls[0]?.[0])).toMatch(
      /\/api\/v1\/analytics\/token-monitoring\?window=30d$/,
    )
  })

  it('reads 404 (no ClickHouse) as analytics off', async () => {
    stub(404, {
      error: { type: 'not_found', message: 'analytics requires the ClickHouse sink' },
    })
    await expect(fetchTokenMonitoring('today')).resolves.toBeNull()
  })

  it('encodes model names and pages sessions', async () => {
    const fn = stub(200, {})
    await fetchTokenMonitoringModel('bedrock/us.anthropic.claude:0', '7d', 25, 50)
    expect(String(fn.mock.calls[0]?.[0])).toMatch(
      /\/analytics\/token-monitoring\/model\?model=bedrock%2Fus\.anthropic\.claude%3A0&window=7d&limit=25&offset=50$/,
    )
    await fetchTokenMonitoringKey('key/1', 'today', 10, 0)
    expect(String(fn.mock.calls[1]?.[0])).toMatch(
      /\/analytics\/token-monitoring\/keys\/key%2F1\?window=today&limit=10&offset=0$/,
    )
  })

  it('surfaces other errors', async () => {
    stub(500, { error: { type: 'internal_error', message: 'boom' } })
    await expect(fetchTokenMonitoring('7d')).rejects.toThrow()
  })
})
