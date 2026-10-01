import { useEffect, useState } from 'react'

/** Returns `value`, but delayed until `delayMs` has passed since it last
 * changed. Used to debounce filter-bar text input before it's committed to
 * the URL / query key, so the analytics endpoint isn't hit on every
 * keystroke. */
export function useDebouncedValue<T>(value: T, delayMs = 400): T {
  const [debounced, setDebounced] = useState(value)

  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value), delayMs)
    return () => window.clearTimeout(id)
  }, [value, delayMs])

  return debounced
}
