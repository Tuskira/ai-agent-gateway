import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
// Self-hosted Inter/JetBrains Mono (see src/styles/tokens.css --font-ui /
// --font-mono / --font-display), replacing the fonts.googleapis.com /
// fonts.gstatic.com <link> tags index.html used to carry -- the console
// no longer makes any request to a Google host. Only the weights
// index.html used to load: Inter 400/500/600/700, JetBrains Mono
// 400/500/600 (no bold mono anywhere in src). Each package's own
// OFL-1.1-licensed font files and LICENSE are picked up by
// rollup-plugin-license into dist/licenses.txt (see vite.config.ts),
// served by the running gateway at /licenses.txt.
import '@fontsource/inter/400.css'
import '@fontsource/inter/500.css'
import '@fontsource/inter/600.css'
import '@fontsource/inter/700.css'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import '@fontsource/jetbrains-mono/600.css'
import './index.css'
import App from './App.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
