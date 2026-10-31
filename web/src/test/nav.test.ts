import { describe, expect, it } from 'vitest'
import { findNavItem, mergeNavGroups, type NavGroup, type NavItem } from '@/lib/nav'

describe('findNavItem', () => {
  it('finds a page by its exact path', () => {
    expect(findNavItem('/token-monitoring')?.label).toBe('Token Monitoring')
    expect(findNavItem('/')?.label).toBe('Overview')
  })

  it("finds a sub-page's section, so the top bar names it instead of Overview", () => {
    expect(findNavItem('/token-monitoring/model')?.label).toBe('Token Monitoring')
    expect(findNavItem('/token-monitoring/keys/abc')?.label).toBe('Token Monitoring')
    expect(findNavItem('/profiles/p1/tools')?.label).toBe('Profiles')
  })

  it('matches whole path segments only', () => {
    expect(findNavItem('/token-monitoring-x')).toBeUndefined()
    expect(findNavItem('/nowhere')).toBeUndefined()
  })
})

describe('mergeNavGroups', () => {
  const Icon = (() => null) as unknown as NavItem['icon']
  const item = (path: string): NavItem => ({ label: path, path, icon: Icon, description: '' })
  const core: NavGroup[] = [
    { label: 'A', items: [item('/a')] },
    { label: 'B', items: [item('/b')] },
  ]

  it('adds items to a group of the same label and inserts new groups before the named one', () => {
    const out = mergeNavGroups(core, [
      { label: 'B', items: [item('/b2')] },
      { label: 'X', items: [item('/x')], before: 'B' },
      { label: 'Y', items: [item('/y')] },
    ])
    expect(out.map((g) => g.label)).toEqual(['A', 'X', 'B', 'Y'])
    expect(out[2]!.items.map((i) => i.path)).toEqual(['/b', '/b2'])
    expect(core[1]!.items).toHaveLength(1) // core is not mutated
  })

  it('is the core nav when there is nothing to add', () => {
    expect(mergeNavGroups(core)).toEqual(core)
  })
})
