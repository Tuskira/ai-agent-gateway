import { describe, expect, it } from 'vitest'
import {
  LIFECYCLE_STATES,
  catalogEntryState,
  deriveRegistryState,
  lifecycleLabel,
  lifecycleOutline,
  lifecycleTitle,
  lifecycleTone,
  matchCatalogEntry,
  mergeMcpRows,
  mergeSkillRows,
  type LifecycleState,
} from '@/lib/lifecycle'
import type { Connector } from '@/lib/connectors'
import type { DiscoveredMcp, DiscoveredSkill } from '@/lib/discovery'
import type { CatalogEntry } from '@/lib/mcpCatalog'
import type { Skill } from '@/lib/skills'

function skill(over: Partial<Skill> & { name: string }): Skill {
  return {
    id: `id-${over.name}`,
    kind: 'skill',
    description: '',
    frontmatter: {},
    arguments: [],
    latest_version: 1,
    enabled: true,
    metadata: {},
    scope: 'tenant',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...over,
  }
}

function usage(over: Partial<DiscoveredSkill> & { name: string }): DiscoveredSkill {
  return {
    calls: 1,
    used_by: 1,
    last_seen: '2026-02-01T00:00:00Z',
    registered: false,
    kind: '',
    ...over,
  }
}

function connector(over: Partial<Connector> & { slug: string }): Connector {
  return {
    id: `c-${over.slug}`,
    name: over.slug,
    endpoint: `https://${over.slug}.example/mcp`,
    timeout_ms: 30000,
    status: 'healthy',
    metadata: {},
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...over,
  }
}

function mcp(over: Partial<DiscoveredMcp> & { server: string }): DiscoveredMcp {
  return {
    tools: ['t'],
    calls: 1,
    used_by: 1,
    last_seen: '2026-02-01T00:00:00Z',
    registered_connector_slug: '',
    via_gateway: false,
    ...over,
  }
}

function entry(over: Partial<CatalogEntry> & { slug: string }): CatalogEntry {
  return {
    id: `cat-${over.slug}`,
    name: over.slug,
    description: '',
    icon: '',
    category: '',
    url: 'https://x.example/mcp',
    url_overridable: false,
    transport: 'streamable-http',
    auth: { kind: 'none', fields: [] },
    suggested_tools: [],
    enabled: true,
    scope: 'platform',
    supported: true,
    added: false,
    ...over,
  }
}

describe('lifecycle labels', () => {
  it('has a label, tone, hover title and outline flag for every state', () => {
    const table: Record<LifecycleState, [string, string, boolean]> = {
      available: ['Available', 'accent', true],
      registered: ['Registered', 'info', false],
      active: ['Active', 'positive', false],
      disabled: ['Disabled', 'neutral', false],
      discovered: ['Discovered', 'neutral', true],
    }
    expect(LIFECYCLE_STATES.slice().sort()).toEqual(Object.keys(table).sort())
    for (const s of LIFECYCLE_STATES) {
      const [label, tone, outline] = table[s]
      expect(lifecycleLabel(s)).toBe(label)
      expect(lifecycleTone(s)).toBe(tone)
      expect(lifecycleOutline(s)).toBe(outline)
      expect(lifecycleTitle(s).length).toBeGreaterThan(10)
    }
  })
})

describe('deriveRegistryState', () => {
  it.each([
    [true, 0, 'registered'],
    [true, undefined, 'registered'],
    [true, 3, 'active'],
    [false, 0, 'disabled'],
    // Disabled wins over traffic (the Models page gets this wrong).
    [false, 50, 'disabled'],
  ] as const)('enabled=%s calls=%s -> %s', (enabled, calls, want) => {
    expect(deriveRegistryState(enabled, calls)).toBe(want)
  })
})

describe('mergeSkillRows', () => {
  it('derives registered / active / disabled / discovered', () => {
    const rows = mergeSkillRows(
      [
        skill({ name: 'review-pr' }),
        skill({ name: 'quiet' }),
        skill({ name: 'old-skill', enabled: false }),
        skill({ name: 'platform-skill', scope: 'platform' }),
      ],
      [
        usage({ name: 'review-pr', registered: true, calls: 5 }),
        usage({ name: 'old-skill', registered: true, calls: 9 }),
        usage({ name: 'platform-skill', registered: true, calls: 2 }),
        usage({ name: 'code-audit', calls: 7, used_by: 2 }),
      ],
      null,
    )
    const by = Object.fromEntries(rows.map((r) => [r.name, r]))
    expect(by['review-pr']!.state).toBe('active')
    expect(by['quiet']!.state).toBe('registered')
    expect(by['old-skill']!.state).toBe('disabled') // traffic does not revive it
    expect(by['old-skill']!.calls).toBe(9)
    expect(by['platform-skill']!.state).toBe('active')
    expect(by['code-audit']).toMatchObject({
      state: 'discovered',
      skill: null,
      calls: 7,
      used_by: 2,
    })
  })

  it('never double-lists a name the server already reports as registered', () => {
    const rows = mergeSkillRows([], [usage({ name: 'x', registered: true })], null)
    expect(rows).toEqual([])
  })

  it('uses one source per row: discovery wins, gateway-served usage is the fallback', () => {
    const rows = mergeSkillRows(
      [skill({ name: 'a' }), skill({ name: 'cmd', kind: 'command' })],
      [usage({ name: 'a', registered: true, calls: 4 })],
      [
        { name: 'a', kind: 'skill', calls: 100, used_by: 9, last_seen: '2026-02-02T00:00:00Z' },
        { name: 'cmd', kind: 'command', calls: 3, used_by: 1, last_seen: '2026-02-02T00:00:00Z' },
      ],
    )
    expect(rows.find((r) => r.name === 'a')!.calls).toBe(4) // not 104
    const cmd = rows.find((r) => r.name === 'cmd')!
    expect(cmd.calls).toBe(3)
    expect(cmd.state).toBe('active')
  })

  it('analytics off: Registered/Disabled only, no Discovered rows', () => {
    const rows = mergeSkillRows(
      [skill({ name: 'a' }), skill({ name: 'b', enabled: false })],
      null,
      null,
    )
    expect(rows.map((r) => r.state)).toEqual(['registered', 'disabled'])
    expect(rows.every((r) => r.calls === 0 && r.last_seen === null)).toBe(true)
  })
})

describe('mergeMcpRows', () => {
  const catalog = [
    entry({ slug: 'context7', name: 'Context7' }),
    entry({ slug: 'deepwiki', name: 'DeepWiki', added: true }),
  ]

  it('derives Active / Registered / Disabled for connectors and attributes via_gateway', () => {
    const rows = mergeMcpRows(
      [
        connector({ slug: 'deepwiki' }),
        connector({ slug: 'drawio' }),
        connector({ slug: 'idle' }),
        connector({ slug: 'off', metadata: { enabled: false } }),
      ],
      [
        mcp({ server: 'deepwiki', registered_connector_slug: 'deepwiki', calls: 2 }),
        mcp({
          server: 'drawio',
          registered_connector_slug: 'drawio',
          via_gateway: true,
          calls: 1,
        }),
        mcp({ server: 'off', registered_connector_slug: 'off', calls: 8 }),
      ],
      catalog,
    )
    const by = Object.fromEntries(rows.map((r) => [r.connector?.slug ?? r.name, r]))
    expect(by['deepwiki']!.state).toBe('active')
    expect(by['drawio']!.state).toBe('active') // through the gateway plane
    expect(by['idle']!.state).toBe('registered')
    expect(by['off']!.state).toBe('disabled') // disabled beats traffic
  })

  it('adds direct and via-gateway calls for one connector', () => {
    const rows = mergeMcpRows(
      [connector({ slug: 'lf' })],
      [
        mcp({ server: 'lf', registered_connector_slug: 'lf', calls: 2, used_by: 1 }),
        mcp({ server: 'lf', registered_connector_slug: 'lf', via_gateway: true, calls: 3, used_by: 2 }),
      ],
      [],
    )
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({ state: 'active', calls: 5, used_by: 2 })
  })

  it('lists an unregistered server as Discovered, matching a catalog entry when one exists', () => {
    const rows = mergeMcpRows(
      [],
      [
        mcp({ server: 'context7', calls: 4 }),
        mcp({ server: 'homegrown', calls: 1 }),
        // DeepWiki is already added: no catalog match, so plain "Add MCP".
        mcp({ server: 'deepwiki' }),
      ],
      catalog,
    )
    const by = Object.fromEntries(rows.map((r) => [r.name, r]))
    expect(by['context7']).toMatchObject({ state: 'discovered', connector: null })
    expect(by['context7']!.catalogMatch?.slug).toBe('context7')
    expect(by['homegrown']!.catalogMatch).toBeNull()
    expect(by['deepwiki']!.catalogMatch).toBeNull()
  })

  it('analytics off: connectors are Registered/Disabled, nothing Discovered', () => {
    const rows = mergeMcpRows(
      [connector({ slug: 'a' }), connector({ slug: 'b', metadata: { enabled: false } })],
      null,
      catalog,
    )
    expect(rows.map((r) => r.state)).toEqual(['registered', 'disabled'])
  })
})

describe('matchCatalogEntry', () => {
  it('matches by slug or name ignoring case and punctuation, and skips unaddable entries', () => {
    const entries = [
      entry({ slug: 'google-drive', name: 'Google Drive', supported: false }),
      entry({ slug: 'draw-io', name: 'Draw.io' }),
      entry({ slug: 'off', name: 'Off', enabled: false }),
    ]
    expect(matchCatalogEntry('DrawIO', entries)?.slug).toBe('draw-io')
    expect(matchCatalogEntry('google_drive', entries)).toBeNull() // OAuth: not addable
    expect(matchCatalogEntry('off', entries)).toBeNull()
    expect(matchCatalogEntry('', entries)).toBeNull()
  })
})

describe('catalogEntryState', () => {
  it('is Available until added, then the connector state', () => {
    const by = new Map<string, LifecycleState>([['c1', 'active']])
    expect(catalogEntryState({ added: false }, by)).toBe('available')
    expect(catalogEntryState({ added: true, connector_id: 'c1' }, by)).toBe('active')
    expect(catalogEntryState({ added: true, connector_id: 'zz' }, by)).toBe('registered')
    expect(catalogEntryState({ added: true }, undefined)).toBe('registered')
  })
})
