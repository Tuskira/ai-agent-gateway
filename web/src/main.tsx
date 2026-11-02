import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
// Self-hosted Inter/JetBrains Mono live in fonts.css so library consumers
// can opt out of them (see vite.lib.config.ts).
import './fonts.css'
import './index.css'
import App from './App.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
