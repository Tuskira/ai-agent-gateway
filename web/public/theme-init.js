// Applied before React mounts so the page never flashes the wrong theme.
//
// Served from the same origin (Vite copies public/ verbatim into the
// build's outDir root, so this lands at /theme-init.js) instead of living
// inline in index.html, so the console's CSP script-src can be a plain
// 'self' with no per-script hash to keep in sync whenever this file
// changes -- see internal/api/security_headers.go.
;(function () {
  try {
    var stored = window.localStorage.getItem('gateway.theme')
    var theme =
      stored === 'light' || stored === 'dark'
        ? stored
        : window.matchMedia('(prefers-color-scheme: dark)').matches
          ? 'dark'
          : 'light'
    if (theme === 'dark') document.documentElement.classList.add('dark')
  } catch (e) {
    // ignore — falls back to light theme
  }
})()
