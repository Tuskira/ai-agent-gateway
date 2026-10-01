import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ThemeToggle } from '@/components/layout/ThemeToggle'
import { setThemePreference } from '@/hooks/use-theme'

const THEME_STORAGE_KEY = 'gateway.theme'

function stubSystemTheme(dark: boolean) {
  vi.stubGlobal(
    'matchMedia',
    (query: string) =>
      ({
        matches: dark && query.includes('prefers-color-scheme: dark'),
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
      }) as unknown as MediaQueryList,
  )
}

describe('ThemeToggle', () => {
  beforeEach(() => {
    stubSystemTheme(false)
    setThemePreference('system')
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('offers system, light and dark, with the current preference pressed', () => {
    render(<ThemeToggle />)

    expect(screen.getByRole('group', { name: 'Theme' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'System' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.getByRole('button', { name: 'Light' })).toHaveAttribute(
      'aria-pressed',
      'false',
    )
    expect(screen.getByRole('button', { name: 'Dark' })).toHaveAttribute(
      'aria-pressed',
      'false',
    )
  })

  it('applies and remembers the chosen theme', async () => {
    const user = userEvent.setup()
    render(<ThemeToggle />)

    await user.click(screen.getByRole('button', { name: 'Dark' }))
    expect(document.documentElement).toHaveClass('dark')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('dark')
    expect(screen.getByRole('button', { name: 'Dark' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )

    await user.click(screen.getByRole('button', { name: 'Light' }))
    expect(document.documentElement).not.toHaveClass('dark')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
  })

  it('follows the operating system when set to system', async () => {
    const user = userEvent.setup()
    render(<ThemeToggle />)
    await user.click(screen.getByRole('button', { name: 'Light' }))

    stubSystemTheme(true)
    await user.click(screen.getByRole('button', { name: 'System' }))

    expect(document.documentElement).toHaveClass('dark')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('system')
  })

  it('keeps every toggle on the page in step', async () => {
    const user = userEvent.setup()
    render(
      <>
        <div data-testid="first">
          <ThemeToggle />
        </div>
        <div data-testid="second">
          <ThemeToggle />
        </div>
      </>,
    )

    const [firstDark, secondDark] = screen.getAllByRole('button', { name: 'Dark' })
    await user.click(firstDark!)
    expect(secondDark).toHaveAttribute('aria-pressed', 'true')
  })

  it('reveals the new theme from the clicked button where view transitions exist', async () => {
    const user = userEvent.setup()
    const startViewTransition = vi.fn((update: () => void) => {
      update()
      return { ready: Promise.reject(new Error('skipped')) }
    })
    Object.defineProperty(document, 'startViewTransition', {
      value: startViewTransition,
      configurable: true,
    })

    try {
      render(<ThemeToggle />)
      await user.click(screen.getByRole('button', { name: 'Dark' }))
      expect(startViewTransition).toHaveBeenCalledTimes(1)
      expect(document.documentElement).toHaveClass('dark')

      // Same theme on screen: nothing to reveal.
      await user.click(screen.getByRole('button', { name: 'Dark' }))
      expect(startViewTransition).toHaveBeenCalledTimes(1)
    } finally {
      Reflect.deleteProperty(document, 'startViewTransition')
    }
  })
})
