import type { RouteObject } from 'react-router-dom'
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
})
