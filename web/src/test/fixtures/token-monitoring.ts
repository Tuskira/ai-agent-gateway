import raw from './token-monitoring-api.json?raw'
import type { TokenMonitoring } from '@/lib/token-monitoring'

/**
 * `token-monitoring-api.json` holds the REAL handlers' answers
 * (`internal/api/handlers/analytics_monitoring_contract_test.go` replays
 * them and regenerates the file with `UPDATE_CONSOLE_FIXTURES=1`), so a
 * field the API renames or drops breaks these tests.
 */
interface Exchange<Res> {
  status: number
  response: Res
}

export interface MonitoringContract {
  overview_7d: Exchange<TokenMonitoring>
  overview_24h: Exchange<TokenMonitoring>
  model: Exchange<TokenMonitoring>
  key: Exchange<TokenMonitoring>
}

export const monitoring = JSON.parse(raw) as MonitoringContract
