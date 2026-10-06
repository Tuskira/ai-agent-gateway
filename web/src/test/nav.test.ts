import { describe, expect, it } from 'vitest'
import { findNavItem } from '@/lib/nav'

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
