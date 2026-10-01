import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, render } from '@testing-library/react'
import { NumberTicker } from '@/components/app/NumberTicker'
import { formatCompactNumber, formatCurrency } from '@/lib/overview'

type ObserverCallback = (entries: { isIntersecting: boolean }[]) => void

/** Installs an IntersectionObserver that reports nothing until the test
 * calls `enterView()`. */
function stubIntersectionObserver() {
  const callbacks: ObserverCallback[] = []
  vi.stubGlobal(
    'IntersectionObserver',
    class {
      constructor(callback: ObserverCallback) {
        callbacks.push(callback)
      }
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  )
  return {
    enterView: () => callbacks.forEach((cb) => cb([{ isIntersecting: true }])),
  }
}

function stubReducedMotion(reduce: boolean) {
  vi.stubGlobal(
    'matchMedia',
    (query: string) =>
      ({
        matches: reduce && query.includes('prefers-reduced-motion'),
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
      }) as unknown as MediaQueryList,
  )
}

describe('NumberTicker', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('writes the final formatted value at once when it cannot observe the viewport', () => {
    const { container } = render(
      <NumberTicker value={515490} format={formatCompactNumber} />,
    )
    expect(container).toHaveTextContent('515.49K')
  })

  it('writes the final formatted value at once under reduced motion', () => {
    stubIntersectionObserver()
    stubReducedMotion(true)
    const { container } = render(<NumberTicker value={11} format={formatCurrency} />)
    expect(container).toHaveTextContent('$11.00')
  })

  it('starts from zero and counts up to the value once it scrolls into view', async () => {
    const observer = stubIntersectionObserver()
    stubReducedMotion(false)
    const { container } = render(
      <NumberTicker value={602} format={formatCompactNumber} />,
    )
    expect(container).toHaveTextContent('0')

    act(() => observer.enterView())
    await vi.waitFor(() => expect(container).toHaveTextContent('602'), {
      timeout: 3000,
    })
  })

  it('follows a changed value', () => {
    const { container, rerender } = render(
      <NumberTicker value={10} format={formatCompactNumber} />,
    )
    rerender(<NumberTicker value={25} format={formatCompactNumber} />)
    expect(container).toHaveTextContent('25')
  })
})
