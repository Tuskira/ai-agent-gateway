import { createBrowserRouter } from 'react-router-dom'
import { gatewayRoutes } from '@/routes/gateway-routes'

export const router = createBrowserRouter(gatewayRoutes())
