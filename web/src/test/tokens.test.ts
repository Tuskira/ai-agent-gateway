import { readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { colors, fonts, radii, shadows } from '@/styles/tokens'

// tokens.css and tokens.ts are both generated from tokens.json by
// scripts/gen-tokens.mjs, so they should never drift — but this test
// verifies that invariant directly against the generated CSS text, so a
// hand-edit of either file (bypassing the generator) still gets caught.

const tokensCssPath = path.resolve(process.cwd(), 'src/styles/tokens.css')
const tokensCss = readFileSync(tokensCssPath, 'utf8')

function kebab(key: string): string {
  if (/^d\d+$/.test(key)) return key.replace(/^d(\d+)$/, 'd-$1')
  return key.replace(/([a-z0-9])([A-Z])/g, '$1-$2').toLowerCase()
}

function expectCssVar(name: string, value: string) {
  const escaped = value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const pattern = new RegExp(`--${name}:\\s*${escaped};`)
  expect(tokensCss, `expected tokens.css to declare --${name}: ${value};`).toMatch(
    pattern,
  )
}

describe('design tokens: tokens.ts stays in sync with tokens.css', () => {
  it('every colors.indigo / colors.slate entry exists in tokens.css', () => {
    for (const [key, value] of Object.entries(colors.indigo)) {
      expectCssVar(`indigo-${kebab(key)}`, value)
    }
    for (const [key, value] of Object.entries(colors.slate)) {
      expectCssVar(`slate-${kebab(key)}`, value)
    }
  })

  it('every colors.sev / colors.status / colors.brand entry exists in tokens.css', () => {
    for (const [key, value] of Object.entries(colors.sev)) {
      expectCssVar(`sev-${kebab(key)}`, value)
    }
    for (const [key, value] of Object.entries(colors.status)) {
      expectCssVar(`status-${kebab(key)}`, value)
    }
    for (const [key, value] of Object.entries(colors.brand)) {
      expectCssVar(`brand-${kebab(key)}`, value)
    }
  })

  it('every radii entry exists in tokens.css as --r-*', () => {
    for (const [key, value] of Object.entries(radii)) {
      expectCssVar(`r-${key}`, value)
    }
  })

  it('every fonts entry exists in tokens.css as --font-*', () => {
    for (const [key, value] of Object.entries(fonts)) {
      expectCssVar(`font-${key}`, value)
    }
  })

  it('every shadows entry exists in tokens.css as the light --shadow-* value', () => {
    for (const [key, value] of Object.entries(shadows)) {
      expectCssVar(`shadow-${key}`, value)
    }
  })
})
