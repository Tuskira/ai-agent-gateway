# @tuskira/gateway-web

Admin console for the Tuskira AI Agent Gateway. React 19 + Vite + TypeScript (strict) +
Tailwind CSS 4 + shadcn/ui.

## Develop

Requires Node 26+ and npm 11+ (no pnpm).

```sh
npm install
npm run dev
```

The dev server proxies `/api` to `http://localhost:8081` (see `vite.config.ts`), so
run the gateway on :8081 alongside the UI. Sign in at `/login` with an API key
(`gk_…`) issued by the gateway.

From the repo root you can instead run:

```sh
make ui-install
make ui-dev
```

## Build

```sh
npm run build
```

Emits a production bundle to `dist/`. In production the gateway itself serves this
bundle at `/` on :8081, so the app talks to the API same-origin — no proxy needed.

```sh
make ui-build   # from the repo root
```

## Using the console as a library

The console is also packaged as `@tuskira/gateway-web`, so you can mount its pages
inside your own React app. The package is not on the npm registry. Each GitHub release
carries it as a tarball, and its version always matches the gateway release tag. React,
React DOM, React Router (`react-router-dom` 7) and TanStack Query 5 are peer dependencies;
everything else installs with the package.

```sh
npm install https://github.com/Tuskira/ai-agent-gateway/releases/download/v<version>/tuskira-gateway-web-<version>.tgz \
  react react-dom react-router-dom @tanstack/react-query
```

npm records the tarball's integrity hash in `package-lock.json`.

```tsx
import {
  AppShell, AuthProvider, RequireAuth, LoginPage, Toaster, TooltipProvider,
  configureGateway, gatewayPageRoutes,
} from '@tuskira/gateway-web'
import '@tuskira/gateway-web/style.css' // Tailwind output + design tokens
import '@tuskira/gateway-web/fonts.css' // optional: self-hosted Inter / JetBrains Mono

configureGateway({
  apiBase: '/gw/api/v1',                  // default '/api/v1'
  mcpBase: 'https://gw.example.com/mcp',  // default: guessed from the page origin
  onUnauthenticated: () => router.navigate('/login'), // default: window.location.assign('/login')
})

const router = createBrowserRouter(
  [
    { path: '/login', element: <LoginPage /> },
    { element: <RequireAuth />, children: [{ element: <AppShell />, children: gatewayPageRoutes() }] },
  ],
  { basename: '/gateway' },
)
```

Wrap the router in `QueryClientProvider`, `AuthProvider` and `TooltipProvider`, and render
`<Toaster />` once, as `src/App.tsx` does. `gatewayRoutes()` returns the complete tree
(login, docs, change-password, shell and the catch-all redirect) if you want the console
to own the whole router. Only what `@tuskira/gateway-web` exports is supported; do not
import from `src/`.

### Adding your own pages

Pass an `extension` to put your own sidebar groups and pages inside the console's shell and
sign-in guard:

```tsx
gatewayRoutes({ extension: { navGroups: [...], routes: [...] } })
```

```tsx
import { Plug } from 'lucide-react'
import { AppShell, gatewayPageRoutes, type ConsoleExtension } from '@tuskira/gateway-web'

const extension: ConsoleExtension = {
  navGroups: [
    // New group, placed before the core "Observe" group (appended without `before`).
    { label: 'Billing', before: 'Observe', items: [
      { label: 'Invoices', path: '/invoices', icon: Plug, description: 'Invoices and usage.' },
    ] },
    // Same label as a core group: the items are added to that group.
    { label: 'Access', items: [
      { label: 'Audit', path: '/audit', icon: Plug, description: 'Audit trail.' },
    ] },
  ],
  routes: [
    { path: '/invoices', Component: InvoicesPage },
    { path: '/audit', Component: AuditPage },
  ],
}

// Own router: pass the extension to both the routes and the shell.
{ element: <AppShell extension={extension} />, children: gatewayPageRoutes(extension) }
```

`gatewayRoutes({ extension })` does both for you. A nav item whose path has no route shows the
"coming soon" placeholder. Set `end: true` on an item whose path is a prefix of other items'
paths so it is active only on its own path.

The browser must reach the gateway API on the path you set as `apiBase`. When the host app
serves the console, add a reverse-proxy rule that forwards `/api/v1/*` (or your `apiBase`
path, rewritten back to `/api/v1`) to the gateway on port 8081. Keep the path, all request
and response headers (`Authorization`, `X-CSRF-Token`, `X-Gateway-Acting-Tenant`) and
cookies unchanged, or sign-in and CSRF checks will fail.

Build the package with `npm run build:lib`, which writes `dist-lib/` (`index.js`, `types/`,
`style.css`, `fonts.css`). `npm pack` then produces the tarball; the release workflow attaches it to the GitHub release on every `v*` tag.

## Quality checks

```sh
npm run lint        # eslint
npm run typecheck    # tsc --noEmit (project references)
npm run test         # vitest
npm run format:check # prettier --check
```

## Adding a shadcn/ui component

```sh
npx shadcn@latest add <component>
```

This project's `components.json` targets the Vite template, the `radix` base, the
`neutral` base color, and CSS variables for theming, with the `@/*` import alias
resolving to `src/*`. New components are written to `src/components/ui/`.

## Design tokens

`tokens.json` (repo root) is the single source of truth for the design system —
colors, spacing, radii, shadows, fonts. Run `npm run gen:tokens` after editing it
to regenerate:

- `src/styles/tokens.css` — the raw tokens as CSS custom properties (`:root` for
  light values, `.dark` for dark overrides), keeping the designer's own token
  names (`--indigo-9`, `--sev-high`, `--r-4`, `--shadow-1`, ...).
- `src/styles/tokens.ts` — the same categorical color scales (`colors.indigo`,
  `colors.slate`, `colors.sev`, `colors.status`, `colors.brand`) plus `fonts`,
  `radii`, `shadows`, for non-CSS consumers (e.g. recharts series colors).

`src/index.css` maps shadcn's semantic variables (`--background`, `--primary`,
`--sidebar`, ...) onto these tokens, and registers Tailwind v4 `@theme` entries so
utilities like `bg-indigo-9`, `text-text-muted`, `border-border-strong`, `shadow-1`,
`rounded-r-4`, and `font-mono` are available anywhere in the app. Do not hand-edit
`src/styles/tokens.css` / `src/styles/tokens.ts` — they're overwritten by the
generator. `src/test/tokens.test.ts` asserts every `tokens.ts` entry has a matching
declaration in `tokens.css`.

## Folder layout

```
web/
├── index.html
├── vite.config.ts        # React + Tailwind plugins, /api dev proxy, vitest config
├── components.json        # shadcn/ui config
├── tokens.json             # design-token source of truth (see Design tokens)
├── scripts/
│   └── gen-tokens.mjs        # tokens.json -> src/styles/tokens.{css,ts}
├── src/
│   ├── main.tsx            # ReactDOM root
│   ├── App.tsx              # QueryClientProvider, AuthProvider, RouterProvider
│   ├── index.css            # Tailwind entrypoint + semantic token mapping
│   ├── styles/
│   │   ├── tokens.css         # generated — raw design tokens as CSS vars
│   │   └── tokens.ts           # generated — same tokens for non-CSS use
│   ├── auth/
│   │   ├── AuthContext.tsx    # AuthProvider / useAuth — principal, signIn, signOut
│   │   └── RequireAuth.tsx    # route guard for the authenticated shell
│   ├── components/
│   │   ├── ui/                 # shadcn/ui primitives (generated, don't hand-edit)
│   │   ├── layout/              # AppShell, Sidebar, TopBar
│   │   └── app/                  # ComingSoon, StatCard
│   ├── hooks/
│   │   └── use-theme.ts          # light/dark toggle, class strategy, localStorage
│   ├── lib/
│   │   ├── api.ts                 # fetch wrapper: bearer auth, 401 handling, ApiError
│   │   ├── nav.ts                  # single source of truth for sidebar nav + routes
│   │   ├── queries.ts                # react-query hooks (useHealth)
│   │   ├── query-client.ts            # QueryClient instance
│   │   ├── types.ts                    # Principal, HealthResponse, ApiErrorBody
│   │   └── utils.ts                     # cn() (shadcn)
│   ├── routes/
│   │   ├── router.tsx                    # createBrowserRouter (data router)
│   │   ├── LoginPage.tsx
│   │   ├── OverviewPage.tsx
│   │   └── DocsPage.tsx
│   └── test/
│       ├── setup.ts
│       ├── LoginPage.test.tsx
│       ├── Sidebar.test.tsx
│       ├── SignOut.test.tsx
│       └── tokens.test.ts
```
