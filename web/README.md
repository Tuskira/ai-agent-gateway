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
