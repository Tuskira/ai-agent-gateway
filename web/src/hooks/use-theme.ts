import { useSyncExternalStore } from 'react'

export type Theme = 'light' | 'dark'
/** What the person picked: a fixed theme, or "follow the operating system". */
export type ThemePreference = Theme | 'system'

/** Viewport position, in px, that the theme reveal circles out from. */
export interface RevealOrigin {
  x: number
  y: number
}

interface ThemeState {
  preference: ThemePreference
  /** The theme actually on screen — `preference` with "system" resolved. */
  theme: Theme
}

// public/theme-init.js reads the same key before React mounts, and treats
// any value other than "light"/"dark" as "follow the system".
const THEME_STORAGE_KEY = 'gateway.theme'
const DARK_QUERY = '(prefers-color-scheme: dark)'
const REDUCED_MOTION_QUERY = '(prefers-reduced-motion: reduce)'
const REVEAL_MS = 750

function readPreference(): ThemePreference {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY)
    if (stored === 'light' || stored === 'dark' || stored === 'system') return stored
  } catch {
    // ignore storage errors, fall through to the system preference
  }
  return 'system'
}

function resolve(preference: ThemePreference): Theme {
  if (preference !== 'system') return preference
  return window.matchMedia(DARK_QUERY).matches ? 'dark' : 'light'
}

const listeners = new Set<() => void>()
let state: ThemeState = { preference: 'system', theme: 'light' }

function commit(preference: ThemePreference) {
  state = { preference, theme: resolve(preference) }
  document.documentElement.classList.toggle('dark', state.theme === 'dark')
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, preference)
  } catch {
    // ignore storage errors — the theme still applies for this session
  }
  listeners.forEach((listener) => listener())
}

if (typeof window !== 'undefined') {
  const preference = readPreference()
  state = { preference, theme: resolve(preference) }
  document.documentElement.classList.toggle('dark', state.theme === 'dark')
  // Follow the operating system while the preference says to.
  window.matchMedia(DARK_QUERY).addEventListener?.('change', () => {
    if (state.preference === 'system') commit('system')
  })
}

/**
 * Switches theme. Given an `origin`, the new theme wipes in as a circle
 * growing from that point (the View Transitions API) instead of every
 * color snapping at once. The switch is instant where the API is missing,
 * under reduced motion, or when the theme on screen doesn't change.
 */
export function setThemePreference(preference: ThemePreference, origin?: RevealOrigin) {
  const animated =
    origin !== undefined &&
    typeof document.startViewTransition === 'function' &&
    !window.matchMedia(REDUCED_MOTION_QUERY).matches &&
    resolve(preference) !== state.theme

  if (!animated) {
    commit(preference)
    return
  }

  const { x, y } = origin
  // Reach the corner farthest from the origin so the circle covers the page.
  const radius = Math.hypot(
    Math.max(x, window.innerWidth - x),
    Math.max(y, window.innerHeight - y),
  )
  // Radius steps follow √(time), so the revealed *area* grows at a constant
  // rate; a linear radius reveals area ∝ r² and visibly accelerates.
  const clipPath = [0, 0.5, 0.707, 0.866, 1].map(
    (step) => `circle(${radius * step}px at ${x}px ${y}px)`,
  )

  const transition = document.startViewTransition(() => commit(preference))
  transition.ready
    .then(() => {
      document.documentElement.animate(
        { clipPath },
        {
          duration: REVEAL_MS,
          easing: 'linear',
          pseudoElement: '::view-transition-new(root)',
        },
      )
    })
    // The browser drops a transition it can't snapshot cleanly. The theme
    // has been applied by then; only the reveal is lost.
    .catch(() => {})
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

const getSnapshot = () => state
const getServerSnapshot = (): ThemeState => ({ preference: 'system', theme: 'light' })

/**
 * Light/dark theme via the `class` strategy on `<html>`, persisted to
 * localStorage and shared by every component that reads it.
 */
export function useTheme(): ThemeState & { setTheme: typeof setThemePreference } {
  const current = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)
  return { ...current, setTheme: setThemePreference }
}
