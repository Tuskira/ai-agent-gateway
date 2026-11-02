import type { RouteObject } from 'react-router-dom'
import { Plug } from 'lucide-react'
import { gatewayPageRoutes, gatewayRoutes } from '@/routes/gateway-routes'

function collectPaths(routes: RouteObject[]): string[] {
  return routes.flatMap((route) => [
    ...(route.path ? [route.path] : route.index ? ['(index)'] : []),
    ...collectPaths(route.children ?? []),
  ])
}

describe('gatewayRoutes', () => {
  it('keeps the console path set stable', () => {
    expect(collectPaths(gatewayRoutes())).toMatchInlineSnapshot(`
      [
        "/login",
        "/docs",
        "/change-password",
        "(index)",
        "/token-monitoring",
        "/connectors",
        "/models",
        "/model-catalog",
        "/profiles",
        "/skills",
        "/users",
        "/access-logs",
        "/llm-logs",
        "/session-timeline",
        "/tool-search",
        "/cache",
        "/api-keys",
        "/credentials",
        "/tenants",
        "/profiles/:id/tools",
        "/token-monitoring/model",
        "/token-monitoring/keys/:id",
        "*",
      ]
    `)
  })

  it('has the login, docs and catch-all routes at the top level', () => {
    const top = gatewayRoutes().map((r) => r.path)
    expect(top).toEqual(['/login', '/docs', undefined, '*'])
  })

  it('page routes carry no login route and sit under the shell in the full tree', () => {
    const pages = collectPaths(gatewayPageRoutes())
    expect(pages).not.toContain('/login')
    expect(pages).not.toContain('/docs')
    expect(pages).toContain('(index)')
    expect(pages).toContain('/profiles/:id/tools')
  })

  describe('with an extension', () => {
    const Page = () => null
    const extension = {
      navGroups: [
        {
          label: 'Mine',
          items: [{ label: 'Mine', path: '/mine', icon: Plug, description: 'x' }],
        },
        {
          label: 'Observe',
          items: [
            { label: 'Soon', path: '/soon', icon: Plug, description: 'placeholder' },
          ],
        },
      ],
      routes: [{ path: '/mine', Component: Page }],
    }

    it('puts the extension routes under the shell, inside the auth guard', () => {
      const tree = gatewayRoutes({ extension })
      const guard = tree.find((r) => !r.path && !!r.children)!
      const shell = guard.children!.find((r) => !r.path && !!r.children)!
      const paths = collectPaths(shell.children!)
      expect(paths).toContain('/mine')
      expect(paths).toContain('/soon') // nav item without a route stays a placeholder
      expect(
        collectPaths(gatewayRoutes({ extension })).filter((p) => p === '/mine'),
      ).toHaveLength(1)
    })

    it('adds only the extension paths to the console path set', () => {
      const base = collectPaths(gatewayRoutes())
      const withExt = collectPaths(gatewayRoutes({ extension }))
      expect(withExt.filter((p) => !base.includes(p)).sort()).toEqual(['/mine', '/soon'])
      expect(collectPaths(gatewayRoutes({}))).toEqual(base)
      expect(collectPaths(gatewayPageRoutes(extension))).toContain('/mine')
    })
  })
})
